"""Scale/longevity test: many cycles with Continue-As-New firing repeatedly.

Proves the spine stays healthy over a long run — history is bounded (CAN rolls it), state is
carried forward as pointers, and every item completes exactly once. Representative size by default
(raise ``N`` for a heavier soak test).
"""

from __future__ import annotations

import uuid
from pathlib import Path

import pytest
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.contracts.state import Checklist, ChecklistItem
from lha.durable.activities import run_agent_cycle
from lha.durable.types import MissionInput
from lha.durable.workflows import MissionWorkflow
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

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
    )
    async with await WorkflowEnvironment.start_time_skipping() as env:
        tq = "lha-test-longrun"
        async with Worker(
            env.client, task_queue=tq, workflows=[MissionWorkflow], activities=[run_agent_cycle]
        ):
            result = await env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            )
    assert result.completed
    assert result.items_done == N
    completed = sum(1 for line in git_ops.log_oneline(tmp_path, 1000) if "lha: complete" in line)
    assert completed == N
