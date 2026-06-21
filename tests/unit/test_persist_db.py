"""Migrations runner + Postgres repositories, exercised against fakes (no Postgres needed)."""

from __future__ import annotations

import sys
import types
from pathlib import Path
from typing import Any

import pytest

from lha.persistence.db import MIGRATION_LOCK_KEY, apply_pending, discover_migrations

REPO_MIGRATIONS = Path(__file__).resolve().parents[2] / "db" / "migrations"


class _Cursor:
    def __init__(self, rows: list[tuple[Any, ...]], rowcount: int = 1) -> None:
        self._rows = rows
        self.rowcount = rowcount

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self._rows


class _Tx:
    def __init__(self, conn: FakeConn) -> None:
        self._conn = conn

    async def __aenter__(self) -> None:
        self._conn.log.append(("BEGIN", None))

    async def __aexit__(self, exc_type: object, *_: object) -> bool:
        self._conn.log.append(("ROLLBACK" if exc_type else "COMMIT", None))
        if exc_type is None:
            self._conn.applied |= self._conn.staged
        self._conn.staged = set()
        return False


class FakeConn:
    """Records statements; emulates schema_migrations + transactional visibility."""

    def __init__(self, applied: set[str] | None = None, fail_on: str | None = None) -> None:
        self.log: list[tuple[str, Any]] = []
        self.applied: set[str] = set(applied or ())
        self.staged: set[str] = set()
        self.fail_on = fail_on

    async def execute(self, query: Any, params: Any = None) -> _Cursor:
        self.log.append((str(query), params))
        if self.fail_on and self.fail_on in str(query):
            raise RuntimeError("syntax error in migration")
        if str(query).startswith("SELECT version FROM schema_migrations"):
            return _Cursor([(v,) for v in sorted(self.applied)])
        if str(query).startswith("INSERT INTO schema_migrations"):
            self.staged.add(params[0])
        return _Cursor([])

    def transaction(self) -> _Tx:
        return _Tx(self)


def _write(tmp: Path, name: str, sql: str) -> None:
    (tmp / name).write_text(sql, encoding="utf-8")


def test_discover_orders_by_name_and_versions_are_stems(tmp_path: Path) -> None:
    _write(tmp_path, "0002_b.sql", "SELECT 2;")
    _write(tmp_path, "0001_a.sql", "SELECT 1;")
    assert [m.version for m in discover_migrations(tmp_path)] == ["0001_a", "0002_b"]
    with pytest.raises(FileNotFoundError):
        discover_migrations(tmp_path / "missing")


@pytest.mark.asyncio
async def test_apply_runs_each_file_unsplit_in_a_transaction_and_records_it(
    tmp_path: Path,
) -> None:
    sql = "-- a comment; with a semicolon\nCREATE TABLE t (x text DEFAULT 'a;b');\nSELECT 1;"
    _write(tmp_path, "0001_init.sql", sql)
    conn = FakeConn()
    applied = await apply_pending(conn, discover_migrations(tmp_path))
    assert applied == ["0001_init"]
    statements = [q for q, _ in conn.log]
    assert sql in statements  # the whole script as ONE batch, never split on ';'
    begin, run, record, commit = statements[3:7]
    assert (begin, run, commit) == ("BEGIN", sql, "COMMIT")
    assert record.startswith("INSERT INTO schema_migrations")
    # Advisory lock taken first and released last.
    assert conn.log[0] == ("SELECT pg_advisory_lock(%s)", (MIGRATION_LOCK_KEY,))
    assert conn.log[-1] == ("SELECT pg_advisory_unlock(%s)", (MIGRATION_LOCK_KEY,))


@pytest.mark.asyncio
async def test_apply_skips_already_applied(tmp_path: Path) -> None:
    _write(tmp_path, "0001_init.sql", "SELECT 1;")
    _write(tmp_path, "0002_next.sql", "SELECT 2;")
    conn = FakeConn(applied={"0001_init"})
    assert await apply_pending(conn, discover_migrations(tmp_path)) == ["0002_next"]
    assert await apply_pending(conn, discover_migrations(tmp_path)) == []  # idempotent


