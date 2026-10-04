"""The shared event record: trace events persisted to ``mission_events`` by every run path."""

from __future__ import annotations

import sys
from pathlib import Path

from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import checks_from_commands
from lha.model.stub import StubModel
from lha.obs.events import TraceEvent, TraceRecorder
from lha.persistence.event_log import MissionEventLog
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import MissionEvent


class _Failing:
    """A store that refuses every write."""

    def __init__(self) -> None:
        self.calls = 0

    async def append_mission_events(self, events: list[MissionEvent]) -> None:
        self.calls += 1
        raise OSError("disk full")


async def test_events_are_written_in_order_stamped_and_redacted(tmp_path: Path) -> None:
    store = SqliteStore(tmp_path / "s.sqlite3")
    await store.open()
    log = MissionEventLog(store)
    recorder = TraceRecorder()
    recorder.listeners.append(log.add)
    recorder.record("cycle_started", mission_id="m1", cycle_id="c1", item_id="01")
    recorder.record(
        "tool_call",
        mission_id="m1",
        cycle_id="c1",
        tool="grep",
        ok=False,
        error="token=ghp_" + "a" * 36,
    )
    await log.aclose()
    rows = await store.read_mission_events()
    assert [(r.kind, r.cycle_id) for r in rows] == [("cycle_started", "c1"), ("tool_call", "c1")]
    assert rows[0].payload == {"item_id": "01"} and rows[0].ts
    assert "ghp_" + "a" * 36 not in str(rows[1].payload)  # redacted before it is persisted
    assert log.written == 2 and log.dropped == 0
    await store.close()


async def test_a_store_failure_drops_the_batch_and_never_raises() -> None:
    store = _Failing()
    log = MissionEventLog(store)  # type: ignore[arg-type]
    log.add(TraceEvent(kind="x", mission_id="m", cycle_id="c"))
    await log.flush()  # no exception
    assert log.dropped == 1 and log.written == 0 and store.calls == 1
    await log.flush()  # nothing pending: no write attempted
    assert store.calls == 1


def test_the_backlog_is_bounded_by_dropping_the_oldest() -> None:
    log = MissionEventLog(_Failing(), max_pending=3)  # type: ignore[arg-type]
    for i in range(5):
        log.add(TraceEvent(kind=f"k{i}", mission_id="m"))
    assert [e.kind for e in log._pending] == ["k2", "k3", "k4"] and log.dropped == 2


def test_a_failing_listener_never_breaks_recording() -> None:
    recorder = TraceRecorder()

    def boom(_: TraceEvent) -> None:
        raise RuntimeError("listener down")

    recorder.listeners.append(boom)
    event = recorder.record("test_x", mission_id="m")
    assert event.kind == "test_x" and recorder.events == [event]


async def test_a_local_mission_leaves_its_trace_in_the_store(tmp_path: Path) -> None:
    db = tmp_path / "lha.sqlite3"
    settings = Settings(  # type: ignore[call-arg]
        _env_file=None, sandbox="local", allow_unsafe_local=True, sqlite_path=str(db), max_cycles=2
    )
    write = TurnResult(
        text='{"tool": "write_file", "arguments": {"path": "out.txt", "content": "hello"}}'
    )
    done = TurnResult(text='{"done": true, "summary": "ok"}')
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="T",
        description="D",
        checklist=Checklist(items=[ChecklistItem(id="01", description="write out.txt")]),
        checks=checks_from_commands(
            [[sys.executable, "-c", "import sys; sys.exit(open('out.txt').read() != 'hello')"]]
        ),
        settings=settings,
        model=StubModel(script=[write, done]),
    )
    assert summary.completed
    store = SqliteStore(db)
    await store.open()
    rows = await store.read_mission_events(mission_id=summary.mission_id)
    mission = await store.get_mission(summary.mission_id)
    await store.close()
    # The row says where the anchor is, so a reader (lha serve) can find the checklist.
    assert mission is not None and mission.workdir == str((tmp_path / "ws").resolve())
    kinds = [r.kind for r in rows]
    assert "cycle_started" in kinds and "checkpoint" in kinds
    (call,) = [r for r in rows if r.kind == "tool_call"]
    assert call.payload == {"tool": "write_file", "ok": True} and call.cycle_id == "c1"
    assert kinds.index("cycle_started") < kinds.index("tool_call") < kinds.index("checkpoint")


