"""Postgres backend of ``MissionStore`` (requires the ``postgres`` extra and ``lha db migrate``).

Uses the tables from ``db/migrations`` (0001-0004); psycopg is imported only when a connection is
opened. The cost-ledger insert and its idempotency key are the same as ``CostLedgerRepo``'s, so
rows written by either are interchangeable. One autocommit connection per store, opened by
``open()`` (which also verifies that the required migrations are applied) and serialized with a
lock.

Semantic-memory VECTORS are written/queried by ``PgSemanticIndex`` (pgvector); this store writes
rows without an embedding (lexical-only mode) and lists row text for BM25.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill, SkillNotVerifiedError
from lha.persistence.sqlite import cost_idempotency_key
from lha.persistence.store import (
    BACKEND_POSTGRES,
    REQUIRED_PG_MIGRATIONS,
    CostRow,
    CostSummary,
    EventRow,
    MissionRow,
    StoreUnavailableError,
)

# Shared with ``CostLedgerRepo`` (``repositories.py``), so rows written by either are identical.
INSERT_COST_SQL = (
    "INSERT INTO cost_ledger "
    "(mission_id, cycle_id, model, input_tokens, output_tokens, usd, role, cost_known, "
    "idempotency_key) "
    "VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) "
    "ON CONFLICT (idempotency_key) DO NOTHING"
)


def _ts(value: object) -> str:
    iso = getattr(value, "isoformat", None)
    return str(iso()) if callable(iso) else str(value or "")


class PostgresStore:
    """``MissionStore`` on Postgres (+ pgvector for dense memory via ``PgSemanticIndex``)."""

    backend = BACKEND_POSTGRES

    def __init__(self, dsn: str, *, connect_timeout_s: int = 5) -> None:
        self.dsn = dsn
        self.degraded_reason = ""
        self._timeout = connect_timeout_s
        self._conn: Any | None = None
        self._lock = asyncio.Lock()

    async def open(self) -> None:
        import psycopg

        if self._conn is not None:
            return
        conn = await psycopg.AsyncConnection.connect(
            self.dsn, autocommit=True, connect_timeout=self._timeout
        )
        try:
            cursor = await conn.execute("SELECT to_regclass('schema_migrations') IS NOT NULL")
            row = await cursor.fetchone()
            applied: set[str] = set()
            if row and row[0]:
                cursor = await conn.execute("SELECT version FROM schema_migrations")
                applied = {str(r[0]) for r in await cursor.fetchall()}
            missing = [v for v in REQUIRED_PG_MIGRATIONS if v not in applied]
            if missing:
                raise StoreUnavailableError(
                    f"postgres schema is missing migrations {missing}; run `lha db migrate`"
                )
        except BaseException:
            await conn.close()
            raise
        self._conn = conn

    async def _execute(self, sql: str, params: tuple[Any, ...] | list[Any] = ()) -> Any:
        async with self._lock:
            if self._conn is None:
                await self.open()
            assert self._conn is not None
            return await self._conn.execute(sql, params)

    async def _fetchall(self, sql: str, params: tuple[Any, ...] | list[Any] = ()) -> list[Any]:
        async with self._lock:
            if self._conn is None:
                await self.open()
            assert self._conn is not None
            cursor = await self._conn.execute(sql, params)
            return list(await cursor.fetchall())

    async def close(self) -> None:
        async with self._lock:
            if self._conn is not None:
                await self._conn.close()
                self._conn = None

    # --- missions -----------------------------------------------------------------------
    async def upsert_mission(
        self,
        *,
        mission_id: str,
        title: str,
        status: str,
        description: str = "",
        head_sha: str | None = None,
        workflow_id: str | None = None,
    ) -> None:
        await self._execute(
            "INSERT INTO missions (mission_id, title, description, status, head_sha, workflow_id) "
            "VALUES (%s, %s, %s, %s, %s, %s) "
            "ON CONFLICT (mission_id) DO UPDATE SET "
            "title = COALESCE(NULLIF(EXCLUDED.title, ''), missions.title), "
            "description = COALESCE(NULLIF(EXCLUDED.description, ''), missions.description), "
            "status = EXCLUDED.status, "
            "head_sha = COALESCE(NULLIF(EXCLUDED.head_sha, ''), missions.head_sha), "
            "workflow_id = COALESCE(EXCLUDED.workflow_id, missions.workflow_id), "
            "updated_at = now()",
            (mission_id, title, description, status, head_sha, workflow_id),
        )

    _MISSION_COLS = (
        "mission_id, title, status, description, head_sha, workflow_id, created_at, updated_at"
    )

    @staticmethod
    def _mission(row: Any) -> MissionRow:
        return MissionRow(
            mission_id=str(row[0]),
            title=str(row[1]),
            status=str(row[2]),
            description=str(row[3] or ""),
            head_sha=row[4],
            workflow_id=row[5],
            created_at=_ts(row[6]),
            updated_at=_ts(row[7]),
        )

    async def get_mission(self, mission_id: str) -> MissionRow | None:
        rows = await self._fetchall(
            f"SELECT {self._MISSION_COLS} FROM missions WHERE mission_id = %s", (mission_id,)
        )
        return self._mission(rows[0]) if rows else None

    async def list_missions(self, *, limit: int = 20) -> list[MissionRow]:
        rows = await self._fetchall(
            f"SELECT {self._MISSION_COLS} FROM missions "
            "ORDER BY updated_at DESC, mission_id LIMIT %s",
            (limit,),
        )
        return [self._mission(r) for r in rows]

    # --- cost ledger --------------------------------------------------------------------
    async def record_cost(self, mission_id: str, entry: CostEntry, *, call_key: str) -> bool:
        cursor = await self._execute(
            INSERT_COST_SQL,
            (
                mission_id,
                entry.cycle_id,
                entry.model,
                entry.input_tokens,
                entry.output_tokens,
                entry.usd if entry.cost_known else None,
                entry.role,
                entry.cost_known,
                cost_idempotency_key(mission_id, entry.cycle_id, call_key),
            ),
        )
        return bool(cursor.rowcount)

    async def list_costs(self, mission_id: str, *, limit: int = 200) -> list[CostRow]:
        rows = await self._fetchall(
            "SELECT * FROM (SELECT id, mission_id, cycle_id, model, role, input_tokens, "
            "output_tokens, usd, cost_known, ts FROM cost_ledger WHERE mission_id = %s "
            "ORDER BY id DESC LIMIT %s) t ORDER BY id",
            (mission_id, limit),
        )
        return [
            CostRow(
                mission_id=str(r[1]),
                cycle_id=str(r[2]),
                model=str(r[3]),
                role=str(r[4]),
                input_tokens=int(r[5]),
                output_tokens=int(r[6]),
                usd=None if r[7] is None else float(r[7]),
                cost_known=bool(r[8]),
                ts=_ts(r[9]),
            )
            for r in rows
        ]

    async def cost_summary(self, mission_id: str) -> CostSummary:
        rows = await self._fetchall(
            "SELECT COUNT(*), COALESCE(SUM(usd), 0), COUNT(*) FILTER (WHERE NOT cost_known), "
            "COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) "
            "FROM cost_ledger WHERE mission_id = %s",
            (mission_id,),
        )
        r = rows[0]
        return CostSummary(
            mission_id=mission_id,
            calls=int(r[0]),
            known_usd=float(r[1]),
            unknown_cost_calls=int(r[2]),
            input_tokens=int(r[3]),
            output_tokens=int(r[4]),
        )

    # --- episodic events ----------------------------------------------------------------
    async def append_event(
        self, mission_id: str, *, cycle_id: str, kind: str, payload: dict[str, Any]
    ) -> int:
        rows = await self._fetchall(
            "INSERT INTO episodic_events (mission_id, cycle_id, kind, payload) "
            "VALUES (%s, %s, %s, %s::jsonb) RETURNING id",
            (mission_id, cycle_id, kind, json.dumps(payload, sort_keys=True, default=str)),
        )
        return int(rows[0][0])

    async def list_events(
        self,
        mission_id: str,
        *,
        kinds: tuple[str, ...] | None = None,
        after_id: int = 0,
        limit: int = 200,
    ) -> list[EventRow]:
        sql = (
            "SELECT id, mission_id, cycle_id, kind, payload, ts FROM episodic_events "
            "WHERE mission_id = %s AND id > %s"
        )
        params: list[Any] = [mission_id, after_id]
        if kinds:
            sql += " AND kind = ANY(%s)"
            params.append(list(kinds))
        sql = f"SELECT * FROM ({sql} ORDER BY id DESC LIMIT %s) t ORDER BY id"
        params.append(limit)
        rows = await self._fetchall(sql, params)
        return [
            EventRow(
                id=int(r[0]),
                mission_id=str(r[1]),
                cycle_id=str(r[2]),
                kind=str(r[3]),
                payload=r[4] if isinstance(r[4], dict) else json.loads(r[4] or "{}"),
                ts=_ts(r[5]),
            )
            for r in rows
        ]

    # --- semantic memory ----------------------------------------------------------------
    async def put_memory(
        self,
        mission_id: str,
        records: list[MemoryRecord],
        *,
        vectors: list[list[float]] | None = None,
        embedding_model: str = "none",
        embedding_version: str = "0",
    ) -> None:
        """Upsert rows WITHOUT vectors (vectors go through ``PgSemanticIndex.add``)."""
        if vectors is not None:
            raise ValueError("PostgresStore.put_memory stores text only; use PgSemanticIndex")
        for record in records:
            await self._execute(
                "INSERT INTO semantic_memory (id, mission_id, kind, text, metadata, "
                "embedding_model, embedding_version, valid) "
                "VALUES (%s, %s, %s, %s, %s::jsonb, %s, %s, %s) "
                "ON CONFLICT (id) DO UPDATE SET mission_id = EXCLUDED.mission_id, "
                "kind = EXCLUDED.kind, text = EXCLUDED.text, metadata = EXCLUDED.metadata, "
                "valid = EXCLUDED.valid",
                (
                    record.id,
                    mission_id,
                    record.kind,
                    record.text,
                    json.dumps(record.metadata, sort_keys=True),
                    embedding_model,
                    embedding_version,
                    record.valid,
                ),
            )

    async def list_memory(self, mission_id: str, *, limit: int = 500) -> list[MemoryRecord]:
        rows = await self._fetchall(
            "SELECT * FROM (SELECT id, kind, text, metadata, embedding_model, "
            "embedding_version, valid, created_at FROM semantic_memory "
            "WHERE mission_id = %s AND valid ORDER BY created_at DESC, id LIMIT %s) t "
            "ORDER BY created_at, id",
            (mission_id, limit),
        )
        return [
            MemoryRecord(
                id=str(r[0]),
                kind=str(r[1]),
                text=str(r[2]),
                metadata={str(k): str(v) for k, v in (r[3] or {}).items()},
                embedding_model=r[4],
                embedding_version=r[5],
                valid=bool(r[6]),
            )
            for r in rows
        ]

    async def invalidate_memory(self, ids: list[str]) -> int:
        if not ids:
            return 0
        cursor = await self._execute(
            "UPDATE semantic_memory SET valid = false WHERE valid AND id = ANY(%s)", (list(ids),)
        )
        return int(cursor.rowcount or 0)

    # --- skills -------------------------------------------------------------------------
    async def put_skill(self, skill: Skill) -> None:
        if not skill.verified:
            raise SkillNotVerifiedError(
                f"refusing to store unverified skill {skill.id!r} (must pass the test gate first)"
            )
        await self._execute(
            "INSERT INTO skills (id, namespace, name, description, code, preconditions, "
            "provenance, expires_at, verified, uses) "
            "VALUES (%s, %s, %s, %s, %s, %s::jsonb, %s, %s, true, %s) "
            "ON CONFLICT (id) DO UPDATE SET namespace = EXCLUDED.namespace, "
            "name = EXCLUDED.name, description = EXCLUDED.description, code = EXCLUDED.code, "
            "preconditions = EXCLUDED.preconditions, provenance = EXCLUDED.provenance, "
            "expires_at = EXCLUDED.expires_at, verified = EXCLUDED.verified, "
            "uses = EXCLUDED.uses, updated_at = now()",
            (
                skill.id,
                skill.namespace,
                skill.name,
                skill.description,
                skill.code,
                json.dumps(skill.preconditions),
                skill.provenance,
                skill.expires_at,
                skill.uses,
            ),
        )

    async def list_skills(self, namespace: str, *, limit: int = 200) -> list[Skill]:
        rows = await self._fetchall(
            "SELECT id, name, description, code, preconditions, namespace, provenance, "
            "expires_at, verified, uses FROM skills "
            "WHERE verified AND namespace IN (%s, 'global') "
            "ORDER BY updated_at DESC, id LIMIT %s",
            (namespace, limit),
        )
        return [
            Skill(
                id=str(r[0]),
                name=str(r[1]),
                description=str(r[2]),
                code=str(r[3]),
                preconditions=[str(p) for p in (r[4] or [])],
                namespace=str(r[5]),
                provenance=str(r[6]),
                expires_at=r[7],
                verified=bool(r[8]),
                uses=int(r[9]),
            )
            for r in rows
        ]
