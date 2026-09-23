"""SQLite backend of ``MissionStore`` (stdlib ``sqlite3``; the zero-infra default).

The tables mirror ``db/migrations`` (0001-0004) with SQLite types: timestamps are ISO-8601 UTC
text, ``jsonb`` is JSON text, vectors are JSON arrays of floats. The schema is created/upgraded on
open and versioned in ``schema_migrations`` (versions prefixed ``sqlite_``).

The database runs in WAL mode with a busy timeout, so a CLI reader (``lha missions``) and a
running mission (or several worker processes) can use the same file. One connection per store is
shared across threads under a lock; every call runs in a worker thread so the event loop never
blocks on disk I/O.
"""

from __future__ import annotations

import asyncio
import json
import sqlite3
import threading
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.ids import idempotency_key
from lha.memory.skills import Skill, SkillNotVerifiedError
from lha.persistence.store import (
    BACKEND_SQLITE,
    CostRow,
    CostSummary,
    EventRow,
    MissionRow,
)

BUSY_TIMEOUT_MS = 10_000

# (version, script) in order. Append new versions; never edit an applied one.
SQLITE_MIGRATIONS: tuple[tuple[str, str], ...] = (
    (
        "sqlite_0001_init",
        """
        CREATE TABLE IF NOT EXISTS missions (
            mission_id         TEXT PRIMARY KEY,
            title              TEXT NOT NULL,
            description        TEXT NOT NULL DEFAULT '',
            acceptance         TEXT NOT NULL DEFAULT '',
            status             TEXT NOT NULL DEFAULT 'RUNNING',
            workflow_id        TEXT,
            run_id             TEXT,
            latest_snapshot_id TEXT,
            latest_session_id  TEXT,
            head_sha           TEXT,
            schema_version     INTEGER NOT NULL DEFAULT 1,
            created_at         TEXT NOT NULL,
            updated_at         TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS cost_ledger (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            mission_id      TEXT NOT NULL,
            cycle_id        TEXT NOT NULL DEFAULT '',
            ts              TEXT NOT NULL,
            model           TEXT NOT NULL,
            input_tokens    INTEGER NOT NULL DEFAULT 0,
            output_tokens   INTEGER NOT NULL DEFAULT 0,
            usd             REAL,
            idempotency_key TEXT UNIQUE,
            role            TEXT NOT NULL DEFAULT '',
            cost_known      INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS cost_ledger_mission_ts ON cost_ledger (mission_id, ts);
        CREATE TABLE IF NOT EXISTS episodic_events (
            id              INTEGER PRIMARY KEY AUTOINCREMENT,
            mission_id      TEXT NOT NULL,
            cycle_id        TEXT NOT NULL DEFAULT '',
            ts              TEXT NOT NULL,
            kind            TEXT NOT NULL,
            payload         TEXT NOT NULL DEFAULT '{}',
            payload_ref     TEXT,
            schema_version  INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS episodic_events_mission ON episodic_events (mission_id, id);
        CREATE TABLE IF NOT EXISTS semantic_memory (
            id                TEXT PRIMARY KEY,
            mission_id        TEXT,
            kind              TEXT NOT NULL DEFAULT 'semantic',
            text              TEXT NOT NULL,
            metadata          TEXT NOT NULL DEFAULT '{}',
            embedding         TEXT,
            embedding_model   TEXT NOT NULL,
            embedding_version TEXT NOT NULL,
            valid             INTEGER NOT NULL DEFAULT 1,
            source_event_id   INTEGER,
            created_at        TEXT NOT NULL,
            schema_version    INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS semantic_memory_mission ON semantic_memory (mission_id);
        CREATE INDEX IF NOT EXISTS semantic_memory_modelver
            ON semantic_memory (embedding_model, embedding_version);
        CREATE TABLE IF NOT EXISTS skills (
            id             TEXT PRIMARY KEY,
            namespace      TEXT NOT NULL DEFAULT 'global',
            name           TEXT NOT NULL,
            description    TEXT NOT NULL,
            code           TEXT NOT NULL DEFAULT '',
            preconditions  TEXT NOT NULL DEFAULT '[]',
            provenance     TEXT NOT NULL DEFAULT '',
            expires_at     TEXT,
            verified       INTEGER NOT NULL DEFAULT 0,
            uses           INTEGER NOT NULL DEFAULT 0,
            created_at     TEXT NOT NULL,
            updated_at     TEXT NOT NULL,
            schema_version INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS skills_namespace ON skills (namespace);
        """,
    ),
)


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")


def cost_idempotency_key(mission_id: str, cycle_id: str, call_key: str) -> str:
    """The ledger row key; identical to ``CostLedgerRepo``'s derivation (Postgres)."""
    return idempotency_key("cost", mission_id, cycle_id, call_key)


