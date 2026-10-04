"""The SQLite mission store, the store factory/fallback, and the ledger/mission plumbing."""

from __future__ import annotations

import asyncio
import sqlite3
from pathlib import Path

import pytest
from pydantic import SecretStr

from lha.config import Settings
from lha.contracts.memory import MemoryRecord
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.governor.cost import CostEntry, CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import CostMeter
from lha.memory.skills import Skill, SkillNotVerifiedError
from lha.model.stub import StubModel
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import (
    BACKEND_SQLITE,
    GateEvent,
    MissionEvent,
    MissionStore,
    StoreUnavailableError,
    default_sqlite_path,
    describe_store,
    open_store,
    resolve_sqlite_path,
)
from lha.persistence.tracking import LedgerSink, MissionTracker, status_for_stop


@pytest.fixture
async def store(tmp_path: Path) -> SqliteStore:
    s = SqliteStore(tmp_path / "db" / "lha.sqlite3")
    await s.open()
    yield s  # type: ignore[misc]
    await s.close()


def _entry(cycle: str, *, usd: float, known: bool = True, role: str = "lead") -> CostEntry:
    return CostEntry(
        cycle_id=cycle,
        model="m",
        input_tokens=10,
        output_tokens=5,
        usd=usd,
        cost_known=known,
        role=role,
    )


