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
    event = recorder.record("x", mission_id="m")
    assert event.kind == "x" and recorder.events == [event]


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
    await store.close()
    kinds = [r.kind for r in rows]
    assert "cycle_started" in kinds and "checkpoint" in kinds
    (call,) = [r for r in rows if r.kind == "tool_call"]
    assert call.payload == {"tool": "write_file", "ok": True} and call.cycle_id == "c1"
    assert kinds.index("cycle_started") < kinds.index("tool_call") < kinds.index("checkpoint")
