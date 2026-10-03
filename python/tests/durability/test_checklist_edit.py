"""Operator checklist edits on a running durable mission (``checklist_edit_v1``):

1. a batch sent while the deadlock gate is open lands at "retry", before the blocked items are
   reset: removing the item that can never pass and adding one that can completes the mission;
2. a batch sent while the mission SLEEPS is applied at once (the sleep goes on) and a refused
   batch is reported in the gate log without touching the anchor;
3. a batch delivered with the start is applied before the first cycle.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from temporalio.testing import WorkflowEnvironment

from lha.config import Settings
from lha.contracts.model import ModelProvider
from lha.contracts.state import SituationSnapshot
from lha.durable.activities import EDIT_EVENT
from lha.durable.signals import (
    QUERY_GATE_LOG,
    QUERY_PENDING_EDITS,
    SIGNAL_CHECKLIST_EDIT,
    SIGNAL_HUMAN_DECISION,
    SIGNAL_SNOOZE,
    STATUS_SLEEPING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import MissionResult
from lha.durable.workflows import MissionWorkflow
from tests.durability._support import commits_with, idle_model, init_mission, working_model
from tests.durability.test_human_gates import (
    _TIMEOUT_S,
    committed_events,
    cycle_with,
    status_is,
    wait_for,
    worker,
)

pytestmark = pytest.mark.asyncio


def _item2_never_works(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    assert snapshot.active_item is not None
    if snapshot.active_item.id == "02":
        return idle_model()
    return working_model(settings, snapshot)


async def test_edit_at_the_deadlock_gate_resolves_it(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, deadlock_gate_seconds=3600)
    tq = "lha-edit-gate"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(_item2_never_works)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)  # 02 blocked after three attempts
        await handle.signal(
            SIGNAL_CHECKLIST_EDIT,
            {
                "edits": [
                    {"op": "remove", "id": "02"},
                    {"op": "add", "description": "task 3", "depends_on": ["01"]},
                ],
                "by": "calvin",
            },
        )
        assert await handle.query(QUERY_PENDING_EDITS) == 1  # held while the gate is open
        await handle.signal(SIGNAL_HUMAN_DECISION, "retry")
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        log: list[str] = await handle.query(QUERY_GATE_LOG)
    assert result.completed, "\n".join(log)
    assert (result.items_done, result.items_total) == (2, 2)
    assert commits_with(tmp_path, "lha: checklist edited by calvin") == 1
    assert commits_with(tmp_path, "lha: unblock") == 0  # nothing left to unblock
    assert any("checklist edited by calvin: removed 02; added 03" in line for line in log)
    edits = [e for e in committed_events(tmp_path) if e.kind == EDIT_EVENT]
    assert len(edits) == 1 and edits[0].cycle_id == "e1" and edits[0].payload["by"] == "calvin"


async def test_edit_while_sleeping_and_a_refused_batch(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, cycle_pause_seconds=3600)
    tq = "lha-edit-sleep"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(working_model)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_SLEEPING)
        await handle.signal(SIGNAL_CHECKLIST_EDIT, {"edits": [{"op": "remove", "id": "99"}]})
        await handle.signal(
            SIGNAL_CHECKLIST_EDIT, {"edits": [{"op": "add", "description": "task 3"}], "by": "ops"}
        )

        async def committed() -> bool:
            return commits_with(tmp_path, "lha: checklist edited by ops") == 1

        await wait_for(committed, "the edit commit")
        assert await handle.query(QUERY_PENDING_EDITS) == 0
        assert await handle.query(MissionWorkflow.status) == STATUS_SLEEPING  # the sleep goes on
        await handle.signal(SIGNAL_SNOOZE, 0)
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        log = await handle.query(QUERY_GATE_LOG)
    assert result.completed and (result.items_done, result.items_total) == (3, 3)
    refused = [line for line in log if "checklist edit refused" in line]
    assert len(refused) == 1 and refused[0].endswith("edit #1: unknown item '99'")
    assert commits_with(tmp_path, "lha: checklist edited") == 1
    kinds = ("sleeping until", "checklist edit refused", "checklist edited by ops", "woke up")
    order = [kind for line in log for kind in kinds if kind in line]
    assert order[:4] == list(kinds), log


async def test_edit_with_the_start_lands_before_the_first_cycle(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2)
    tq = "lha-edit-early"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(_item2_never_works)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run,
            inp,
            id=f"mission:{inp.mission_id}",
            task_queue=tq,
            start_signal=SIGNAL_CHECKLIST_EDIT,  # delivered with the start: before any cycle
            start_signal_args=[{"edits": [{"op": "remove", "id": "02"}]}],
        )
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert result.completed and result.cycles == 1
    assert (result.items_done, result.items_total) == (1, 1)
