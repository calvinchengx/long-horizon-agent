"""Durability tests for the escalation ladder, SLEEPING and the deadlock gate's "impossible".

Built on the approval mechanism of ``test_approvals`` (a queued irreversible action opens a gate
after the cycle; an approval reaches the next cycle once). These prove, on the time-skipping
server:

1. a real mission end to end: the flagged command is queued (a ``tool_approval`` "pending" event),
   the gate shows the pending action, a human approves, and the next cycle runs it exactly once;
2. an unanswered gate walks the escalation ladder (reminders as anchor events, in the gate log and
   on the webhook) and then REJECTS by default;
3. SLEEPING is set for the pause between cycles and for a scheduled start (and a snooze wakes it);
4. the deadlock gate can declare the mission impossible (a human or its default), ending it
   IMPOSSIBLE with a final checkpoint; a "retry" default is never used.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any

import httpx
import pytest
from pydantic import SecretStr
from temporalio import activity
from temporalio.client import WorkflowHandle
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.config import Settings
from lha.contracts.model import ModelProvider, ToolCall, TurnResult
from lha.contracts.state import EventRecord, SituationSnapshot
from lha.durable.activities import declare_impossible, make_cycle_activity, make_notify_activity
from lha.durable.signals import (
    GATE_DEADLOCK,
    GATE_TOOL_CALL,
    QUERY_GATE,
    QUERY_GATE_LOG,
    QUERY_STATUS,
    SIGNAL_HUMAN_DECISION,
    SIGNAL_SNOOZE,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_SLEEPING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import (
    OUTCOME_ABORTED,
    OUTCOME_COMPLETED,
    OUTCOME_IMPOSSIBLE,
    CycleInput,
    CycleResult,
    GateView,
    MissionResult,
    PendingApproval,
)
from lha.durable.workflows import MissionWorkflow
from lha.model.stub import StubModel
from lha.safety.commands import classify_command
from lha.state import git_ops
from tests.durability._support import (
    SETTINGS,
    commits_with,
    idle_model,
    init_mission,
    working_model,
    write_turn,
)
from tests.durability.test_durable_spine import (
    ROW_ACTIVITY,
    _healthy,
    _snapshot_activity,
    _unblock_activity,
)

_TIMEOUT_S = 120
_POLL_S = 0.05

ModelFactory = Callable[[Settings, SituationSnapshot], ModelProvider]


def gated_argv(marker: Path) -> list[str]:
    """Appends to ``marker`` (each real execution is countable), then a flagged ``git push`` to a
    remote that does not exist (harmless: it fails at once)."""
    return ["sh", "-c", f"echo ran >> {marker}; git push lha-no-such-remote HEAD"]


def _done() -> TurnResult:
    return TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn")


def gated_model(argv: list[str]) -> ModelFactory:
    """Every cycle: run the flagged ``argv``, write the item's work file, done."""

    def factory(_settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
        assert snapshot.active_item is not None
        call = ToolCall(id="r1", name="run_command", arguments={"argv": argv})
        return StubModel(
            script=[
                TurnResult(tool_calls=[call], stop_reason="tool_use"),
                write_turn(f"work/{snapshot.active_item.id}.txt"),
                _done(),
            ]
        )

    return factory


class Webhook:
    """Captures webhook POSTs through an httpx mock transport (no network)."""

    def __init__(self) -> None:
        self.posts: list[dict[str, Any]] = []

    def transport(self) -> httpx.MockTransport:
        def handle(request: httpx.Request) -> httpx.Response:
            self.posts.append(json.loads(request.content))
            return httpx.Response(204)

        return httpx.MockTransport(handle)


def worker(
    env: WorkflowEnvironment,
    tq: str,
    cycle: object,
    webhook: Webhook | None = None,
    **options: Any,
) -> Worker:
    """``options`` go to ``Worker`` (e.g. a short ``max_heartbeat_throttle_interval`` so a
    cancellation reaches a heartbeating activity in well under the default 60 s)."""
    settings, transport = SETTINGS, None
    if webhook is not None:
        settings = SETTINGS.model_copy(
            update={"gate_webhook_url": SecretStr("https://hooks.test/lha")}
        )
        transport = webhook.transport()
    return Worker(
        env.client,
        task_queue=tq,
        workflows=[MissionWorkflow],
        activities=[
            cycle,  # type: ignore[list-item]
            make_notify_activity(settings=settings, transport=transport),
            declare_impossible,
            _healthy,
            _unblock_activity,
            _snapshot_activity,
            ROW_ACTIVITY,
        ],
        **options,
    )


def cycle_with(factory: ModelFactory) -> object:
    return make_cycle_activity(settings=SETTINGS, model_factory=factory)


async def wait_for(probe: Callable[[], Awaitable[bool]], what: str) -> None:
    async def loop() -> None:
        while not await probe():
            await asyncio.sleep(_POLL_S)

    try:
        await asyncio.wait_for(loop(), _TIMEOUT_S)
    except TimeoutError:
        pytest.fail(f"timed out waiting for {what}")


async def status_is(handle: WorkflowHandle[Any, Any], status: str) -> None:
    async def probe() -> bool:
        return await handle.query(QUERY_STATUS) == status

    await wait_for(probe, f"status {status}")


def committed_events(workdir: Path) -> list[EventRecord]:
    raw = git_ops.run_git(workdir, "show", "HEAD:.lha/events.ndjson")
    return [EventRecord.model_validate_json(line) for line in raw.splitlines() if line.strip()]


# --- 1. real mission: queued → gate → approve → runs once -----------------------------------
@pytest.mark.asyncio
async def test_real_mission_approval_runs_the_action_once(tmp_path: Path) -> None:
    marker = tmp_path / "ran.log"
    argv = gated_argv(marker)
    assert classify_command(argv) is not None
    work = tmp_path / "work"
    inp = await init_mission(work, n=2)
    tq = "lha-gates-approve"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(gated_model(argv))),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)
        gate: GateView | None = await handle.query(QUERY_GATE, result_type=GateView)
        assert gate is not None and gate.kind == GATE_TOOL_CALL
        assert gate.options == ["approve", "reject"] and gate.default_action == "reject"
        assert gate.request is not None and "git push" in gate.request.reason
        assert gate.deadline and gate.next_escalation_at
        assert not marker.exists()  # queued, not run
        await handle.signal(SIGNAL_HUMAN_DECISION, "approve")
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        log: list[str] = await handle.query(QUERY_GATE_LOG)

    assert result.completed and result.outcome == OUTCOME_COMPLETED
    assert result.status == STATUS_DONE and result.cycles == 2
    assert marker.read_text(encoding="utf-8") == "ran\n"  # the approved call ran exactly once
    events = committed_events(work)
    decisions = [(e.cycle_id, e.payload["decision"]) for e in events if e.kind == "tool_approval"]
    assert decisions == [("c1", "pending"), ("c2", "approve")]
    kinds = [e.kind for e in events]
    assert "gate_opened" in kinds and "gate_resolved" in kinds
    assert any("resolved: approve" in line for line in log)