@pytest.mark.asyncio
async def test_failed_migration_is_not_recorded_and_lock_released(tmp_path: Path) -> None:
    _write(tmp_path, "0001_ok.sql", "SELECT 1;")
    _write(tmp_path, "0002_bad.sql", "BROKEN SQL;")
    conn = FakeConn(fail_on="BROKEN")
    with pytest.raises(RuntimeError):
        await apply_pending(conn, discover_migrations(tmp_path))
    assert conn.applied == {"0001_ok"}
    assert ("ROLLBACK", None) in conn.log
    assert conn.log[-1][0] == "SELECT pg_advisory_unlock(%s)"


def test_repo_migrations_self_record_for_initdb() -> None:
    """Each shipped migration inserts its own version, so a docker-entrypoint-initdb.d init is
    recognized by ``apply_migrations`` as already applied."""
    migrations = discover_migrations(REPO_MIGRATIONS)
    assert [m.version for m in migrations][:2] == ["0001_init", "0002_idempotent_ledger"]
    for m in migrations:
        text = m.sql()
        assert "CREATE TABLE IF NOT EXISTS schema_migrations" in text
        assert f"VALUES ('{m.version}') ON CONFLICT DO NOTHING" in text
    ledger = migrations[1].sql()
    assert "cost_ledger_idempotency_key" in ledger
    assert "ALTER COLUMN id TYPE text" in ledger


# --- repositories (fake psycopg module) ---------------------------------------------------
class _RepoConn:
    def __init__(self, sink: list[tuple[str, Any]], rowcount: int) -> None:
        self._sink = sink
        self._rowcount = rowcount

    async def __aenter__(self) -> _RepoConn:
        return self

    async def __aexit__(self, *_: object) -> None:
        return None

    async def execute(self, query: str, params: Any = None) -> _Cursor:
        self._sink.append((query, params))
        return _Cursor([], rowcount=self._rowcount)


@pytest.fixture
def fake_psycopg(monkeypatch: pytest.MonkeyPatch) -> list[tuple[str, Any]]:
    sink: list[tuple[str, Any]] = []
    rowcounts = iter([1, 0])

    class AsyncConnection:
        @staticmethod
        async def connect(dsn: str, autocommit: bool = False) -> _RepoConn:
            return _RepoConn(sink, next(rowcounts, 0))

    module = types.ModuleType("psycopg")
    module.AsyncConnection = AsyncConnection  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "psycopg", module)
    monkeypatch.delitem(sys.modules, "lha.persistence.repositories", raising=False)
    return sink


@pytest.mark.asyncio
async def test_cost_ledger_record_is_idempotent(fake_psycopg: list[tuple[str, Any]]) -> None:
    from lha.governor.cost import CostEntry
    from lha.persistence.repositories import CostLedgerRepo

    repo = CostLedgerRepo("postgresql://fake")
    entry = CostEntry(cycle_id="c1", model="m", input_tokens=1, output_tokens=2, usd=0.5)
    assert await repo.record("m1", entry, call_key=0) is True
    assert await repo.record("m1", entry, call_key=0) is False  # retry: conflict, no-op
    await repo.record("m1", entry, call_key=1)
    (q1, p1), (_, p2), (_, p3) = fake_psycopg
    assert "ON CONFLICT (idempotency_key) DO NOTHING" in q1
    assert p1[-1] == p2[-1]  # same logical call -> same key
    assert p3[-1] != p1[-1]  # a different call in the same cycle is a different row


@pytest.mark.asyncio
async def test_mission_upsert_keeps_head_sha(fake_psycopg: list[tuple[str, Any]]) -> None:
    from lha.persistence.repositories import MissionRepo

    await MissionRepo("postgresql://fake").upsert(mission_id="m1", title="t", status="RUNNING")
    (query, params) = fake_psycopg[0]
    assert "COALESCE(EXCLUDED.head_sha, missions.head_sha)" in query
    assert params[-1] is None
