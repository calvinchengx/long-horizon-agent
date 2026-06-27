"""Postgres + pgvector semantic store (production implementation of ``SemanticIndex``).

Same interface as the in-memory index, so callers never change. Requires the ``postgres`` extra
(``psycopg`` + ``pgvector``) and the schema in ``db/migrations/`` (0002 makes ``id`` text). Queries are gated
by (embedding_model, embedding_version) so vectors are never compared across embedder versions.
Imported only when ``LHA_MODEL_BACKEND``-adjacent config selects Postgres — not at package import.

Vector parameters are cast explicitly (``%s::vector``): a plain Python ``list`` is sent as a
``float8[]``, for which pgvector has no ``<=>`` operator (only an explicit/assignment cast).

Rows are keyed by ``MemoryRecord.id`` (so a dense hit maps back to the same record the caller
stored, e.g. for hybrid fusion), and re-adding an id upserts rather than duplicating. The
embedder's ``dim`` must equal the ``vector(N)`` column width; a mismatch is rejected at
construction instead of surfacing as an opaque database error (or silently wrong scores).
"""

from __future__ import annotations

from typing import Any

from lha.contracts.memory import Embedder, MemoryRecord, RetrievalHit

#: Width of ``semantic_memory.embedding`` in ``db/migrations/0001_init.sql`` (``vector(1024)``).
DEFAULT_COLUMN_DIM = 1024


class EmbeddingDimensionError(ValueError):
    """The embedder's vector width does not match the pgvector column width."""


class PgSemanticIndex:
    """A ``SemanticIndex`` backed by Postgres + pgvector (HNSW cosine)."""

    def __init__(
        self,
        *,
        dsn: str,
        embedder: Embedder,
        column_dim: int = DEFAULT_COLUMN_DIM,
        pool: Any | None = None,
    ) -> None:
        if embedder.dim != column_dim:
            raise EmbeddingDimensionError(
                f"embedder {embedder.name!r} produces dim={embedder.dim} vectors but the "
                f"semantic_memory.embedding column is vector({column_dim})"
            )
        self._dsn = dsn
        self._embedder = embedder
        self._column_dim = column_dim
        self._pool: Any | None = pool  # injectable (tests / shared pools)

    async def _ensure_pool(self) -> Any:
        if self._pool is None:
            from psycopg_pool import AsyncConnectionPool

            pool = AsyncConnectionPool(self._dsn, open=False, configure=_register_vector)
            await pool.open()
            self._pool = pool
        return self._pool

    def _check_vector(self, vector: list[float]) -> None:
        if len(vector) != self._column_dim:
            raise EmbeddingDimensionError(
                f"embedder {self._embedder.name!r} returned a dim={len(vector)} vector; "
                f"expected {self._column_dim}"
            )

    async def add(self, records: list[MemoryRecord]) -> None:
        if not records:
            return
        vectors = await self._embedder.embed([r.text for r in records])
        for vector in vectors:
            self._check_vector(vector)
        pool = await self._ensure_pool()
        async with pool.connection() as conn:
            for record, vector in zip(records, vectors, strict=True):
                await conn.execute(
                    "INSERT INTO semantic_memory "
                    "(id, mission_id, text, embedding, embedding_model, embedding_version, valid) "
                    "VALUES (%s, %s, %s, %s::vector, %s, %s, %s) "
                    "ON CONFLICT (id) DO UPDATE SET mission_id = EXCLUDED.mission_id, "
                    "text = EXCLUDED.text, embedding = EXCLUDED.embedding, "
                    "embedding_model = EXCLUDED.embedding_model, "
                    "embedding_version = EXCLUDED.embedding_version, valid = EXCLUDED.valid",
                    (
                        record.id,
                        record.metadata.get("mission_id"),
                        record.text,
                        vector,
                        self._embedder.name,
                        self._embedder.version,
                        record.valid,
                    ),
                )

    async def query(self, text: str, *, k: int = 5) -> list[RetrievalHit]:
        query_vec = (await self._embedder.embed([text]))[0]
        self._check_vector(query_vec)
        pool = await self._ensure_pool()
        async with pool.connection() as conn:
            cursor = await conn.execute(
                "SELECT id, text, embedding_model, embedding_version, valid, "
                "1 - (embedding <=> %s::vector) AS score "
                "FROM semantic_memory "
                "WHERE valid AND embedding_model = %s AND embedding_version = %s "
                "ORDER BY embedding <=> %s::vector LIMIT %s",
                (
                    query_vec,
                    self._embedder.name,
                    self._embedder.version,
                    query_vec,
                    k,
                ),
            )
            rows = await cursor.fetchall()

        hits: list[RetrievalHit] = []
        for row in rows:
            record = MemoryRecord(
                id=str(row[0]),
                kind="semantic",
                text=str(row[1]),
                embedding_model=str(row[2]),
                embedding_version=str(row[3]),
                valid=bool(row[4]),
            )
            hits.append(RetrievalHit(record=record, score=float(row[5])))
        return hits

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None


async def _register_vector(conn: Any) -> None:
    from pgvector.psycopg import register_vector_async

    await register_vector_async(conn)
