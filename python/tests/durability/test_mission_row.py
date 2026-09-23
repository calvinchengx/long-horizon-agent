"""The ``missions`` row tracks the statuses the WORKFLOW owns (``record_mission_status``).

The cycle activity only sees what a cycle observes; SLEEPING, an open deadlock gate and the
final outcome the workflow decides (a gate's retry / abort / impossible, a cancellation) reach
the row through the workflow. On the time-skipping server, with the same SQLite store the cycle
activity writes, these prove the row reads:

* SLEEPING while the mission sleeps between cycles, then DONE;
* WAITING_ON_HUMAN while the deadlock gate is open (the cycle wrote IMPOSSIBLE just before),
  then DONE after "retry", ABORTED after "abort", IMPOSSIBLE after "impossible";
* ABORTED after a cancellation (``lha mission-abort``), whether it sleeps or waits on a gate;
* and a row write that keeps failing never fails or blocks the mission.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

import pytest
from temporalio import activity
from temporalio.client import WorkflowFailureError, WorkflowHandle
from temporalio.exceptions import is_cancelled_exception
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.durable.activities import _record_mission_status, make_notify_activity
from lha.durable.signals import (
    SIGNAL_HUMAN_DECISION,
    SIGNAL_SNOOZE,
    STATUS_ABORTED,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_RUNNING,
    STATUS_SLEEPING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import (
    OUTCOME_ABORTED,
    OUTCOME_COMPLETED,
    OUTCOME_IMPOSSIBLE,
    CycleInput,
    CycleResult,
    MissionResult,
    MissionStatusInput,
)
from lha.durable.workflows import MissionWorkflow
from lha.persistence.store import MissionRow, open_store
from tests.durability import test_durable_spine as spine
from tests.durability._support import SETTINGS, init_mission, working_model
from tests.durability.test_human_gates import _never_works, cycle_with, status_is, wait_for, worker

_TIMEOUT_S = 120


async def mission_row(mission_id: str) -> MissionRow | None:
    store = await open_store(SETTINGS)
    try:
        return await store.get_mission(mission_id)
    finally:
        await store.close()


async def row_is(mission_id: str, status: str) -> None:
    async def probe() -> bool:
        row = await mission_row(mission_id)
        return row is not None and row.status == status

    await wait_for(probe, f"missions row {status}")


async def _seed_row(mission_id: str, workdir: str) -> None:
    """What ``lha mission-start`` writes before the workflow starts."""
    await _record_mission_status(
        MissionStatusInput(mission_id=mission_id, workdir=workdir, status=STATUS_RUNNING),
        settings=SETTINGS,
    )


@pytest.mark.asyncio
async def test_row_reads_sleeping_while_sleeping_then_done(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, cycle_pause_seconds=600)
    tq = "lha-row-sleep"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(working_model)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_SLEEPING)
        await row_is(inp.mission_id, STATUS_SLEEPING)
        await handle.signal(SIGNAL_SNOOZE, 0)
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert result.completed
    row = await mission_row(inp.mission_id)
    assert row is not None and row.status == STATUS_DONE
    assert row.head_sha == result.head_sha
    assert row.workflow_id == f"mission:{inp.mission_id}"
    assert row.title == "Test mission"  # the cycle's write; the workflow's keeps it


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("decision", "outcome", "final"),
    [
        ("retry", OUTCOME_COMPLETED, STATUS_DONE),
        ("abort", OUTCOME_ABORTED, STATUS_ABORTED),
        ("impossible", OUTCOME_IMPOSSIBLE, STATUS_IMPOSSIBLE),
    ],
)
async def test_row_waits_on_the_deadlock_gate_then_records_the_outcome(
    tmp_path: Path, decision: str, outcome: str, final: str
) -> None:
    spine._gate["idle_cycles"] = 3  # three failed cycles block item 01; "retry" then works
    inp = await init_mission(tmp_path, n=1, deadlock_gate_seconds=3600)
    tq = f"lha-row-gate-{decision}"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(spine._fails_three_times)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)
        await row_is(inp.mission_id, STATUS_WAITING_ON_HUMAN)
        await handle.signal(SIGNAL_HUMAN_DECISION, decision)
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert result.outcome == outcome and result.status == final
    row = await mission_row(inp.mission_id)
    assert row is not None and row.status == final


async def _cancel_and_expect_aborted(mission_id: str, handle: WorkflowHandle[Any, Any]) -> None:
    await handle.cancel()
    with pytest.raises(WorkflowFailureError) as info:
        await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert is_cancelled_exception(info.value.cause)
    await row_is(mission_id, STATUS_ABORTED)


@pytest.mark.asyncio
async def test_cancelling_a_sleeping_mission_records_aborted(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, cycle_pause_seconds=600)
    tq = "lha-row-cancel-sleep"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(working_model)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_SLEEPING)
        await row_is(inp.mission_id, STATUS_SLEEPING)
        await _cancel_and_expect_aborted(inp.mission_id, handle)


@pytest.mark.asyncio
async def test_cancelling_at_an_open_gate_records_aborted(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=1, deadlock_gate_seconds=3600)
    tq = "lha-row-cancel-gate"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(_never_works)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)
        await row_is(inp.mission_id, STATUS_WAITING_ON_HUMAN)
        await _cancel_and_expect_aborted(inp.mission_id, handle)


_row_calls: list[str] = []


@activity.defn(name="record_mission_status")
async def _broken_row(inp: MissionStatusInput) -> bool:
    _row_calls.append(inp.status)
    raise RuntimeError("store is on fire (injected)")


@pytest.mark.asyncio
async def test_a_failing_row_write_never_fails_the_mission(tmp_path: Path) -> None:
    _row_calls.clear()
    inp = await init_mission(tmp_path, n=2, cycle_pause_seconds=60)
    await _seed_row(inp.mission_id, inp.workdir)
    tq = "lha-row-broken"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue=tq,
            workflows=[MissionWorkflow],
            activities=[
                cycle_with(working_model),  # type: ignore[list-item]
                make_notify_activity(settings=SETTINGS),
                spine._healthy,
                spine._unblock_activity,
                spine._snapshot_activity,
                _broken_row,
            ],
        ),
    ):
        result: MissionResult = await asyncio.wait_for(
            env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            ),
            _TIMEOUT_S,
        )
    assert result.completed and result.status == STATUS_DONE
    # SLEEPING once (between the two cycles) and DONE, each tried 3 times, then given up.
    assert _row_calls == [STATUS_SLEEPING] * 3 + [STATUS_DONE] * 3
    row = await mission_row(inp.mission_id)
    assert row is not None and row.status == STATUS_DONE  # the cycle activity's own write


_cycle_started = asyncio.Event()


@activity.defn(name="run_agent_cycle")
async def _endless_cycle(_inp: CycleInput) -> CycleResult:
    """A cycle that runs until it is cancelled (heartbeating, so the cancel reaches it)."""
    _cycle_started.set()
    while True:
        activity.heartbeat()
        await asyncio.sleep(0.05)


@pytest.mark.asyncio
async def test_cancelling_during_a_cycle_records_aborted_and_never_parks(tmp_path: Path) -> None:
    _cycle_started.clear()
    inp = await init_mission(tmp_path, n=1)
    tq = "lha-row-cancel-cycle"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, _endless_cycle),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await asyncio.wait_for(_cycle_started.wait(), _TIMEOUT_S)
        await _cancel_and_expect_aborted(inp.mission_id, handle)
        parks = await handle.query(MissionWorkflow.park_reason)
    assert parks == ""