async def test_a_sub_agents_tool_calls_are_recorded_with_its_role(tmp_path: Path) -> None:
    from lha.agents.roles import ROLES
    from lha.agents.subagent import SubAgent
    from lha.contracts.tools import ToolContext
    from lha.execution.dispatcher import AllowListDispatcher
    from lha.execution.sandbox_local import LocalSandbox
    from lha.execution.tools import default_local_tools

    (tmp_path / "a.txt").write_text("x")
    script = [
        TurnResult(text='{"tool": "read_file", "arguments": {"path": "a.txt"}}'),
        TurnResult(text='{"tool": "read_file", "arguments": {"path": "missing.txt"}}'),
        TurnResult(text='{"tool": "write_file", "arguments": {"path": "b", "content": "y"}}'),
        TurnResult(text='{"done": true, "summary": "ok"}'),
    ]
    recorder = TraceRecorder()
    agent = SubAgent(
        role=ROLES["researcher"],  # read-only: write_file is refused
        model=StubModel(script=script),
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        recorder=recorder,
        cycle_id="c3",
    )
    session = await LocalSandbox().open(workdir=str(tmp_path))
    await agent.run(objective="look", ctx=ToolContext(mission_id="m1", session=session))
    calls = [(e.cycle_id, e.data) for e in recorder.events if e.kind == "tool_call"]
    assert calls[0] == ("c3", {"tool": "read_file", "ok": True, "role": "researcher"})
    assert calls[1][1]["ok"] is False and "cannot read" in str(calls[1][1]["error"])
    assert calls[2][1]["tool"] == "write_file" and "not available" in str(calls[2][1]["error"])
    assert all(e.mission_id == "m1" for e in recorder.events)


async def test_an_organizations_sub_agents_leave_their_tool_calls_in_the_store(
    tmp_path: Path,
) -> None:
    from lha.agents.orchestrator import Orchestrator

    db = tmp_path / "lha.sqlite3"
    settings = Settings(  # type: ignore[call-arg]
        _env_file=None,
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        sqlite_path=str(db),
        max_cycles=5,
    )
    look = TurnResult(text='{"tool": "list_files", "arguments": {}}')
    done = TurnResult(text='{"done": true, "summary": "did it"}')
    approve = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
    orchestrator = Orchestrator(
        settings,
        research_per_item=1,
        do_review=True,
        models={
            "lead": StubModel(script=[done]),
            "researcher": StubModel(script=[look, done]),
            "reviewer": StubModel(script=[look, approve]),
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path / "ws"),
        title="Org",
        description="D",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[checks_from_commands([[sys.executable, "-c", "pass"]])[0]],
    )
    assert summary.completed
    store = SqliteStore(db)
    await store.open()
    rows = await store.read_mission_events(mission_id=summary.mission_id)
    await store.close()
    roles = sorted(
        str(r.payload.get("role")) for r in rows if r.kind == "tool_call" and "role" in r.payload
    )
    assert roles == ["researcher", "reviewer"], [(r.kind, r.payload) for r in rows]


class _DownModel(StubModel):
    """A model whose every call fails (a researcher that cannot run)."""

    async def complete(self, messages, **kwargs):  # type: ignore[no-untyped-def, override]
        raise RuntimeError("researcher down")


async def test_a_failed_researcher_is_recorded_and_the_item_still_runs(tmp_path: Path) -> None:
    from lha.agents.orchestrator import Orchestrator

    db = tmp_path / "lha.sqlite3"
    settings = Settings(  # type: ignore[call-arg]
        _env_file=None,
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        sqlite_path=str(db),
        max_cycles=5,
    )
    done = TurnResult(text='{"done": true, "summary": "did it"}')
    orchestrator = Orchestrator(
        settings,
        research_per_item=1,
        do_review=False,
        models={"lead": StubModel(script=[done]), "researcher": _DownModel(script=[])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path / "ws"),
        title="Org",
        description="D",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[checks_from_commands([[sys.executable, "-c", "pass"]])[0]],
    )
    assert summary.completed
    store = SqliteStore(db)
    await store.open()
    rows = await store.read_mission_events(mission_id=summary.mission_id)
    await store.close()
    (failed,) = [r.payload for r in rows if r.kind == "research_failed"]
    assert failed["item"] == "01" and "researcher down" in str(failed["error"])
