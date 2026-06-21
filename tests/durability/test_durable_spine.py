"""Phase-0 durability tests for the Temporal spine.

These run against Temporal's in-process **time-skipping** test server — no Docker, no API key,
deterministic, and fast (retry backoffs are auto-skipped). They prove the three core guarantees:

1. a mission runs to completion through the durable scheduler;
2. a crash *after a side effect* (commit) is recovered by retry WITHOUT double-applying the step
   (idempotency) — the literal "reboot on day 12 loses nothing" property;
3. Continue-As-New (history bounding) does not break long runs.
"""

from __future__ import annotations

import uuid
from pathlib import Path

import pytest
from temporalio import activity
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.contracts.state import Checklist, ChecklistItem
from lha.durable.activities import _execute_cycle, run_agent_cycle
from lha.durable.types import CycleInput, CycleResult, MissionInput
from lha.durable.workflows import MissionWorkflow
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor


def _checklist(n: int) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id=f"{i:02d}", description=f"task {i}") for i in range(1, n + 1)]
    )


async def _init_mission(tmp_path: Path, n: int, *, cycles_before_can: int = 200) -> MissionInput:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="Test mission", description="phase-0", items=_checklist(n))
    return MissionInput(
        mission_id=uuid.uuid4().hex[:8],
        workdir=str(tmp_path),
        max_cycles=50,
        cycles_before_can=cycles_before_can,
    )


def _complete_commits(workdir: Path) -> int:
    return sum(1 for line in git_ops.log_oneline(workdir, 200) if "lha: complete" in line)


@pytest.mark.asyncio
async def test_mission_completes(tmp_path: Path) -> None:
    inp = await _init_mission(tmp_path, n=3)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        tq = "lha-test-complete"
        async with Worker(
            env.client, task_queue=tq, workflows=[MissionWorkflow], activities=[run_agent_cycle]
        ):
            result = await env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            )
    assert result.completed
    assert result.items_done == 3
    assert result.items_total == 3
    # Each item produced exactly one checkpoint commit.
    assert _complete_commits(tmp_path) == 3


# --- crash-after-side-effect idempotency -----------------------------------------------
_crash_state: dict[str, bool] = {"crashed": False}


@activity.defn(name="run_agent_cycle")
async def _flaky_cycle(inp: CycleInput) -> CycleResult:
    """Do the real work (commit), then crash ONCE before returning — a mid-cycle crash."""
    result = await _execute_cycle(inp)  # real side effect (git commit) happens here
    if not _crash_state["crashed"]:
        _crash_state["crashed"] = True
        raise RuntimeError("injected one-time crash after side effect")
    return result


@pytest.mark.asyncio
async def test_crash_after_side_effect_is_idempotent(tmp_path: Path) -> None:
    _crash_state["crashed"] = False
    inp = await _init_mission(tmp_path, n=3)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        tq = "lha-test-crash"
        async with Worker(
            env.client, task_queue=tq, workflows=[MissionWorkflow], activities=[_flaky_cycle]
        ):
            result = await env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            )
    assert _crash_state["crashed"]  # the injected crash really fired
    assert result.completed
    # The crash happened AFTER a commit; on retry the cycle re-read fresh state and continued —
    # so every item is committed exactly once. No double-apply.
    assert _complete_commits(tmp_path) == 3


@pytest.mark.asyncio
async def test_continue_as_new_completes(tmp_path: Path) -> None:
    # Force a Continue-As-New after every single cycle, then confirm the mission still completes.
    inp = await _init_mission(tmp_path, n=3, cycles_before_can=1)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        tq = "lha-test-can"
        async with Worker(
            env.client, task_queue=tq, workflows=[MissionWorkflow], activities=[run_agent_cycle]
        ):
            result = await env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            )
    assert result.completed
    assert result.items_done == 3
    assert _complete_commits(tmp_path) == 3
