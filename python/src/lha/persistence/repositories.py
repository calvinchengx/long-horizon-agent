"""Postgres repositories (requires the ``postgres`` extra).

Persist mission state + the cost ledger to the tables in ``db/migrations/`` (0001-0003) so a
mission's status/spend is queryable with plain SQL and survives independently of git. Imported
only when Postgres is configured — not at package import.
"""

from __future__ import annotations

import psycopg

from lha.governor.cost import CostEntry
from lha.ids import idempotency_key

_INSERT_COST = (
    "INSERT INTO cost_ledger "
    "(mission_id, cycle_id, model, input_tokens, output_tokens, usd, role, cost_known, "
    "idempotency_key) "
    "VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) "
    "ON CONFLICT (idempotency_key) DO NOTHING"
)


class CostLedgerRepo:
    """Persists per-call spend to ``cost_ledger`` — idempotently (requires migrations 0002+0003).

    A call whose cost is unknown (``cost_known=False``, e.g. an unpriced model) is stored with
    ``usd = NULL`` — never as $0 — so ``total_usd`` is the KNOWN spend and ``unknown_cost_calls``
    says how much of the spend is unaccounted for.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn

    async def record(self, mission_id: str, entry: CostEntry, *, call_key: str | int) -> bool:
        """Insert one ledger row; return False if this logical call was already recorded.

        ``call_key`` identifies the logical model call within its cycle (e.g. its sequence number
        in the cycle's ledger). The same ``(mission_id, cycle_id, call_key)`` always maps to the
        same idempotency key, so a retried/replayed write never double-counts spend.
        """
        key = idempotency_key("cost", mission_id, entry.cycle_id, call_key)
        async with await psycopg.AsyncConnection.connect(self._dsn, autocommit=True) as conn:
            cursor = await conn.execute(
                _INSERT_COST,
                (
                    mission_id,
                    entry.cycle_id,
                    entry.model,
                    entry.input_tokens,
                    entry.output_tokens,
                    entry.usd if entry.cost_known else None,
                    entry.role,
                    entry.cost_known,
                    key,
                ),
            )
            return bool(cursor.rowcount)

    async def total_usd(self, mission_id: str) -> float:
        async with await psycopg.AsyncConnection.connect(self._dsn, autocommit=True) as conn:
            cursor = await conn.execute(
                "SELECT COALESCE(SUM(usd), 0) FROM cost_ledger WHERE mission_id = %s",
                (mission_id,),
            )
            row = await cursor.fetchone()
            return float(row[0]) if row else 0.0

    async def unknown_cost_calls(self, mission_id: str) -> int:
        """Number of recorded calls whose cost is unknown (``usd IS NULL``)."""
        async with await psycopg.AsyncConnection.connect(self._dsn, autocommit=True) as conn:
            cursor = await conn.execute(
                "SELECT COUNT(*) FROM cost_ledger WHERE mission_id = %s AND NOT cost_known",
                (mission_id,),
            )
            row = await cursor.fetchone()
            return int(row[0]) if row else 0


class MissionRepo:
    """Upserts/reads mission rows (status, head_sha) in ``missions``."""

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn

    async def upsert(
        self, *, mission_id: str, title: str, status: str, head_sha: str | None = None
    ) -> None:
        """Insert or update a mission; ``head_sha=None`` keeps the stored sha (never nulls it)."""
        async with await psycopg.AsyncConnection.connect(self._dsn, autocommit=True) as conn:
            await conn.execute(
                "INSERT INTO missions (mission_id, title, status, head_sha) "
                "VALUES (%s, %s, %s, %s) "
                "ON CONFLICT (mission_id) DO UPDATE SET "
                "status = EXCLUDED.status, "
                "head_sha = COALESCE(EXCLUDED.head_sha, missions.head_sha), "
                "updated_at = now()",
                (mission_id, title, status, head_sha),
            )

    async def get_status(self, mission_id: str) -> str | None:
        async with await psycopg.AsyncConnection.connect(self._dsn, autocommit=True) as conn:
            cursor = await conn.execute(
                "SELECT status FROM missions WHERE mission_id = %s", (mission_id,)
            )
            row = await cursor.fetchone()
            return str(row[0]) if row else None
