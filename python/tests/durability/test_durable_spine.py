"""Durability tests for the Temporal spine.

These run against Temporal's in-process **time-skipping** test server — no Docker, no API key,
deterministic, and fast (timers and retry backoffs are auto-skipped). Every item is gated by a
REAL check command run in the (local) sandbox; nothing completes on the model's say-so. They
prove:

1. a mission runs to completion through the durable scheduler, with real counts and head sha;
2. a crash AFTER the commit is recovered by retry without advancing another item (the retried
   attempt finds its own checkpoint in HEAD) — no double-apply;
3. a crash BEFORE the commit leaves no residue: the retry resets the checkout to HEAD, so the
   crashed attempt's partial edits are never committed;
4. Continue-As-New (history bounding) does not break long runs;
5. a mission whose remaining item can never pass is reported ``deadlocked`` (not completed);
6. an outage longer than the activity retries PARKS the mission (durable timers + health probes)
   and it resumes when healthy instead of failing;
7. budget exhaustion and invalid configuration end the mission explicitly;
8. the human gate honors an early "retry" decision and rejects decisions outside its options.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from temporalio import activity
from temporalio.api.enums.v1 import EventType
from temporalio.client import WorkflowFailureError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.config import Settings
from lha.contracts.model import ModelProvider, Usage
from lha.contracts.state import SituationSnapshot
from lha.durable.activities import (
    _execute_cycle,
    _read_snapshot,
    _unblock,
    declare_impossible,
    make_cycle_activity,
    notify_gate,
)
from lha.durable.signals import (
    QUERY_STATUS,
    SIGNAL_HUMAN_DECISION,
    STATUS_ABORTED,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
)
from lha.durable.types import (
    OUTCOME_ABORTED,
    OUTCOME_BUDGET_EXHAUSTED,
    OUTCOME_COMPLETED,
    OUTCOME_DEADLOCKED,
    CycleInput,
    CycleResult,
    HealthInput,
    HealthReport,
    MissionInput,
    MissionResult,
    UnblockInput,
)
from lha.durable.workflows import MissionWorkflow
from lha.model.stub import StubModel
from lha.state import git_ops
from tests.durability._support import (
    SETTINGS,
    all_committed_paths,
    commits_with,
    idle_model,
    init_mission,
    working_model,
    write_turn,
)

_TIMEOUT_S = 120  # a hung workflow must fail the test, not wedge the suite


@activity.defn(name="check_mission_health")
async def _healthy(inp: HealthInput) -> HealthReport:
    return HealthReport(healthy=True, reason="ok")


@activity.defn(name="unblock_items")
async def _unblock_activity(inp: UnblockInput) -> CycleResult:
    return await _unblock(inp)


@activity.defn(name="read_mission_snapshot")
async def _snapshot_activity(inp: HealthInput) -> CycleResult:
    return await _read_snapshot(inp)


# The gate activities (anchor events + optional webhook, final "impossible" checkpoint).
GATE_ACTIVITIES = [notify_gate, declare_impossible]


async def _run(
    env: WorkflowEnvironment,
    inp: MissionInput,
    cycle_activity: object,
    *,
    health: object = _healthy,
    task_queue: str = "lha-test",
) -> MissionResult:
    async with Worker(
        env.client,
        task_queue=task_queue,
        workflows=[MissionWorkflow],
        activities=[
            cycle_activity,
            health,
            _unblock_activity,
            _snapshot_activity,
            *GATE_ACTIVITIES,
        ],  # type: ignore[list-item]
    ):
        return await asyncio.wait_for(
            env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=task_queue
            ),
            _TIMEOUT_S,
        )


@pytest.mark.asyncio
async def test_mission_completes(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=3)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(
            env, inp, make_cycle_activity(settings=SETTINGS, model_factory=working_model)
        )
    assert result.completed and result.outcome == OUTCOME_COMPLETED
    assert (result.items_done, result.items_total) == (3, 3)
    assert result.head_sha == git_ops.head_sha(tmp_path)
    assert result.status == STATUS_DONE
    # Each item produced exactly one verified checkpoint commit.
    assert commits_with(tmp_path, "lha: complete") == 3


# --- crash AFTER the commit ------------------------------------------------------------
_after: dict[str, bool] = {"crashed": False}


@activity.defn(name="run_agent_cycle")
async def _crash_after_commit(inp: CycleInput) -> CycleResult:
    """Do the real work (commit), then crash ONCE before returning."""
    result = await _execute_cycle(inp, settings=SETTINGS, model_factory=working_model)
    if not _after["crashed"] and result.advanced:
        _after["crashed"] = True
        raise RuntimeError("injected one-time crash after side effect")
    return result


@pytest.mark.asyncio
async def test_crash_after_commit_is_idempotent(tmp_path: Path) -> None:
    _after["crashed"] = False
    inp = await init_mission(tmp_path, n=3)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(env, inp, _crash_after_commit)
    assert _after["crashed"]
    assert result.completed
    # The retried attempt found its cycle's checkpoint in HEAD and returned it instead of doing
    # another item under the same cycle id: 3 items, 3 commits, 3 cycles.
    assert commits_with(tmp_path, "lha: complete") == 3
    assert result.cycles == 3


# --- crash BEFORE the commit -----------------------------------------------------------
class _CrashingModel(StubModel):
    """Writes partial work + a residue file, then the 'process dies' mid-cycle."""

    def __init__(self, item_id: str, workdir: Path) -> None:
        self._workdir = workdir
        super().__init__(
            script=[
                write_turn(f"work/{item_id}.txt", "partial"),
                write_turn("residue.txt", "half-finished edit"),
            ]
        )

    async def complete(self, messages, *, tools=None, max_tokens=None):  # type: ignore[no-untyped-def]
        if self._turn >= 2:
            _before["residue_seen"] = int((self._workdir / "residue.txt").exists())
            raise RuntimeError("injected crash before commit")
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


_before: dict[str, int] = {"crashes": 0, "residue_seen": 0}
_workdir: list[Path] = []


def _crash_once_model(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    assert snapshot.active_item is not None
    if snapshot.active_item.id == "02" and _before["crashes"] == 0:
        _before["crashes"] += 1
        return _CrashingModel("02", _workdir[0])
    return working_model(settings, snapshot)


@pytest.mark.asyncio
async def test_crash_before_commit_discards_residue(tmp_path: Path) -> None:
    _before.update(crashes=0, residue_seen=0)
    _workdir[:] = [tmp_path]
    inp = await init_mission(tmp_path, n=3)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(
            env, inp, make_cycle_activity(settings=SETTINGS, model_factory=_crash_once_model)
        )
    assert _before["crashes"] == 1
    assert _before["residue_seen"] == 1  # the crashed attempt really left residue behind
    assert result.completed and (result.items_done, result.items_total) == (3, 3)
    assert "residue.txt" not in all_committed_paths(tmp_path)
    assert not (tmp_path / "residue.txt").exists()
    assert (tmp_path / "work" / "02.txt").read_text(encoding="utf-8") == "done"
    assert commits_with(tmp_path, "lha: complete") == 3


@pytest.mark.asyncio
async def test_continue_as_new_completes(tmp_path: Path) -> None:
    # Force a Continue-As-New after every single cycle, then confirm the mission still completes.
    inp = await init_mission(tmp_path, n=3, cycles_before_can=1)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(
            env, inp, make_cycle_activity(settings=SETTINGS, model_factory=working_model)
        )
    assert result.completed
    assert (result.items_done, result.items_total) == (3, 3)
    assert result.cycles == 3  # carried across runs
    assert commits_with(tmp_path, "lha: complete") == 3


# --- deadlock ------------------------------------------------------------------------------
def _item2_never_works(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    assert snapshot.active_item is not None
    if snapshot.active_item.id == "02":
        return idle_model()
    return working_model(settings, snapshot)


@pytest.mark.asyncio
async def test_deadlocked_mission_is_reported_as_deadlocked(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(
            env, inp, make_cycle_activity(settings=SETTINGS, model_factory=_item2_never_works)
        )
    assert not result.completed
    assert result.outcome == OUTCOME_DEADLOCKED
    assert result.status == STATUS_IMPOSSIBLE
    assert (result.items_done, result.items_total) == (1, 2)
    assert "02" in result.reason
    # 1 success + 3 failed attempts on item 02 (then it is blocked and never re-picked).
    assert result.cycles == 4
    assert commits_with(tmp_path, "lha: complete") == 1


# --- park and resume after an outage -------------------------------------------------------
_outage: dict[str, int] = {"failures_left": 0, "health_calls": 0}


@activity.defn(name="run_agent_cycle")
async def _flaky_dependency_cycle(inp: CycleInput) -> CycleResult:
    if _outage["failures_left"] > 0:
        _outage["failures_left"] -= 1
        raise ConnectionError("model API unreachable (injected outage)")
    return await _execute_cycle(inp, settings=SETTINGS, model_factory=working_model)


@activity.defn(name="check_mission_health")
async def _recovering_health(inp: HealthInput) -> HealthReport:
    _outage["health_calls"] += 1
    if _outage["health_calls"] < 2:
        return HealthReport(healthy=False, reason="model down")
    return HealthReport(healthy=True, reason="ok")


@pytest.mark.asyncio
async def test_outage_parks_then_resumes(tmp_path: Path) -> None:
    # 7 consecutive failures > the 5 attempts of the cycle retry policy: without parking, the
    # workflow would fail.
    _outage.update(failures_left=7, health_calls=0)
    inp = await init_mission(tmp_path, n=2)
    tq = "lha-test-park"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue=tq,
            workflows=[MissionWorkflow],
            activities=[
                _flaky_dependency_cycle,
                _recovering_health,
                _unblock_activity,
                _snapshot_activity,
            ],
        ),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        result = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        timers = [
            e
            async for e in handle.fetch_history_events()
            if e.event_type == EventType.EVENT_TYPE_TIMER_STARTED
        ]
        status = await handle.query(QUERY_STATUS)  # a worker must be up to answer queries
    assert result.completed and (result.items_done, result.items_total) == (2, 2)
    assert _outage["health_calls"] >= 2
    assert len(timers) >= 2  # durable backoff sleeps while parked
    assert status == STATUS_DONE


# --- explicit non-success endings ------------------------------------------------------
class _PricedStub(StubModel):
    def estimate_cost_usd(self, usage: Usage) -> float:
        return 5.0  # every call is priced far above the tiny budget below


def _priced_model(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    return _PricedStub(script=working_model(settings, snapshot)._script)  # type: ignore[attr-defined]


@pytest.mark.asyncio
async def test_budget_exhaustion_ends_mission(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, budget_usd=1.0)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result = await _run(
            env, inp, make_cycle_activity(settings=SETTINGS, model_factory=_priced_model)
        )
    assert not result.completed
    assert result.outcome == OUTCOME_BUDGET_EXHAUSTED
    assert "budget" in result.reason
    assert (result.items_done, result.items_total) == (0, 2)


@pytest.mark.asyncio
async def test_empty_check_list_fails_the_mission(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=1, check_commands=[])
    async with await WorkflowEnvironment.start_time_skipping() as env:
        with pytest.raises(WorkflowFailureError) as info:
            await _run(
                env, inp, make_cycle_activity(settings=SETTINGS, model_factory=working_model)
            )
    assert "gating check" in str(info.value.cause)
    assert commits_with(tmp_path, "lha: complete") == 0


# --- human gate on deadlock ------------------------------------------------------------
_gate: dict[str, int] = {"idle_cycles": 3}


def _fails_three_times(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    if _gate["idle_cycles"] > 0:
        _gate["idle_cycles"] -= 1
        return idle_model()
    return working_model(settings, snapshot)


async def _run_with_signal(
    env: WorkflowEnvironment, inp: MissionInput, decision: str, factory: object
) -> tuple[MissionResult, list[str]]:
    tq = "lha-test-gate"
    async with Worker(
        env.client,
        task_queue=tq,
        workflows=[MissionWorkflow],
        activities=[
            make_cycle_activity(settings=SETTINGS, model_factory=factory),  # type: ignore[arg-type]
            _healthy,
            _unblock_activity,
            _snapshot_activity,
            *GATE_ACTIVITIES,
        ],
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        # Sent BEFORE the gate opens: must be held, not cleared, until the gate consumes it.
        await handle.signal(SIGNAL_HUMAN_DECISION, decision)
        result = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        rejected: list[str] = await handle.query(MissionWorkflow.rejected_decisions)
    return result, rejected


@pytest.mark.asyncio
async def test_early_retry_decision_unblocks_and_completes(tmp_path: Path) -> None:
    _gate["idle_cycles"] = 3
    inp = await init_mission(tmp_path, n=1, deadlock_gate_seconds=3600)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result, rejected = await _run_with_signal(env, inp, "retry", _fails_three_times)
    assert result.completed
    assert rejected == []
    assert commits_with(tmp_path, "lha: unblock") == 1


@pytest.mark.asyncio
async def test_invalid_decision_is_rejected_and_default_applies(tmp_path: Path) -> None:
    _gate["idle_cycles"] = 99
    inp = await init_mission(tmp_path, n=1, deadlock_gate_seconds=600)
    async with await WorkflowEnvironment.start_time_skipping() as env:
        result, rejected = await _run_with_signal(env, inp, "maybe later", _fails_three_times)
    # The default "abort" after the timeout ends the mission ABORTED (not IMPOSSIBLE).
    assert result.outcome == OUTCOME_ABORTED and result.status == STATUS_ABORTED
    assert "by default" in result.reason
    assert rejected == ["maybe later"]
