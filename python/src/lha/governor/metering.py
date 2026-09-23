"""Metered model provider: every ``complete()`` is budget-checked BEFORE it runs and recorded AFTER.

Wrap every provider a mission uses (lead, researchers, reviewer, reflection, planner, ...) with the
SAME ``CostMeter`` so one ledger sees all spend and one governor enforces one ceiling::

    meter = CostMeter(ledger=CostLedger(), governor=BudgetGovernor(ceiling_usd=10, max_cycles=100))
    lead = meter.wrap(model_for_role("lead"), role="lead")
    meter.cycle_id = "c1"   # attribute subsequent calls to a cycle

Before each call the governor's hard stop (``authorize_call``) is consulted with this call's
WORST-CASE cost (conservative input-token estimate + the full ``max_tokens`` of output) plus the
worst case of calls already in flight (so a parallel fan-out cannot collectively overshoot). A
refusal raises ``BudgetExceeded``. After the call the ACTUAL cost is computed by the provider from
the reported usage and recorded; an unpriced model is recorded with ``cost_known=False``.

Persistence hook: set ``meter.on_record`` to an async callable (e.g.
``lha.persistence.tracking.LedgerSink``) and every recorded entry is also handed to it, so EVERY
metered call reaches the persistent ``cost_ledger``. A failing hook is logged, never raised: the
model call already happened and its spend is already in the in-memory ledger.
"""

from __future__ import annotations

import json
import math
from collections.abc import Awaitable, Callable

import structlog

from lha.contracts.model import (
    ModelMessage,
    ModelProvider,
    TurnResult,
    UnknownPriceError,
    Usage,
)
from lha.governor.cost import CostEntry, CostLedger
from lha.governor.governor import BudgetGovernor, GovernorDecision

# Conservative chars-per-token for the pre-call input estimate (real text averages ~4; code and
# non-English text run lower). Over-estimating only makes the hard stop trip slightly early.
_CHARS_PER_TOKEN = 2.0
_PER_MESSAGE_OVERHEAD_TOKENS = 8
# Output ceiling assumed when neither the call nor the provider states a max_tokens.
DEFAULT_ASSUMED_MAX_OUTPUT_TOKENS = 8192


class BudgetExceeded(RuntimeError):
    """A model call was refused before it ran because it could breach the budget."""

    def __init__(self, decision: GovernorDecision) -> None:
        super().__init__(
            f"budget governor refused model call: {decision.reason} "
            f"(spent ${decision.spent_usd:.4f}, projected ${decision.projected_usd:.4f}, "
            f"ceiling ${decision.ceiling_usd:.4f})"
        )
        self.decision = decision


def estimate_input_tokens(
    messages: list[ModelMessage], tools: list[dict[str, object]] | None = None
) -> int:
    """A deliberately conservative (high) input-token estimate for a request."""
    chars = sum(len(m.content) for m in messages)
    chars += sum(len(json.dumps(c.arguments)) + len(c.name) for m in messages for c in m.tool_calls)
    if tools:
        chars += len(json.dumps(tools))
    return math.ceil(chars / _CHARS_PER_TOKEN) + _PER_MESSAGE_OVERHEAD_TOKENS * len(messages)


class CostMeter:
    """Shared ledger + governor + in-flight reservations for every metered provider of a mission."""

    def __init__(
        self,
        *,
        ledger: CostLedger,
        governor: BudgetGovernor,
        assumed_max_output_tokens: int = DEFAULT_ASSUMED_MAX_OUTPUT_TOKENS,
    ) -> None:
        self.ledger = ledger
        self.governor = governor
        self.cycle_id = "c0"
        self.assumed_max_output_tokens = assumed_max_output_tokens
        self._reserved_usd = 0.0
        # Called with every recorded entry (the persistent ledger); see the module docstring.
        self.on_record: Callable[[CostEntry], Awaitable[None]] | None = None

    @property
    def reserved_usd(self) -> float:
        """Worst-case cost of calls currently in flight."""
        return self._reserved_usd

    def wrap(self, provider: ModelProvider, *, role: str = "") -> MeteredModel:
        return MeteredModel(provider, self, role=role)

    def _reserve(self, usd: float) -> None:
        self._reserved_usd += usd

    def _release(self, usd: float) -> None:
        self._reserved_usd = max(0.0, self._reserved_usd - usd)


class MeteredModel(ModelProvider):
    """A ``ModelProvider`` that enforces the budget per call and records every call's spend."""

    def __init__(self, provider: ModelProvider, meter: CostMeter, *, role: str = "") -> None:
        self._provider = provider
        self._meter = meter
        self.role = role
        self.name = provider.name

    @property
    def inner(self) -> ModelProvider:
        return self._provider

    def worst_case_usd(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> float | None:
        """Upper bound on this call's cost, or ``None`` if the provider cannot price it."""
        output_cap = (
            max_tokens
            or getattr(self._provider, "default_max_tokens", None)
            or self._meter.assumed_max_output_tokens
        )
        usage = Usage(
            input_tokens=estimate_input_tokens(messages, tools), output_tokens=int(output_cap)
        )
        try:
            return self._provider.estimate_cost_usd(usage)
        except UnknownPriceError:
            return None

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        meter = self._meter
        worst = self.worst_case_usd(messages, tools=tools, max_tokens=max_tokens)
        decision = meter.governor.authorize_call(
            meter.ledger, worst_case_usd=worst, reserved_usd=meter.reserved_usd
        )
        if not decision.allow:
            raise BudgetExceeded(decision)

        reservation = worst or 0.0
        meter._reserve(reservation)
        try:
            result = await self._provider.complete(messages, tools=tools, max_tokens=max_tokens)
        finally:
            meter._release(reservation)

        try:
            usd: float | None = self._provider.estimate_cost_usd(result.usage)
        except UnknownPriceError:
            usd = None
        entry = meter.ledger.record(
            cycle_id=meter.cycle_id, usage=result.usage, usd=usd, role=self.role
        )
        if meter.on_record is not None:
            try:
                await meter.on_record(entry)
            except Exception as exc:  # persistence must never fail a completed call
                structlog.get_logger("lha.governor").warning(
                    "cost_hook_failed", error=f"{type(exc).__name__}: {exc}"
                )
        return result

    def estimate_cost_usd(self, usage: Usage) -> float:
        return self._provider.estimate_cost_usd(usage)

    async def aclose(self) -> None:
        close = getattr(self._provider, "aclose", None)
        if close is not None:
            await close()
