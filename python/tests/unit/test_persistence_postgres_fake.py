"""``PostgresStore`` against a fake psycopg connection: SQL parameters, NULL-for-unknown cost, the
migration gate on open, and row mapping. (The real-database run is tests/integration.)"""

from __future__ import annotations

import sys
import types
from datetime import UTC, datetime
from typing import Any

import pytest

from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill, SkillNotVerifiedError
from lha.persistence.postgres import PostgresStore
from lha.persistence.store import REQUIRED_PG_MIGRATIONS, StoreUnavailableError

_TS = datetime(2026, 1, 2, 3, 4, 5, tzinfo=UTC)


class _Cursor:
    def __init__(self, rows: list[tuple[Any, ...]], rowcount: int = 1) -> None:
        self._rows = rows
        self.rowcount = rowcount

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self._rows

    async def fetchone(self) -> tuple[Any, ...] | None:
        return self._rows[0] if self._rows else None


class _Conn:
    """Records every statement; answers from a queue of canned results."""

    def __init__(self, results: list[_Cursor] | None = None) -> None:
        self.calls: list[tuple[str, Any]] = []
        self.results = list(results or [])
        self.closed = False

    async def execute(self, sql: str, params: Any = ()) -> _Cursor:
        self.calls.append((sql, params))
        return self.results.pop(0) if self.results else _Cursor([])

    async def close(self) -> None:
        self.closed = True


def _store(*results: _Cursor) -> tuple[PostgresStore, _Conn]:
    store = PostgresStore("postgresql://fake")
    conn = _Conn(list(results))
    store._conn = conn
    return store, conn


def _fake_psycopg(monkeypatch: pytest.MonkeyPatch, conn: _Conn) -> None:
    module = types.ModuleType("psycopg")

    class AsyncConnection:
        @staticmethod
        async def connect(*_a: object, **_k: object) -> _Conn:
            return conn

    module.AsyncConnection = AsyncConnection  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "psycopg", module)


async def test_open_requires_every_migration(monkeypatch: pytest.MonkeyPatch) -> None:
    partial = _Conn([_Cursor([(True,)]), _Cursor([("0001_init",)])])
    _fake_psycopg(monkeypatch, partial)
    with pytest.raises(StoreUnavailableError, match="lha db migrate"):
        await PostgresStore("postgresql://fake").open()
    assert partial.closed

    empty = _Conn([_Cursor([(False,)])])  # no schema_migrations table at all
    _fake_psycopg(monkeypatch, empty)
    with pytest.raises(StoreUnavailableError):
        await PostgresStore("postgresql://fake").open()

    full = _Conn([_Cursor([(True,)]), _Cursor([(v,) for v in REQUIRED_PG_MIGRATIONS])])
    _fake_psycopg(monkeypatch, full)
    store = PostgresStore("postgresql://fake")
    await store.open()
    await store.open()  # idempotent
    await store.close()
    assert full.closed


async def test_missions_sql_and_mapping() -> None:
    row = ("m1", "T", "DONE", "D", "abc", "wf", _TS, _TS)
    store, conn = _store(_Cursor([]), _Cursor([row]), _Cursor([]), _Cursor([row]))
    await store.upsert_mission(mission_id="m1", title="T", status="DONE", head_sha="abc")
    sql, params = conn.calls[0]
    assert "ON CONFLICT (mission_id)" in sql and "COALESCE(NULLIF(EXCLUDED.head_sha" in sql
    assert params == ("m1", "T", "", "DONE", "abc", None)
    got = await store.get_mission("m1")
    assert got is not None and got.head_sha == "abc" and got.updated_at.startswith("2026-01-02")
    assert await store.get_mission("missing") is None
    assert [m.mission_id for m in await store.list_missions(limit=5)] == ["m1"]


async def test_cost_rows_store_null_for_unknown_cost() -> None:
    store, conn = _store(_Cursor([], rowcount=1), _Cursor([], rowcount=0))
    unknown = CostEntry(
        cycle_id="c1", model="m", input_tokens=1, output_tokens=2, usd=0.0, cost_known=False
    )
    assert await store.record_cost("m1", unknown, call_key="k#0") is True
    assert await store.record_cost("m1", unknown, call_key="k#0") is False
    params = conn.calls[0][1]
    assert params[5] is None and params[7] is False  # usd NULL, cost_known false
    assert params[8] == conn.calls[1][1][8]  # same logical call → same idempotency key

    rows = [(1, "m1", "c1", "m", "lead", 1, 2, None, False, _TS)]
    store, _ = _store(_Cursor(rows), _Cursor([(1, 0.0, 1, 1, 2)]))
    listed = await store.list_costs("m1")
    assert listed[0].usd is None and listed[0].role == "lead"
    summary = await store.cost_summary("m1")
    assert (summary.calls, summary.unknown_cost_calls) == (1, 1)


async def test_events_memory_and_skills_sql() -> None:
    event_row = (7, "m1", "c1", "cycle_outcome", {"item_id": "01"}, _TS)
    mem_row = ("a", "fact", "alpha", {"k": "v"}, "hash", "1", True, _TS)
    skill_row = ("s", "n", "d", "c", ["pre"], "/r", "p", None, True, 0)
    store, conn = _store(
        _Cursor([(7,)]),
        _Cursor([event_row]),
        _Cursor([]),
        _Cursor([mem_row]),
        _Cursor([], rowcount=1),
        _Cursor([]),
        _Cursor([skill_row]),
    )
    assert (
        await store.append_event("m1", cycle_id="c1", kind="cycle_outcome", payload={"a": 1}) == 7
    )
    events = await store.list_events("m1", kinds=("cycle_outcome",), after_id=3, limit=5)
    assert events[0].payload == {"item_id": "01"} and events[0].id == 7
    assert "kind = ANY(%s)" in conn.calls[1][0] and conn.calls[1][1] == [
        "m1",
        3,
        ["cycle_outcome"],
        5,
    ]

    await store.put_memory("m1", [MemoryRecord(id="a", kind="fact", text="alpha")])
    memory = await store.list_memory("m1")
    assert memory[0].metadata == {"k": "v"} and memory[0].kind == "fact"
    assert await store.invalidate_memory(["a"]) == 1
    assert await store.invalidate_memory([]) == 0
    with pytest.raises(ValueError, match="PgSemanticIndex"):
        await store.put_memory("m1", [MemoryRecord(id="a", kind="f", text="t")], vectors=[[1.0]])

    await store.put_skill(Skill(id="s", name="n", description="d", code="c", verified=True))
    with pytest.raises(SkillNotVerifiedError):
        await store.put_skill(Skill(id="u", name="n", description="d", code="c"))
    skills = await store.list_skills("/r")
    assert skills[0].preconditions == ["pre"] and skills[0].verified
