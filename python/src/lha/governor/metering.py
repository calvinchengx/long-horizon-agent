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

Tracing: every metered call is an OpenTelemetry span (``chat <model>``, or ``invoke_agent
<model>`` for ``run_external``) with the role, cycle, token usage and cost (``lha.obs.otel``;
a no-op unless tracing is configured).
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
from lha.obs.otel import span

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

    @property
    def remaining_usd(self) -> float:
        """The ceiling less what has been spent and what calls in flight have reserved."""
        return self.governor.ceiling_usd - self.ledger.total_usd - self._reserved_usd

    def wrap(self, provider: ModelProvider, *, role: str = "") -> MeteredModel:
        return MeteredModel(provider, self, role=role)

    def _reserve(self, usd: float) -> None:
        self._reserved_usd += usd

    def _release(self, usd: float) -> None:
        self._reserved_usd = max(0.0, self._reserved_usd - usd)

    async def run_external[T](
        self,
        run: Callable[[], Awaitable[tuple[T, Usage]]],
        *,
        worst_case_usd: float | None,
        role: str = "",
    ) -> T:
        """Meter work that is not a ``ModelProvider`` call (a System One evaluation).

        Authorized with ``worst_case_usd`` (``None``: the work cannot be priced, which the
        governor refuses unless unknown cost is allowed) and recorded under ``role`` with the
        cost the work reported (``Usage.reported_cost_usd``), else its worst case.
        """
        result, _usage = await self.run_metered(run, worst_case_usd=worst_case_usd, role=role)
        return result

    async def run_metered[T](
        self,
        run: Callable[[], Awaitable[tuple[T, Usage]]],
        *,
        worst_case_usd: float | None,
        role: str = "",
    ) -> tuple[T, Usage]:
        """``run_external``, returning the recorded usage too."""
        decision = self.governor.authorize_call(
            self.ledger, worst_case_usd=worst_case_usd, reserved_usd=self.reserved_usd
        )
        if not decision.allow:
            raise BudgetExceeded(decision)
        reservation = worst_case_usd or 0.0
        self._reserve(reservation)
        try:
            result, usage = await run()
        finally:
            self._release(reservation)
        entry = self.ledger.record(
            cycle_id=self.cycle_id,
            usage=usage,
            usd=worst_case_usd if usage.reported_cost_usd is None else usage.reported_cost_usd,
            role=role,
        )
        if self.on_record is not None:
            try:
                await self.on_record(entry)
            except Exception as exc:  # persistence must never fail completed work
                structlog.get_logger("lha.governor").warning(
                    "cost_hook_failed", error=f"{type(exc).__name__}: {exc}"
                )
        return result, usage


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

    @property
    def meter(self) -> CostMeter:
        """The shared meter this provider records into."""
        return self._meter

    @property
    def remaining_usd(self) -> float:
        """What is left of the mission's budget (``CostMeter.remaining_usd``)."""
        return self._meter.remaining_usd

    def worst_case_usd(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> float | None:
        """Upper bound on this call's cost, or ``None`` if the provider cannot price it."""
        return self._worst_case_usd(self._provider, messages, tools=tools, max_tokens=max_tokens)

    def _worst_case_usd(
        self,
        provider: ModelProvider,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> float | None:
        output_cap = (
            max_tokens
            or getattr(provider, "default_max_tokens", None)
            or self._meter.assumed_max_output_tokens
        )
        usage = Usage(
            input_tokens=estimate_input_tokens(messages, tools), output_tokens=int(output_cap)
        )
        try:
            return provider.estimate_cost_usd(usage)
        except UnknownPriceError:
            return None

    def _for_this_call(self) -> ModelProvider:
        """The provider for one call: one that carries its own spend cap (``claude -p``) is
        capped to what is left of the budget, so its worst case never exceeds it."""
        capped = getattr(self._provider, "budget_capped", None)
        return self._provider if capped is None else capped(self._meter.remaining_usd)

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        with span(f"chat {self.name}", self._span_attributes("chat")) as traced:
            result, entry = await self._complete(messages, tools=tools, max_tokens=max_tokens)
            usage = result.usage
            traced.set(
                {
                    "gen_ai.response.model": usage.model,
                    "gen_ai.usage.input_tokens": usage.input_tokens,
                    "gen_ai.usage.output_tokens": usage.output_tokens,
                    "gen_ai.response.finish_reasons": result.stop_reason,
                    "lha.cost_usd": entry.usd if entry.cost_known else None,
                }
            )
            return result

    def _span_attributes(self, operation: str) -> dict[str, object]:
        return {
            "gen_ai.operation.name": operation,
            "gen_ai.request.model": self.name,
            "lha.role": self.role,
            "lha.cycle_id": self._meter.cycle_id,
        }

    async def _complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> tuple[TurnResult, CostEntry]:
        meter = self._meter
        provider = self._for_this_call()
        worst = self._worst_case_usd(provider, messages, tools=tools, max_tokens=max_tokens)
        decision = meter.governor.authorize_call(
            meter.ledger, worst_case_usd=worst, reserved_usd=meter.reserved_usd
        )
        if not decision.allow:
            raise BudgetExceeded(decision)

        reservation = worst or 0.0
        meter._reserve(reservation)
        try:
            result = await provider.complete(messages, tools=tools, max_tokens=max_tokens)
        finally:
            meter._release(reservation)

        try:
            usd: float | None = provider.estimate_cost_usd(result.usage)
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
        return result, entry

    async def run_external[T](
        self, run: Callable[[], Awaitable[tuple[T, Usage]]], *, worst_case_usd: float
    ) -> T:
        """Meter work that spends outside ``complete()`` (a whole ``claude -p`` session).

        Authorized with ``worst_case_usd`` like any call; afterwards the cost the work itself
        reported (``Usage.reported_cost_usd``) is recorded. Work that reported nothing (a killed
        session whose models have no price) is charged its full ``worst_case_usd``:
        conservative, never $0.
        ``run`` returns its result and the usage to record.
        """
        with span(f"invoke_agent {self.name}", self._span_attributes("invoke_agent")) as traced:
            result, usage = await self._run_external(run, worst_case_usd=worst_case_usd)
            traced.set(
                {
                    "gen_ai.response.model": usage.model,
                    "gen_ai.usage.input_tokens": usage.input_tokens,
                    "gen_ai.usage.output_tokens": usage.output_tokens,
                    "lha.cost_usd": usage.reported_cost_usd,
                }
            )
            return result

    async def _run_external[T](
        self, run: Callable[[], Awaitable[tuple[T, Usage]]], *, worst_case_usd: float
    ) -> tuple[T, Usage]:
        return await self._meter.run_metered(run, worst_case_usd=worst_case_usd, role=self.role)

    def estimate_cost_usd(self, usage: Usage) -> float:
        return self._provider.estimate_cost_usd(usage)

    async def aclose(self) -> None:
        close = getattr(self._provider, "aclose", None)
        if close is not None:
            await close()


async def run_external[T](
    model: ModelProvider | None,
    run: Callable[[], Awaitable[tuple[T, Usage]]],
    *,
    worst_case_usd: float,
) -> T:
    """``MeteredModel.run_external`` when ``model`` is metered; otherwise just ``run``."""
    if isinstance(model, MeteredModel):
        return await model.run_external(run, worst_case_usd=worst_case_usd)
    result, _ = await run()
    return result
