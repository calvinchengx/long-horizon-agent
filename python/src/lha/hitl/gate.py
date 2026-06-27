"""Gate policies.

- ``AutoPolicyGate``: for unattended/dev runs — auto-approves *reversible* actions (they carry a
  saga compensation) and applies the request's default for *irreversible* ones (never silently
  takes an irreversible action). The durable, human-answerable gate is the workflow's signal-based
  version; this is the policy fallback / timeout behavior.
- ``CallbackGate``: delegates to an async resolver (e.g. a Slack/console prompt), with the policy
  default applied if the resolver returns ``None``, raises, or answers with a decision that was
  not one of the request's ``options`` (fail closed).
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from lha.contracts.hitl import GateDecision, GateRequest, GateResolution, RiskTier


class AutoPolicyGate:
    """Resolves gates by policy alone (no human)."""

    def __init__(self, *, auto_approve_reversible: bool = True) -> None:
        self._auto_approve_reversible = auto_approve_reversible

    async def request(self, req: GateRequest) -> GateResolution:
        if req.risk is RiskTier.REVERSIBLE and self._auto_approve_reversible:
            return GateResolution(
                gate_id=req.gate_id, decision=GateDecision.APPROVE, resolved_by="auto-policy"
            )
        return GateResolution(
            gate_id=req.gate_id,
            decision=req.default_action,
            resolved_by="default",
            defaulted=True,
        )


class CallbackGate:
    """Asks an async resolver; falls back to the request's default if it returns ``None``."""

    def __init__(self, resolver: Callable[[GateRequest], Awaitable[GateDecision | None]]) -> None:
        self._resolver = resolver

    async def request(self, req: GateRequest) -> GateResolution:
        try:
            decision = await self._resolver(req)
        except Exception:
            decision = None
        if decision is None or decision not in req.options:
            return GateResolution(
                gate_id=req.gate_id,
                decision=req.default_action,
                resolved_by="default",
                defaulted=True,
            )
        return GateResolution(gate_id=req.gate_id, decision=decision, resolved_by="human")
