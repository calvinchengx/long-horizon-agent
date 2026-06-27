"""Migrations, the cost ledger, missions and pgvector memory against a real Postgres."""

from __future__ import annotations

from pathlib import Path

import pytest

from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.embeddings import HashEmbedder
from tests.integration.conftest import requires_postgres

pytestmark = [pytest.mark.integration, requires_postgres]
psycopg = pytest.importorskip("psycopg")
pytest.importorskip("pgvector")

MIGRATIONS = Path(__file__).resolve().parents[3] / "db" / "migrations"
ALL_VERSIONS = sorted(p.stem for p in MIGRATIONS.glob("*.sql"))


async def _migrate(dsn: str) -> list[str]:
    from lha.persistence.db import apply_migrations

    return await apply_migrations(dsn, migrations_dir=MIGRATIONS)


async def _scalar(dsn: str, sql: str, params: tuple[object, ...] = ()) -> object:
    async with await psycopg.AsyncConnection.connect(dsn, autocommit=True) as conn:
        cursor = await conn.execute(sql, params)
        row = await cursor.fetchone()
        return row[0] if row else None


# --- migrations -------------------------------------------------------------------------------


async def test_migrations_apply_in_order_then_are_a_noop(pg_dsn: str) -> None:
    assert await _migrate(pg_dsn) == ALL_VERSIONS
    assert await _migrate(pg_dsn) == []
    count = await _scalar(pg_dsn, "SELECT count(*) FROM schema_migrations")
    assert count == len(ALL_VERSIONS)


async def test_schema_after_migrations(pg_dsn: str) -> None:
    await _migrate(pg_dsn)
    column = "SELECT {} FROM information_schema.columns WHERE table_name = %s AND column_name = %s"
    # semantic_memory ids are the app's prefixed string ids (``fact_...``), not uuids.
    assert await _scalar(pg_dsn, column.format("data_type"), ("semantic_memory", "id")) == "text"
    # An unknown cost is stored as NULL, so usd must be nullable with no $0 default.
    assert await _scalar(pg_dsn, column.format("is_nullable"), ("cost_ledger", "usd")) == "YES"
    assert await _scalar(pg_dsn, column.format("column_default"), ("cost_ledger", "usd")) is None


# --- cost ledger ------------------------------------------------------------------------------


def _entry(cycle: str, *, usd: float, known: bool = True) -> CostEntry:
    return CostEntry(
        cycle_id=cycle, model="m", input_tokens=10, output_tokens=5, usd=usd, cost_known=known
    )


async def test_cost_ledger_is_idempotent_and_keeps_unknown_cost_null(pg_dsn: str) -> None:
    from lha.persistence.repositories import CostLedgerRepo

    await _migrate(pg_dsn)
    repo = CostLedgerRepo(pg_dsn)
    assert await repo.record("m1", _entry("c1", usd=0.25), call_key=0) is True
    assert await repo.record("m1", _entry("c1", usd=0.25), call_key=0) is False  # replayed write
    assert await repo.record("m1", _entry("c1", usd=0.0, known=False), call_key=1) is True
    assert await repo.record("m1", _entry("c2", usd=0.5), call_key=0) is True

    assert await repo.total_usd("m1") == pytest.approx(0.75)  # known spend only
    assert await repo.unknown_cost_calls("m1") == 1
    unknown_usd = await _scalar(
        pg_dsn, "SELECT usd FROM cost_ledger WHERE mission_id = %s AND NOT cost_known", ("m1",)
    )
    assert unknown_usd is None  # never recorded as $0


# --- missions ---------------------------------------------------------------------------------


async def test_mission_upsert_without_sha_keeps_the_stored_sha(pg_dsn: str) -> None:
    from lha.persistence.repositories import MissionRepo

    await _migrate(pg_dsn)
    repo = MissionRepo(pg_dsn)
    await repo.upsert(mission_id="m1", title="t", status="running", head_sha="abc123")
    await repo.upsert(mission_id="m1", title="t", status="parked")
    assert await repo.get_status("m1") == "parked"
    sha = await _scalar(pg_dsn, "SELECT head_sha FROM missions WHERE mission_id = %s", ("m1",))
    assert sha == "abc123"


# --- pgvector semantic memory -----------------------------------------------------------------


def _fact(fid: str, text: str) -> MemoryRecord:
    return MemoryRecord(id=fid, kind="semantic", text=text, metadata={"mission_id": "m1"})


async def test_semantic_index_stores_app_ids_and_ranks_by_similarity(pg_dsn: str) -> None:
    from lha.memory.semantic_pg import PgSemanticIndex

    await _migrate(pg_dsn)
    index = PgSemanticIndex(dsn=pg_dsn, embedder=HashEmbedder(dim=1024))
    try:
        await index.add(
            [
                _fact("fact_parser", "the parser rejects trailing commas in json arrays"),
                _fact("fact_cache", "the http cache stores etags for conditional requests"),
                _fact("fact_auth", "tokens are rotated every hour by the auth service"),
            ]
        )
        hits = await index.query("json parser trailing commas", k=3)
        assert hits[0].record.id == "fact_parser"
        scores = [h.score for h in hits]
        assert scores == sorted(scores, reverse=True)  # most similar first
        assert 0.0 < scores[0] <= 1.0 + 1e-6

        # Re-adding an id upserts instead of duplicating.
        await index.add([_fact("fact_parser", "the parser accepts trailing commas now")])
        rows = await _scalar(pg_dsn, "SELECT count(*) FROM semantic_memory")
        assert rows == 3
    finally:
        await index.close()


async def test_semantic_index_never_compares_across_embedder_versions(pg_dsn: str) -> None:
    from lha.memory.semantic_pg import PgSemanticIndex

    await _migrate(pg_dsn)
    old = HashEmbedder(dim=1024)
    new = HashEmbedder(dim=1024)
    new.version = "2"
    old_index = PgSemanticIndex(dsn=pg_dsn, embedder=old)
    new_index = PgSemanticIndex(dsn=pg_dsn, embedder=new)
    try:
        await old_index.add([_fact("fact_old", "retry budget is five attempts")])
        assert await new_index.query("retry budget", k=5) == []
        assert [h.record.id for h in await old_index.query("retry budget", k=5)] == ["fact_old"]
    finally:
        await old_index.close()
        await new_index.close()