# --- 2. escalation ladder then default reject -----------------------------------------------
PUSH = PendingApproval(
    fingerprint="fp-push", tool="run_command", reason="git push", arguments="['git', 'push']"
)


def scripted_cycles(seen: list[CycleInput]) -> object:
    @activity.defn(name="run_agent_cycle")
    async def cycle(inp: CycleInput) -> CycleResult:
        seen.append(inp)
        first = len(seen) == 1
        return CycleResult(
            item_id="01",
            advanced=True,
            head_sha="abc",
            is_complete=not first,
            items_done=0 if first else 1,
            items_total=1,
            pending_approvals=[PUSH] if first else [],
        )

    return cycle


@pytest.mark.asyncio
async def test_unanswered_gate_escalates_then_rejects(tmp_path: Path) -> None:
    work = tmp_path / "work"
    inp = await init_mission(
        work, n=1, approval_timeout_seconds=3600, gate_escalation_seconds=[2700, 900, 9999]
    )
    seen: list[CycleInput] = []
    webhook = Webhook()
    tq = "lha-gates-ladder"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, scripted_cycles(seen), webhook),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        log: list[str] = await handle.query(QUERY_GATE_LOG)

    assert result.completed and len(seen) == 2
    assert seen[1].approved_actions == []  # the default rejected it
    assert [p["event"] for p in webhook.posts] == [
        "opened",
        "reminder",
        "reminder",
        "defaulted",
    ]  # 9999 s is past the 3600 s timeout: ignored
    assert webhook.posts[0]["request"]["fingerprint"] == "fp-push"
    assert webhook.posts[-1]["decision"] == "reject"
    events = committed_events(work)
    assert [e.payload["step"] for e in events if e.kind == "gate_reminder"] == [1, 2]
    assert [e.kind for e in events if e.kind.startswith("gate_")][-1] == "gate_defaulted"
    assert sum(1 for line in log if "reminder" in line) == 2
    assert any("timed out: default reject" in line for line in log)