# --- schema / file ----------------------------------------------------------------------------
async def test_sqlite_store_is_wal_and_schema_is_versioned(store: SqliteStore) -> None:
    assert isinstance(store, MissionStore)
    assert store.backend == BACKEND_SQLITE
    assert store.journal_mode.lower() == "wal"
    conn = sqlite3.connect(store.path)
    versions = [r[0] for r in conn.execute("SELECT version FROM schema_migrations")]
    tables = {r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type = 'table'")}
    conn.close()
    assert versions == [
        "sqlite_0001_init",
        "sqlite_0002_hitl_gates",
        "sqlite_0003_mission_events",
        "sqlite_0004_mission_workdir",
    ]
    assert {
        "missions",
        "hitl_gates",
        "cost_ledger",
        "episodic_events",
        "mission_events",
        "semantic_memory",
        "skills",
    } <= tables

    # Re-opening an existing file applies nothing twice and keeps the data.
    await store.upsert_mission(mission_id="m1", title="t", status="RUNNING")
    await store.close()
    again = SqliteStore(store.path)
    await again.open()
    assert (await again.get_mission("m1")) is not None
    await again.close()


# --- missions ---------------------------------------------------------------------------------
async def test_mission_upsert_tracks_status_and_keeps_head(store: SqliteStore) -> None:
    assert await store.get_mission("nope") is None
    await store.upsert_mission(mission_id="m1", title="T", description="D", status="RUNNING")
    await store.upsert_mission(
        mission_id="m1", title="", status="RUNNING", head_sha="abc", workdir="/w"
    )
    await store.upsert_mission(mission_id="m1", title="", status="DONE")  # head_sha=None keeps
    row = await store.get_mission("m1")
    assert row is not None
    assert (row.title, row.description, row.status, row.head_sha) == ("T", "D", "DONE", "abc")
    assert row.workdir == "/w"  # a write without a workdir keeps it
    assert row.created_at and row.updated_at >= row.created_at

    await store.upsert_mission(mission_id="m2", title="second", status="RUNNING")
    assert [m.mission_id for m in await store.list_missions()] == ["m2", "m1"]
    assert [m.mission_id for m in await store.list_missions(limit=1)] == ["m2"]


@pytest.mark.parametrize("terminal", ["DONE", "IMPOSSIBLE", "ABORTED"])
async def test_a_terminal_status_is_never_overwritten_by_a_non_terminal_one(
    store: SqliteStore, terminal: str
) -> None:
    await store.upsert_mission(mission_id="m1", title="T", status="RUNNING")
    await store.upsert_mission(mission_id="m1", title="", status=terminal)
    # A late write from a cycle that was still finishing (e.g. after mission-abort).
    for late in ("RUNNING", "WAITING_ON_HUMAN", "SLEEPING", "DEGRADED_PARK"):
        await store.upsert_mission(mission_id="m1", title="", status=late, head_sha="late")
    row = await store.get_mission("m1")
    assert row is not None and row.status == terminal
    assert row.head_sha == "late"  # the other fields still update
    # Terminal to terminal is allowed (the workflow's final word), and so is an explicit reopen.
    await store.upsert_mission(mission_id="m1", title="", status="ABORTED")
    row = await store.get_mission("m1")
    assert row is not None and row.status == "ABORTED"
    await store.upsert_mission(mission_id="m1", title="", status="RUNNING", reopen=True)
    row = await store.get_mission("m1")
    assert row is not None and row.status == "RUNNING"


# --- human gates ------------------------------------------------------------------------------
def _gate(event: str, at: str, *, step: int = 0, decision: str = "", by: str = "") -> GateEvent:
    return GateEvent(
        mission_id="m1",
        gate_id="deadlock-3",
        kind="deadlock",
        event=event,
        at=at,
        question="Mission m1 is deadlocked. Retry, abort or impossible?",
        options=["retry", "abort", "impossible"],
        default_action="abort",
        deadline="2026-01-01T02:00:00+00:00",
        decision=decision,
        resolved_by=by,
        step=step,
    )


async def test_gate_lifecycle_is_recorded_idempotently(store: SqliteStore) -> None:
    opened = _gate("opened", "2026-01-01T01:00:00+00:00")
    await store.record_gate_event(opened)
    await store.record_gate_event(opened)  # a retried write
    (row,) = await store.list_gates("m1")
    assert (row.status, row.kind, row.reminders, row.decision) == ("OPEN", "deadlock", 0, None)
    assert row.options == ["retry", "abort", "impossible"] and row.default_action == "abort"
    assert row.opened_at == "2026-01-01T01:00:00+00:00" and row.risk == "deadlock"

    for step in (1, 2, 1):  # the last is an out-of-order retry: the count never goes down
        await store.record_gate_event(_gate("reminder", "2026-01-01T01:10:00+00:00", step=step))
    (row,) = await store.list_gates("m1")
    assert (row.status, row.reminders) == ("ESCALATED", 2)

    resolved = _gate("resolved", "2026-01-01T01:30:00+00:00", decision="retry", by="human")
    await store.record_gate_event(resolved)
    await store.record_gate_event(resolved)
    # A late reminder or a second, different close never changes a closed gate.
    await store.record_gate_event(_gate("reminder", "2026-01-01T01:40:00+00:00", step=3))
    await store.record_gate_event(_gate("defaulted", "2026-01-01T02:00:00+00:00", decision="abort"))
    (row,) = await store.list_gates()
    assert (row.status, row.decision, row.resolved_by) == ("RESOLVED", "retry", "human")
    assert (row.reminders, row.resolved_at) == (2, "2026-01-01T01:30:00+00:00")

    # The same gate id opened again later (e.g. the same action asked for again) reopens it.
    await store.record_gate_event(_gate("opened", "2026-01-02T00:00:00+00:00"))
    (row,) = await store.list_gates("m1")
    assert (row.status, row.decision, row.reminders) == ("OPEN", None, 0)


async def test_a_close_without_an_opening_still_records_the_outcome(store: SqliteStore) -> None:
    request = {"fingerprint": "f" * 32, "tool": "run_command", "argv": '["make", "release"]'}
    await store.record_gate_event(
        GateEvent(
            mission_id="m2",
            gate_id="approval-ffff",
            kind="tool_call",
            event="defaulted",
            at="2026-01-01T00:00:00+00:00",
            options=["approve", "reject"],
            default_action="reject",
            decision="reject",
            resolved_by="default (timeout)",
            risk="irreversible",
            request=request,
        )
    )
    await store.record_gate_event(_gate("opened", "2026-01-03T00:00:00+00:00"))
    rows = await store.list_gates()
    assert [r.mission_id for r in rows] == ["m1", "m2"]  # most recently opened first
    assert [r.mission_id for r in await store.list_gates(limit=1)] == ["m1"]
    (m2,) = await store.list_gates("m2")
    assert (m2.status, m2.decision, m2.request, m2.risk) == (
        "DEFAULTED",
        "reject",
        request,
        "irreversible",
    )
    assert await store.list_gates("nope") == []
    with pytest.raises(ValueError, match="unknown gate event"):
        await store.record_gate_event(_gate("exploded", "2026-01-01T00:00:00+00:00"))


# --- cost ledger ------------------------------------------------------------------------------
async def test_cost_ledger_is_idempotent_and_unknown_cost_is_null(store: SqliteStore) -> None:
    assert await store.record_cost("m1", _entry("c1", usd=0.25), call_key="a#0") is True
    assert await store.record_cost("m1", _entry("c1", usd=0.25), call_key="a#0") is False
    assert await store.record_cost("m1", _entry("c1", usd=9.0, known=False), call_key="a#1")
    assert await store.record_cost("m1", _entry("c2", usd=0.5), call_key="a#0")  # other cycle
    assert await store.record_cost("m2", _entry("c1", usd=7.0), call_key="a#0")  # other mission

    summary = await store.cost_summary("m1")
    assert summary.calls == 3
    assert summary.known_usd == pytest.approx(0.75)  # unknown is NOT counted as $0 or $9
    assert summary.unknown_cost_calls == 1
    assert (summary.input_tokens, summary.output_tokens) == (30, 15)

    rows = await store.list_costs("m1")
    assert [r.cycle_id for r in rows] == ["c1", "c1", "c2"]
    assert rows[1].usd is None and rows[1].cost_known is False
    assert rows[0].usd == pytest.approx(0.25) and rows[0].role == "lead"
    assert [r.cycle_id for r in await store.list_costs("m1", limit=1)] == ["c2"]
    assert (await store.cost_summary("none")).calls == 0


# --- events -----------------------------------------------------------------------------------
async def test_events_are_ordered_filterable_and_bounded(store: SqliteStore) -> None:
    ids = [
        await store.append_event(
            "m1", cycle_id=f"c{i}", kind="k" if i % 2 else "j", payload={"i": i}
        )
        for i in range(6)
    ]
    assert ids == sorted(ids)
    everything = await store.list_events("m1")
    assert [e.payload["i"] for e in everything] == [0, 1, 2, 3, 4, 5]
    assert [e.payload["i"] for e in await store.list_events("m1", kinds=("k",))] == [1, 3, 5]
    assert [e.payload["i"] for e in await store.list_events("m1", limit=2)] == [4, 5]
    assert [e.payload["i"] for e in await store.list_events("m1", after_id=ids[3])] == [4, 5]
    assert await store.list_events("other") == []


# --- semantic memory --------------------------------------------------------------------------
async def test_mission_events_are_appended_in_order_and_read_forward(store: SqliteStore) -> None:
    await store.append_mission_events(
        [
            MissionEvent("m1", "c1", "cycle_started", {"item_id": "01"}),
            MissionEvent("m2", "c1", "cycle_started", {"item_id": "07"}, ts="2026-10-03T00:00:00Z"),
            MissionEvent("m1", "c1", "tool_call", {"tool": "grep", "ok": True}),
        ]
    )
    await store.append_mission_events([])  # nothing to write is not an error
    every = await store.read_mission_events()
    assert [(e.mission_id, e.kind) for e in every] == [
        ("m1", "cycle_started"),
        ("m2", "cycle_started"),
        ("m1", "tool_call"),
    ]
    assert every[0].id < every[1].id < every[2].id and every[0].ts  # stamped at the write
    assert every[1].ts == "2026-10-03T00:00:00Z" and every[2].payload == {
        "tool": "grep",
        "ok": True,
    }
    m1 = await store.read_mission_events(mission_id="m1")
    assert [e.kind for e in m1] == ["cycle_started", "tool_call"]
    # Paging forward: the oldest ``limit`` after the last id seen, never the newest.
    first = await store.read_mission_events(limit=1)
    rest = await store.read_mission_events(after_id=first[-1].id, limit=5)
    assert [e.id for e in first + rest] == [e.id for e in every]
    assert await store.read_mission_events(after_id=every[-1].id) == []
    assert {e.schema_version for e in every} == {1}
    last = await store.last_mission_event("m1")
    assert last is not None and last.id == every[2].id and last.kind == "tool_call"
    assert await store.last_mission_event("m3") is None


async def test_memory_upsert_vectors_gating_and_soft_invalidation(store: SqliteStore) -> None:
    a = MemoryRecord(id="a", kind="fact", text="alpha", metadata={"item_id": "01"})
    b = MemoryRecord(id="b", kind="progress", text="beta")
    await store.put_memory(
        "m1", [a], vectors=[[1.0, 0.0]], embedding_model="e", embedding_version="1"
    )
    await store.put_memory("m1", [b])  # lexical-only row (no vector)
    await store.put_memory(
        "m1",
        [a.model_copy(update={"text": "alpha v2"})],
        vectors=[[0.0, 1.0]],
        embedding_model="e",
        embedding_version="1",
    )

    listed = await store.list_memory("m1")
    assert [(r.id, r.text) for r in listed] == [("a", "alpha v2"), ("b", "beta")]
    assert listed[0].metadata == {"item_id": "01"} and listed[0].kind == "fact"

    vecs = await store.memory_vectors("m1", embedding_model="e", embedding_version="1")
    assert [(r.id, v) for r, v in vecs] == [("a", [0.0, 1.0])]
    assert await store.memory_vectors("m1", embedding_model="e", embedding_version="2") == []

    assert await store.invalidate_memory(["a", "zzz"]) == 1
    assert await store.invalidate_memory(["a"]) == 0
    assert [r.id for r in await store.list_memory("m1")] == ["b"]
    assert await store.invalidate_memory([]) == 0
    with pytest.raises(ValueError, match="one vector per record"):
        await store.put_memory("m1", [a, b], vectors=[[1.0]])


async def test_stale_memory_is_unvectored_or_another_embedders(store: SqliteStore) -> None:
    def rec(i: str) -> MemoryRecord:
        return MemoryRecord(id=i, kind="fact", text=i)

    await store.put_memory(
        "m1", [rec("cur")], vectors=[[1.0]], embedding_model="e", embedding_version="2"
    )
    await store.put_memory(
        "m1", [rec("old")], vectors=[[1.0]], embedding_model="e", embedding_version="1"
    )
    await store.put_memory("m1", [rec("lex")])  # stored while the dense channel was down
    await store.put_memory(
        "m2", [rec("other")], vectors=[[1.0]], embedding_model="f", embedding_version="2"
    )
    await store.put_memory("m1", [rec("gone")])
    await store.invalidate_memory(["gone"])  # soft-forgotten rows are never re-embedded

    stale = await store.stale_memory("m1", embedding_model="e", embedding_version="2")
    assert [(m, r.id) for m, r in stale] == [("m1", "old"), ("m1", "lex")]
    everyone = await store.stale_memory(None, embedding_model="e", embedding_version="2", limit=2)
    assert [r.id for _, r in everyone] == ["old", "lex"]
    assert await store.count_stale_memory(None, embedding_model="e", embedding_version="2") == {
        "m1": 2,
        "m2": 1,
    }
    assert await store.count_stale_memory("m2", embedding_model="f", embedding_version="2") == {}


# --- skills -----------------------------------------------------------------------------------
async def test_skills_round_trip_namespaced_and_verified_only(store: SqliteStore) -> None:
    skill = Skill(
        id="s1",
        name="add endpoint",
        description="add a REST endpoint",
        code="summary: ...",
        preconditions=["fastapi installed"],
        namespace="/repo/a",
        provenance="m1:c3:abc",
        expires_at="2099-01-01",
        verified=True,
    )
    await store.put_skill(skill)
    await store.put_skill(
        Skill(id="g", name="g", description="global", code="", namespace="global", verified=True)
    )
    await store.put_skill(
        Skill(
            id="o", name="o", description="other repo", code="", namespace="/repo/b", verified=True
        )
    )
    with pytest.raises(SkillNotVerifiedError):
        await store.put_skill(Skill(id="u", name="u", description="unverified", code=""))

    got = await store.list_skills("/repo/a")
    assert {s.id for s in got} == {"s1", "g"}
    assert next(s for s in got if s.id == "s1") == skill


# --- factory / fallback -----------------------------------------------------------------------
def test_sqlite_path_is_relocated_out_of_the_mission_checkout(tmp_path: Path) -> None:
    ws = tmp_path / "ws"
    inside = Settings(_env_file=None, sqlite_path=str(ws / ".lha" / "lha.sqlite3"))  # type: ignore[call-arg]
    assert resolve_sqlite_path(inside, workdir=ws) == ws.resolve() / ".git" / "lha" / "lha.sqlite3"
    outside = Settings(_env_file=None, sqlite_path=str(tmp_path / "x.sqlite3"))  # type: ignore[call-arg]
    assert resolve_sqlite_path(outside, workdir=ws) == (tmp_path / "x.sqlite3").resolve()


def test_default_sqlite_path_is_per_user_and_shared(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import lha.persistence.store as store_mod

    monkeypatch.setattr(Path, "home", classmethod(lambda _cls: tmp_path / "home"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "xdg"))
    assert default_sqlite_path() == tmp_path / "xdg" / "lha" / "lha.sqlite3"
    monkeypatch.setenv("XDG_DATA_HOME", "relative/ignored")  # XDG says: ignore relative
    monkeypatch.setattr(store_mod.sys, "platform", "darwin")
    assert default_sqlite_path() == (
        tmp_path / "home" / "Library" / "Application Support" / "lha" / "lha.sqlite3"
    )
    monkeypatch.delenv("XDG_DATA_HOME")
    monkeypatch.setattr(store_mod.sys, "platform", "linux")
    assert default_sqlite_path() == tmp_path / "home" / ".local" / "share" / "lha" / "lha.sqlite3"
    monkeypatch.setattr(store_mod.sys, "platform", "win32")
    monkeypatch.setenv("LOCALAPPDATA", str(tmp_path / "appdata"))
    assert default_sqlite_path() == tmp_path / "appdata" / "lha" / "lha.sqlite3"


def test_unset_sqlite_path_uses_the_default_wherever_the_process_runs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "xdg"))
    monkeypatch.delenv("LHA_SQLITE_PATH")
    settings = Settings(_env_file=None)  # type: ignore[call-arg]
    assert settings.sqlite_path == ""
    expected = tmp_path / "xdg" / "lha" / "lha.sqlite3"
    for cwd in (tmp_path, tmp_path / "elsewhere"):
        cwd.mkdir(exist_ok=True)
        monkeypatch.chdir(cwd)
        assert resolve_sqlite_path(settings) == expected
    assert describe_store(settings) == f"sqlite {expected}"


