"""Tests for ledgers, the agent-as-judge, and reflection."""

from __future__ import annotations

import pytest

from lha.agents.judge import AgentAsJudge
from lha.agents.reflection import reflect_on_failure
from lha.contracts.model import TurnResult
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus
from lha.durable.ledgers import ProgressLedger, TaskLedger
from lha.model.stub import StubModel


def test_task_ledger_tracks_open_tickets() -> None:
    ledger = TaskLedger()
    ledger.upsert(Ticket(id="t1", contract=TaskContract(objective="a")))
    ledger.upsert(Ticket(id="t2", contract=TaskContract(objective="b"), status=TicketStatus.DONE))
    assert [t.id for t in ledger.open_tickets()] == ["t1"]


def test_progress_ledger_detects_stall() -> None:
    ledger = ProgressLedger(stall_limit=2)
    ledger.record(made_progress=True)
    assert not ledger.stalled
    ledger.record(made_progress=False)
    ledger.record(made_progress=False)
    assert ledger.stalled


@pytest.mark.asyncio
async def test_judge_parses_verdict() -> None:
    model = StubModel(
        script=[TurnResult(text='{"pass": true, "score": 0.9, "rationale": "looks good"}')]
    )
    verdict = await AgentAsJudge(model).judge(candidate="x", rubric="r")
    assert verdict.passed
    assert verdict.score == 0.9
    assert "good" in verdict.rationale


@pytest.mark.asyncio
async def test_judge_defaults_to_fail_on_garbage() -> None:
    model = StubModel(script=[TurnResult(text="not json at all")])
    verdict = await AgentAsJudge(model).judge(candidate="x", rubric="r")
    assert not verdict.passed
    assert verdict.score == 0.0


@pytest.mark.asyncio
async def test_reflection_returns_postmortem() -> None:
    model = StubModel(script=[TurnResult(text="Root cause: missing import. Try adding it.")])
    out = await reflect_on_failure(
        model=model, item_description="add a function", failure_summary="ImportError: x"
    )
    assert "Root cause" in out
