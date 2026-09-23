"""Scale/longevity test: many cycles with Continue-As-New firing repeatedly.

Proves the spine stays healthy over a long run — history is bounded (CAN rolls it), state is
carried forward as pointers, and every item completes exactly once. Representative size by default
(raise ``N`` for a heavier soak test).
"""

from __future__ import annotations

import asyncio
import uuid
from pathlib import Path

import pytest
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.contracts.state import Checklist, ChecklistItem
from lha.durable.activities import make_cycle_activity, make_record_status_activity
from lha.durable.types import OUTCOME_COMPLETED, MissionInput
from lha.durable.workflows import MissionWorkflow
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from tests.durability._support import CHECK_COMMANDS, SETTINGS, working_model

N = 25  # items == cycles; CAN every 5 → multiple history rolls


@pytest.mark.asyncio
async def test_long_run_bounded_history_with_continue_as_new(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(
        title="long run",
        description="scale",
        items=Checklist(
            items=[ChecklistItem(id=f"{i:02d}", description=f"task {i}") for i in range(1, N + 1)]
        ),
    )
    inp = MissionInput(
        mission_id=uuid.uuid4().hex[:8],
        workdir=str(tmp_path),
        max_cycles=500,
        cycles_before_can=5,
        check_commands=CHECK_COMMANDS,  # a real gating check per item
    )
    async with await WorkflowEnvironment.start_time_skipping() as env:
        tq = "lha-test-longrun"
        async with Worker(
            env.client,
            task_queue=tq,
            workflows=[MissionWorkflow],
            activities=[
                make_cycle_activity(settings=SETTINGS, model_factory=working_model),
                make_record_status_activity(settings=SETTINGS),
            ],
        ):
            result = await asyncio.wait_for(
                env.client.execute_workflow(
                    MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
                ),
                300,
            )
    assert result.completed and result.outcome == OUTCOME_COMPLETED
    assert (result.items_done, result.items_total) == (N, N)
    assert result.cycles == N
    assert result.head_sha == git_ops.head_sha(tmp_path)
    completed = sum(1 for line in git_ops.log_oneline(tmp_path, 1000) if "lha: complete" in line)
    assert completed == N