def test_relative_sqlite_path_resolves_against_cwd_and_warns_once(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import lha.persistence.store as store_mod

    warnings: list[dict[str, object]] = []

    class _Log:
        def warning(self, event: str, **kw: object) -> None:
            warnings.append({"event": event, **kw})

    monkeypatch.setattr(store_mod, "get_logger", lambda _name: _Log())
    monkeypatch.setattr(store_mod, "_warned_relative", set())
    monkeypatch.chdir(tmp_path)
    settings = Settings(_env_file=None, sqlite_path="rel/x.sqlite3")  # type: ignore[call-arg]
    assert resolve_sqlite_path(settings) == (tmp_path / "rel" / "x.sqlite3").resolve()
    assert resolve_sqlite_path(settings) == (tmp_path / "rel" / "x.sqlite3").resolve()
    assert [w["event"] for w in warnings] == ["sqlite_path_relative"]
    # A relative path inside a mission checkout is still moved under .git/lha/.
    ws = tmp_path / "rel"
    assert resolve_sqlite_path(settings, workdir=ws) == ws.resolve() / ".git" / "lha" / "x.sqlite3"


def test_describe_store_names_postgres_and_its_fallback(tmp_path: Path) -> None:
    dsn = SecretStr("postgresql://u:p@h/db")
    path = tmp_path / "f.sqlite3"
    on = Settings(_env_file=None, postgres_dsn=dsn, sqlite_path=str(path))  # type: ignore[call-arg]
    assert describe_store(on) == f"postgres (LHA_POSTGRES_DSN) (falls back to SQLite at {path})"
    off = on.model_copy(update={"postgres_fallback_to_sqlite": False})
    assert describe_store(off) == "postgres (LHA_POSTGRES_DSN)"
    assert "u:p" not in describe_store(on)


async def test_open_store_defaults_to_sqlite(tmp_path: Path) -> None:
    settings = Settings(_env_file=None, sqlite_path=str(tmp_path / "s.sqlite3"))  # type: ignore[call-arg]
    store = await open_store(settings)
    try:
        assert store.backend == BACKEND_SQLITE and store.degraded_reason == ""
        assert (tmp_path / "s.sqlite3").exists()
    finally:
        await store.close()


async def test_unusable_postgres_falls_back_to_sqlite_or_fails(tmp_path: Path) -> None:
    # Nothing listens on port 1 (and psycopg may not even be installed): Postgres is unusable.
    dsn = SecretStr("postgresql://u:p@127.0.0.1:1/none?connect_timeout=1")
    fallback = Settings(_env_file=None, postgres_dsn=dsn, sqlite_path=str(tmp_path / "f.sqlite3"))  # type: ignore[call-arg]
    store = await open_store(fallback)
    try:
        assert store.backend == BACKEND_SQLITE
        assert store.degraded_reason.startswith("postgres unavailable")
    finally:
        await store.close()

    strict = fallback.model_copy(update={"postgres_fallback_to_sqlite": False})
    with pytest.raises(StoreUnavailableError, match="postgres unavailable"):
        await open_store(strict)


# --- ledger sink / mission tracker ------------------------------------------------------------
class _Unpriced(StubModel):
    def estimate_cost_usd(self, usage: Usage) -> float:
        from lha.contracts.model import UnknownPriceError

        raise UnknownPriceError("no price")


def _meter() -> CostMeter:
    governor = BudgetGovernor(ceiling_usd=10.0, max_cycles=100, allow_unknown_cost=True)
    return CostMeter(ledger=CostLedger(), governor=governor)


async def test_every_metered_call_reaches_the_persistent_ledger(store: SqliteStore) -> None:
    meter = _meter()
    msg = [ModelMessage(role="user", content="hi")]
    planner = meter.wrap(StubModel(), role="planner")
    await planner.complete(msg)  # before the sink exists (e.g. the Planner's call)

    sink = LedgerSink(store, "m1", key_prefix="run")
    await sink.backfill(list(meter.ledger.entries))
    sink.attach(meter)
    meter.cycle_id = "c1"
    await meter.wrap(StubModel(), role="lead").complete(msg)
    await meter.wrap(_Unpriced(script=[TurnResult(text="x")]), role="researcher").complete(msg)

    rows = await store.list_costs("m1")
    assert [(r.cycle_id, r.role) for r in rows] == [
        ("c0", "planner"),
        ("c1", "lead"),
        ("c1", "researcher"),
    ]
    assert rows[2].usd is None and not rows[2].cost_known
    assert len(rows) == len(meter.ledger.entries) and sink.written == 3

    # A replayed write of the same logical calls is a no-op.
    replay = LedgerSink(store, "m1", key_prefix="run")
    await replay.backfill(list(meter.ledger.entries))
    assert replay.written == 0 and len(await store.list_costs("m1")) == 3


async def test_ledger_and_tracker_failures_never_fail_the_run() -> None:
    class _Broken(SqliteStore):
        async def record_cost(self, *a: object, **k: object) -> bool:
            raise sqlite3.OperationalError("disk I/O error")

        async def upsert_mission(self, **k: object) -> None:
            raise sqlite3.OperationalError("disk I/O error")

    broken = _Broken(":memory:")
    meter = _meter()
    sink = LedgerSink(broken, "m1")
    sink.attach(meter)
    result = await meter.wrap(StubModel(), role="lead").complete(
        [ModelMessage(role="user", content="hi")]
    )
    assert result.text and sink.failures == 1 and len(meter.ledger.entries) == 1

    tracker = MissionTracker(broken, "m1")
    await tracker.running()
    assert tracker.failures == 1


async def test_meter_survives_a_hook_that_raises() -> None:
    meter = _meter()

    async def boom(_entry: CostEntry) -> None:
        raise RuntimeError("hook exploded")

    meter.on_record = boom
    result = await meter.wrap(StubModel(), role="lead").complete(
        [ModelMessage(role="user", content="hi")]
    )
    assert result.text and len(meter.ledger.entries) == 1


@pytest.mark.parametrize(
    ("stopped", "status"),
    [
        ("complete", "DONE"),
        ("deadlocked: blocked: 01", "IMPOSSIBLE"),
        ("governor: budget", "ABORTED"),
        ("loop on item 01", "ABORTED"),
        ("max_cycles", "ABORTED"),
        ("error: RuntimeError", "ABORTED"),
    ],
)
def test_status_for_stop_mirrors_the_workflow(stopped: str, status: str) -> None:
    assert status_for_stop(stopped) == status


async def test_tracker_records_transitions(store: SqliteStore) -> None:
    tracker = MissionTracker(store, "m1", title="T", description="D", workflow_id="mission:m1")
    await tracker.running()
    assert (await store.get_mission("m1")).status == "RUNNING"  # type: ignore[union-attr]
    assert await tracker.finish("complete", head_sha="f00") == "DONE"
    row = await store.get_mission("m1")
    assert row is not None
    assert (row.status, row.head_sha, row.workflow_id, row.title) == (
        "DONE",
        "f00",
        "mission:m1",
        "T",
    )


def test_concurrent_first_opens_succeed(tmp_path: Path) -> None:
    """Several stores open and write one fresh file at once (two processes starting on a shared
    store): the WAL conversion and schema creation race is retried, not failed."""
    import threading

    path = tmp_path / "shared" / "lha.sqlite3"
    errors: list[BaseException] = []
    go = threading.Barrier(12)

    def worker(i: int) -> None:
        try:
            go.wait()
            s = SqliteStore(path)
            s._open_sync()
            try:
                asyncio.run(s.upsert_mission(mission_id=f"m{i}", title="t", status="RUNNING"))
            finally:
                asyncio.run(s.close())
        except BaseException as exc:  # collected and asserted below
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(12)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert errors == []
    s = SqliteStore(path)
    s._open_sync()
    try:
        assert len(asyncio.run(s.list_missions(limit=50))) == 12
    finally:
        asyncio.run(s.close())