class SqliteStore:
    """``MissionStore`` on a local SQLite file."""

    backend = BACKEND_SQLITE

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path).expanduser().resolve()
        self.degraded_reason = ""
        self._conn: sqlite3.Connection | None = None
        self._lock = threading.Lock()

    # --- plumbing -----------------------------------------------------------------------
    def _open_sync(self) -> None:
        if self._conn is not None:
            return
        self.path.parent.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(
            self.path, timeout=BUSY_TIMEOUT_MS / 1000, check_same_thread=False, isolation_level=None
        )
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute(f"PRAGMA busy_timeout={BUSY_TIMEOUT_MS}")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute(
            "CREATE TABLE IF NOT EXISTS schema_migrations "
            "(version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"
        )
        applied = {str(r[0]) for r in conn.execute("SELECT version FROM schema_migrations")}
        for version, script in SQLITE_MIGRATIONS:
            if version in applied:
                continue
            conn.execute("BEGIN IMMEDIATE")
            try:
                # Re-check under the write lock: another process may have just applied it.
                done = conn.execute(
                    "SELECT 1 FROM schema_migrations WHERE version = ?", (version,)
                ).fetchone()
                if done is None:
                    for statement in script.split(";"):
                        if statement.strip():
                            conn.execute(statement)
                    conn.execute(
                        "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
                        (version, _now()),
                    )
                conn.execute("COMMIT")
            except BaseException:
                conn.execute("ROLLBACK")
                raise
        self._conn = conn

    async def open(self) -> None:
        await asyncio.to_thread(self._locked, self._open_sync)

    def _locked[T](self, fn: Callable[[], T]) -> T:
        with self._lock:
            return fn()

    async def _run[T](self, fn: Callable[[sqlite3.Connection], T]) -> T:
        def _call() -> T:
            with self._lock:
                self._open_sync()
                assert self._conn is not None
                return fn(self._conn)

        return await asyncio.to_thread(_call)

    @property
    def journal_mode(self) -> str:
        with self._lock:
            self._open_sync()
            assert self._conn is not None
            return str(self._conn.execute("PRAGMA journal_mode").fetchone()[0])

    async def close(self) -> None:
        def _close() -> None:
            with self._lock:
                if self._conn is not None:
                    self._conn.close()
                    self._conn = None

        await asyncio.to_thread(_close)

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
        now = _now()

        def _upsert(conn: sqlite3.Connection) -> None:
            conn.execute(
                "INSERT INTO missions (mission_id, title, description, status, head_sha, "
                "workflow_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) "
                "ON CONFLICT (mission_id) DO UPDATE SET "
                "title = CASE WHEN excluded.title != '' THEN excluded.title ELSE title END, "
                "description = CASE WHEN excluded.description != '' "
                "THEN excluded.description ELSE description END, "
                "status = excluded.status, "
                "head_sha = COALESCE(NULLIF(excluded.head_sha, ''), head_sha), "
                "workflow_id = COALESCE(excluded.workflow_id, workflow_id), "
                "updated_at = excluded.updated_at",
                (mission_id, title, description, status, head_sha, workflow_id, now, now),
            )

        await self._run(_upsert)

    @staticmethod
    def _mission(row: sqlite3.Row) -> MissionRow:
        return MissionRow(
            mission_id=row["mission_id"],
            title=row["title"],
            status=row["status"],
            description=row["description"],
            head_sha=row["head_sha"],
            workflow_id=row["workflow_id"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )

    async def get_mission(self, mission_id: str) -> MissionRow | None:
        def _get(conn: sqlite3.Connection) -> MissionRow | None:
            row = conn.execute(
                "SELECT * FROM missions WHERE mission_id = ?", (mission_id,)
            ).fetchone()
            return self._mission(row) if row is not None else None

        return await self._run(_get)

    async def list_missions(self, *, limit: int = 20) -> list[MissionRow]:
        def _list(conn: sqlite3.Connection) -> list[MissionRow]:
            rows = conn.execute(
                "SELECT * FROM missions ORDER BY updated_at DESC, mission_id LIMIT ?", (limit,)
            ).fetchall()
            return [self._mission(r) for r in rows]

        return await self._run(_list)

    # --- cost ledger --------------------------------------------------------------------
    async def record_cost(self, mission_id: str, entry: CostEntry, *, call_key: str) -> bool:
        key = cost_idempotency_key(mission_id, entry.cycle_id, call_key)

        def _insert(conn: sqlite3.Connection) -> bool:
            cursor = conn.execute(
                "INSERT OR IGNORE INTO cost_ledger (mission_id, cycle_id, ts, model, "
                "input_tokens, output_tokens, usd, idempotency_key, role, cost_known) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    mission_id,
                    entry.cycle_id,
                    _now(),
                    entry.model,
                    entry.input_tokens,
                    entry.output_tokens,
                    entry.usd if entry.cost_known else None,
                    key,
                    entry.role,
                    1 if entry.cost_known else 0,
                ),
            )
            return cursor.rowcount > 0

        return await self._run(_insert)

    async def list_costs(self, mission_id: str, *, limit: int = 200) -> list[CostRow]:
        def _list(conn: sqlite3.Connection) -> list[CostRow]:
            rows = conn.execute(
                "SELECT * FROM (SELECT * FROM cost_ledger WHERE mission_id = ? "
                "ORDER BY id DESC LIMIT ?) ORDER BY id",
                (mission_id, limit),
            ).fetchall()
            return [
                CostRow(
                    mission_id=r["mission_id"],
                    cycle_id=r["cycle_id"],
                    model=r["model"],
                    role=r["role"],
                    input_tokens=int(r["input_tokens"]),
                    output_tokens=int(r["output_tokens"]),
                    usd=None if r["usd"] is None else float(r["usd"]),
                    cost_known=bool(r["cost_known"]),
                    ts=r["ts"],
                )
                for r in rows
            ]

        return await self._run(_list)

    async def cost_summary(self, mission_id: str) -> CostSummary:
        def _summary(conn: sqlite3.Connection) -> CostSummary:
            row = conn.execute(
                "SELECT COUNT(*), COALESCE(SUM(usd), 0), "
                "COALESCE(SUM(CASE WHEN cost_known = 0 THEN 1 ELSE 0 END), 0), "
                "COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) "
                "FROM cost_ledger WHERE mission_id = ?",
                (mission_id,),
            ).fetchone()
            return CostSummary(
                mission_id=mission_id,
                calls=int(row[0]),
                known_usd=float(row[1]),
                unknown_cost_calls=int(row[2]),
                input_tokens=int(row[3]),
                output_tokens=int(row[4]),
            )

        return await self._run(_summary)

    # --- episodic events ----------------------------------------------------------------
    async def append_event(
        self, mission_id: str, *, cycle_id: str, kind: str, payload: dict[str, Any]
    ) -> int:
        body = json.dumps(payload, sort_keys=True, default=str)

        def _append(conn: sqlite3.Connection) -> int:
            cursor = conn.execute(
                "INSERT INTO episodic_events (mission_id, cycle_id, ts, kind, payload) "
                "VALUES (?, ?, ?, ?, ?)",
                (mission_id, cycle_id, _now(), kind, body),
            )
            return int(cursor.lastrowid or 0)

        return await self._run(_append)

    async def list_events(
        self,
        mission_id: str,
        *,
        kinds: tuple[str, ...] | None = None,
        after_id: int = 0,
        limit: int = 200,
    ) -> list[EventRow]:
        def _list(conn: sqlite3.Connection) -> list[EventRow]:
            sql = "SELECT * FROM episodic_events WHERE mission_id = ? AND id > ?"
            params: list[Any] = [mission_id, after_id]
            if kinds:
                sql += f" AND kind IN ({', '.join('?' for _ in kinds)})"
                params.extend(kinds)
            sql = f"SELECT * FROM ({sql} ORDER BY id DESC LIMIT ?) ORDER BY id"
            params.append(limit)
            return [
                EventRow(
                    id=int(r["id"]),
                    mission_id=r["mission_id"],
                    cycle_id=r["cycle_id"],
                    kind=r["kind"],
                    payload=json.loads(r["payload"] or "{}"),
                    ts=r["ts"],
                )
                for r in conn.execute(sql, params).fetchall()
            ]

        return await self._run(_list)

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
        if not records:
            return
        if vectors is not None and len(vectors) != len(records):
            raise ValueError("put_memory: one vector per record is required")
        now = _now()
        rows = [
            (
                record.id,
                mission_id,
                record.kind,
                record.text,
                json.dumps(record.metadata, sort_keys=True),
                json.dumps(vectors[i]) if vectors is not None else None,
                embedding_model,
                embedding_version,
                1 if record.valid else 0,
                now,
            )
            for i, record in enumerate(records)
        ]

        def _put(conn: sqlite3.Connection) -> None:
            conn.executemany(
                "INSERT INTO semantic_memory (id, mission_id, kind, text, metadata, embedding, "
                "embedding_model, embedding_version, valid, created_at) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
                "ON CONFLICT (id) DO UPDATE SET mission_id = excluded.mission_id, "
                "kind = excluded.kind, text = excluded.text, metadata = excluded.metadata, "
                "embedding = excluded.embedding, embedding_model = excluded.embedding_model, "
                "embedding_version = excluded.embedding_version, valid = excluded.valid",
                rows,
            )

        await self._run(_put)

    @staticmethod
    def _record(row: sqlite3.Row) -> MemoryRecord:
        return MemoryRecord(
            id=row["id"],
            kind=row["kind"],
            text=row["text"],
            metadata={str(k): str(v) for k, v in json.loads(row["metadata"] or "{}").items()},
            embedding_model=row["embedding_model"],
            embedding_version=row["embedding_version"],
            valid=bool(row["valid"]),
        )

    async def list_memory(self, mission_id: str, *, limit: int = 500) -> list[MemoryRecord]:
        def _list(conn: sqlite3.Connection) -> list[MemoryRecord]:
            rows = conn.execute(
                "SELECT * FROM (SELECT rowid AS rid, * FROM semantic_memory "
                "WHERE mission_id = ? AND valid = 1 ORDER BY rowid DESC LIMIT ?) ORDER BY rid",
                (mission_id, limit),
            ).fetchall()
            return [self._record(r) for r in rows]

        return await self._run(_list)

    async def memory_vectors(
        self,
        mission_id: str,
        *,
        embedding_model: str,
        embedding_version: str,
        limit: int = 2000,
    ) -> list[tuple[MemoryRecord, list[float]]]:
        """Valid records with a vector from exactly this embedder model+version (SQLite dense)."""

        def _list(conn: sqlite3.Connection) -> list[tuple[MemoryRecord, list[float]]]:
            rows = conn.execute(
                "SELECT * FROM (SELECT rowid AS rid, * FROM semantic_memory "
                "WHERE mission_id = ? AND valid = 1 AND embedding IS NOT NULL "
                "AND embedding_model = ? AND embedding_version = ? "
                "ORDER BY rowid DESC LIMIT ?) ORDER BY rid",
                (mission_id, embedding_model, embedding_version, limit),
            ).fetchall()
            return [(self._record(r), [float(x) for x in json.loads(r["embedding"])]) for r in rows]

        return await self._run(_list)

    async def invalidate_memory(self, ids: list[str]) -> int:
        if not ids:
            return 0

        def _invalidate(conn: sqlite3.Connection) -> int:
            cursor = conn.execute(
                f"UPDATE semantic_memory SET valid = 0 WHERE valid = 1 AND id IN "
                f"({', '.join('?' for _ in ids)})",
                ids,
            )
            return cursor.rowcount

        return await self._run(_invalidate)

    # --- skills -------------------------------------------------------------------------
    async def put_skill(self, skill: Skill) -> None:
        if not skill.verified:
            raise SkillNotVerifiedError(
                f"refusing to store unverified skill {skill.id!r} (must pass the test gate first)"
            )
        now = _now()

        def _put(conn: sqlite3.Connection) -> None:
            conn.execute(
                "INSERT INTO skills (id, namespace, name, description, code, preconditions, "
                "provenance, expires_at, verified, uses, created_at, updated_at) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
                "ON CONFLICT (id) DO UPDATE SET namespace = excluded.namespace, "
                "name = excluded.name, description = excluded.description, "
                "code = excluded.code, preconditions = excluded.preconditions, "
                "provenance = excluded.provenance, expires_at = excluded.expires_at, "
                "verified = excluded.verified, uses = excluded.uses, "
                "updated_at = excluded.updated_at",
                (
                    skill.id,
                    skill.namespace,
                    skill.name,
                    skill.description,
                    skill.code,
                    json.dumps(skill.preconditions),
                    skill.provenance,
                    skill.expires_at,
                    1,
                    skill.uses,
                    now,
                    now,
                ),
            )

        await self._run(_put)

    async def list_skills(self, namespace: str, *, limit: int = 200) -> list[Skill]:
        def _list(conn: sqlite3.Connection) -> list[Skill]:
            rows = conn.execute(
                "SELECT * FROM skills WHERE verified = 1 AND namespace IN (?, 'global') "
                "ORDER BY updated_at DESC, id LIMIT ?",
                (namespace, limit),
            ).fetchall()
            return [
                Skill(
                    id=r["id"],
                    name=r["name"],
                    description=r["description"],
                    code=r["code"],
                    preconditions=list(json.loads(r["preconditions"] or "[]")),
                    namespace=r["namespace"],
                    provenance=r["provenance"],
                    expires_at=r["expires_at"],
                    verified=bool(r["verified"]),
                    uses=int(r["uses"]),
                )
                for r in rows
            ]

        return await self._run(_list)