@pytest.mark.asyncio
async def test_invalid_decision_keeps_the_gate_open(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path / "work", n=1)
    seen: list[CycleInput] = []
    tq = "lha-gates-invalid"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, scripted_cycles(seen)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)
        await handle.signal(SIGNAL_HUMAN_DECISION, "impossible")  # not a tool-call option

        async def rejected() -> bool:
            return "impossible" in await handle.query(MissionWorkflow.rejected_decisions)

        await wait_for(rejected, "the invalid decision to be rejected")
        assert await handle.query(QUERY_STATUS) == STATUS_WAITING_ON_HUMAN
        await handle.signal(SIGNAL_HUMAN_DECISION, "Approve")
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert result.completed
    assert [a.fingerprint for a in seen[1].approved_actions] == ["fp-push"]


# --- 3. SLEEPING ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_pause_between_cycles_is_sleeping(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=2, cycle_pause_seconds=600)
    tq = "lha-sleep-pause"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(working_model)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_SLEEPING)
        assert await handle.query(MissionWorkflow.cycles_done) == 1
        assert await handle.query(MissionWorkflow.resume_at) > 0
        await handle.signal(SIGNAL_SNOOZE, 0)  # wake it now
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
        log: list[str] = await handle.query(QUERY_GATE_LOG)
    assert result.completed and result.cycles == 2
    assert any("sleeping until" in line for line in log)


@pytest.mark.asyncio
async def test_scheduled_start_sleeps_first(tmp_path: Path) -> None:
    async with await WorkflowEnvironment.start_time_skipping() as env:
        now = (await env.get_current_time()).timestamp()
        inp = await init_mission(tmp_path, n=1, resume_at=now + 3600)
        tq = "lha-sleep-start"
        async with worker(env, tq, cycle_with(working_model)):
            handle = await env.client.start_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            )
            await status_is(handle, STATUS_SLEEPING)
            assert await handle.query(MissionWorkflow.cycles_done) == 0
            result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
            finished = (await env.get_current_time()).timestamp()
    assert result.completed
    assert finished >= now + 3600  # the durable timer really held the start back


# --- 4. deadlock gate: impossible ------------------------------------------------------------
def _never_works(_settings: Settings, _snapshot: SituationSnapshot) -> ModelProvider:
    return idle_model()


@pytest.mark.asyncio
async def test_deadlock_gate_human_declares_impossible(tmp_path: Path) -> None:
    inp = await init_mission(tmp_path, n=1, deadlock_gate_seconds=3600)
    tq = "lha-deadlock-impossible"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(_never_works)),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
        )
        await status_is(handle, STATUS_WAITING_ON_HUMAN)
        gate: GateView | None = await handle.query(QUERY_GATE, result_type=GateView)
        assert gate is not None and gate.kind == GATE_DEADLOCK
        assert gate.options == ["retry", "abort", "impossible"] and gate.default_action == "abort"
        # Item 01 failed 3 cycles in a row: ops.lifecycle.should_declare_impossible says so.
        assert gate.recommended == "impossible" and "Recommended: impossible" in gate.question
        assert "impossible" in await handle.query("open_question")
        await handle.signal(SIGNAL_HUMAN_DECISION, "Impossible")
        result: MissionResult = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    assert result.outcome == OUTCOME_IMPOSSIBLE and result.status == STATUS_IMPOSSIBLE
    assert not result.completed and "by a human" in result.reason
    assert commits_with(tmp_path, "lha: mission declared impossible") == 1
    final = [e for e in committed_events(tmp_path) if e.kind == "mission_impossible"]
    assert len(final) == 1 and final[0].payload["blocked"] == ["01"]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("configured", "outcome"),
    [("impossible", OUTCOME_IMPOSSIBLE), ("retry", OUTCOME_ABORTED)],  # retry: never a default
)
async def test_deadlock_gate_defaults(tmp_path: Path, configured: str, outcome: str) -> None:
    inp = await init_mission(
        tmp_path, n=1, deadlock_gate_seconds=600, deadlock_gate_default=configured
    )
    tq = f"lha-deadlock-default-{configured}"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        worker(env, tq, cycle_with(_never_works)),
    ):
        result: MissionResult = await asyncio.wait_for(
            env.client.execute_workflow(
                MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=tq
            ),
            _TIMEOUT_S,
        )
    assert result.outcome == outcome and "by default" in result.reason
    assert "gate_defaulted" in [e.kind for e in committed_events(tmp_path)]
