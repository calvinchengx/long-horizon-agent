"""Human-in-the-loop contract.

Gates pause on high-blast-radius/irreversible actions. The durable mechanism (a Temporal signal
with a timeout) lives in the workflow; this contract defines the request/resolution shape and the
policy a gate applies. Every gate has a timeout + a default action, so an unattended run never
stalls forever invisibly and never takes an irreversible action without a decision.
"""

from __future__ import annotations

from enum import Enum
from typing import Protocol, runtime_checkable

from pydantic import BaseModel, Field


class GateDecision(str, Enum):
    APPROVE = "approve"
    REJECT = "reject"
    ABORT = "abort"


class RiskTier(str, Enum):
    REVERSIBLE = "reversible"  # has a saga compensation; can auto-proceed
    IRREVERSIBLE = "irreversible"  # merge-to-protected-main, deploy, external comms


class GateRequest(BaseModel):
    """A request for a human decision."""

    gate_id: str
    question: str
    risk: RiskTier = RiskTier.IRREVERSIBLE
    default_action: GateDecision = GateDecision.ABORT  # applied on timeout
    options: list[GateDecision] = Field(
        default_factory=lambda: [GateDecision.APPROVE, GateDecision.REJECT]
    )
    context: dict[str, str] = Field(default_factory=dict)


class GateResolution(BaseModel):
    """The outcome of a gate."""

    gate_id: str
    decision: GateDecision
    resolved_by: str = ""
    defaulted: bool = False  # True if the timeout default was applied (no human answered)


@runtime_checkable
class HITLGate(Protocol):
    """Requests a decision and returns a resolution. Impl: ``src/lha/hitl/``."""

    async def request(self, req: GateRequest) -> GateResolution: ...
