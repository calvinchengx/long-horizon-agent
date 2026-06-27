"""Durable approval of irreversible actions (Temporal time-skipping server, scripted cycles).

A cycle that attempts a gated command reports it as a pending approval; the workflow parks as
WAITING_ON_HUMAN with the question queryable; a human decision is delivered by signal; an approved
action is handed to the next cycle (by fingerprint) exactly once, and a rejected one is never asked
about again.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from temporalio import activity
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.durable.signals import QUERY_STATUS, SIGNAL_HUMAN_DECISION, STATUS_WAITING_ON_HUMAN
from lha.durable.types import CycleInput, CycleResult, MissionInput, PendingApproval
from lha.durable.workflows import MissionWorkflow
from tests.durability.test_durable_spine import _healthy, _snapshot_activity, _unblock_activity

PUSH = PendingApproval(
    fingerprint="fp-push", tool="run_command", reason="git push", arguments="['git', 'push']"
)


def _result(*, complete: bool, pending: list[PendingApproval], used: list[str]) -> CycleResult:
    return CycleResult(
        item_id="01",
        advanced=True,
        head_sha="abc",
        is_complete=complete,
        items_done=1 if complete else 0,
        items_total=1,
        pending_approvals=pending,
        used_approvals=used,
    )


def _scripted_cycles(seen: list[CycleInput], approve_path: bool):  # type: ignore[no-untyped-def]
    @activity.defn(name="run_agent_cycle")
    async def cycle(inp: CycleInput) -> CycleResult:
        seen.append(inp)
        approved = [a.fingerprint for a in inp.approved_actions]
        if len(seen) == 1:
            return _result(complete=False, pending=[PUSH], used=[])
        if approve_path:
            return _result(complete=True, pending=[], used=approved)
        if len(seen) == 2:  # asks again after a rejection: must not reopen the gate
            return _result(complete=False, pending=[PUSH], used=[])
        return _result(complete=True, pending=[], used=[])

    return cycle


async def _run(tmp_path: Path, *, decision: str) -> tuple[list[CycleInput], str, str]:
    seen: list[CycleInput] = []
    inp = MissionInput(mission_id="m1", workdir=str(tmp_path), check_commands=[["true"]])
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue="lha-approvals",
            workflows=[MissionWorkflow],
            activities=[
                _scripted_cycles(seen, decision == "approve"),
                _healthy,
                _unblock_activity,
                _snapshot_activity,
            ],
        ),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id="mission:m1", task_queue="lha-approvals"
        )
        status = question = ""
        for _ in range(100):
            status = await handle.query(QUERY_STATUS)
            if status == STATUS_WAITING_ON_HUMAN:
                question = await handle.query("open_question")
                break
            await asyncio.sleep(0.05)
        await handle.signal(SIGNAL_HUMAN_DECISION, decision)
        result = await asyncio.wait_for(handle.result(), 60)
        assert result.completed
    return seen, status, question


@pytest.mark.asyncio
async def test_approved_action_reaches_the_next_cycle_once(tmp_path: Path) -> None:
    seen, status, question = await _run(tmp_path, decision="approve")
    assert status == STATUS_WAITING_ON_HUMAN
    assert "irreversible action" in question and "git push" in question
    assert [a.fingerprint for a in seen[0].approved_actions] == []
    assert [a.fingerprint for a in seen[1].approved_actions] == ["fp-push"]
    assert len(seen) == 2


@pytest.mark.asyncio
async def test_rejected_action_is_not_asked_again(tmp_path: Path) -> None:
    seen, _status, _question = await _run(tmp_path, decision="reject")
    # Cycle 2 re-requested the same push; the workflow did not reopen a gate (or it would still be
    # waiting for a second decision), and nothing was ever approved.
    assert len(seen) == 3
    assert all(not inp.approved_actions for inp in seen)
