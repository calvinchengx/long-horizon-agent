"""The SQLite mission store, the store factory/fallback, and the ledger/mission plumbing."""

from __future__ import annotations

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
    MissionStore,
    StoreUnavailableError,
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
    assert versions == ["sqlite_0001_init"]
    assert {"missions", "cost_ledger", "episodic_events", "semantic_memory", "skills"} <= tables

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
    await store.upsert_mission(mission_id="m1", title="", status="RUNNING", head_sha="abc")
    await store.upsert_mission(mission_id="m1", title="", status="DONE")  # head_sha=None keeps
    row = await store.get_mission("m1")
    assert row is not None
    assert (row.title, row.description, row.status, row.head_sha) == ("T", "D", "DONE", "abc")
    assert row.created_at and row.updated_at >= row.created_at

    await store.upsert_mission(mission_id="m2", title="second", status="RUNNING")
    assert [m.mission_id for m in await store.list_missions()] == ["m2", "m1"]
    assert [m.mission_id for m in await store.list_missions(limit=1)] == ["m2"]


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
