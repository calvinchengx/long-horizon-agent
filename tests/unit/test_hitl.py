"""Tests for the HITL gate policies."""

from __future__ import annotations

import pytest

from lha.contracts.hitl import GateDecision, GateRequest, RiskTier
from lha.hitl.gate import AutoPolicyGate, CallbackGate


@pytest.mark.asyncio
async def test_auto_policy_approves_reversible() -> None:
    gate = AutoPolicyGate()
    result = await gate.request(
        GateRequest(gate_id="g1", question="merge a feature branch?", risk=RiskTier.REVERSIBLE)
    )
    assert result.decision is GateDecision.APPROVE
    assert not result.defaulted


@pytest.mark.asyncio
async def test_auto_policy_defaults_irreversible() -> None:
    gate = AutoPolicyGate()
    result = await gate.request(
        GateRequest(
            gate_id="g2",
            question="deploy to production?",
            risk=RiskTier.IRREVERSIBLE,
            default_action=GateDecision.ABORT,
        )
    )
    assert result.decision is GateDecision.ABORT
    assert result.defaulted  # no human → safe default applied (never silently proceeds)


@pytest.mark.asyncio
async def test_callback_gate_uses_human_decision() -> None:
    async def resolver(_: GateRequest) -> GateDecision:
        return GateDecision.APPROVE

    result = await CallbackGate(resolver).request(GateRequest(gate_id="g3", question="ok?"))
    assert result.decision is GateDecision.APPROVE
    assert result.resolved_by == "human"


@pytest.mark.asyncio
async def test_callback_gate_falls_back_to_default() -> None:
    async def resolver(_: GateRequest) -> GateDecision | None:
        return None

    result = await CallbackGate(resolver).request(
        GateRequest(gate_id="g4", question="ok?", default_action=GateDecision.REJECT)
    )
    assert result.decision is GateDecision.REJECT
    assert result.defaulted
