"""An in-memory semantic index (cosine similarity).

The $0/offline default and the reference implementation of the ``SemanticIndex`` contract. The
pgvector-backed store (added with the Postgres infra) implements the same interface, so callers
never change. Records are stamped with the embedder's model+version on insert, and queries only
compare within a single (model, version) — never across — to avoid silent vector drift.
"""

from __future__ import annotations

import math

from lha.contracts.memory import Embedder, MemoryRecord, RetrievalHit, SemanticIndex


def cosine(a: list[float], b: list[float]) -> float:
    """Cosine similarity of two equal-length vectors (0.0 if either is all-zero).

    A length mismatch is a bug (embedder/column dimension drift), never "no similarity", so it
    raises instead of quietly scoring 0.0.
    """
    if len(a) != len(b):
        raise ValueError(f"cosine: vector length mismatch ({len(a)} != {len(b)})")
    dot = sum(x * y for x, y in zip(a, b, strict=True))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0.0 or nb == 0.0:
        return 0.0
    return dot / (na * nb)


class InMemorySemanticIndex(SemanticIndex):
    """A simple, exact-cosine semantic index backed by Python dicts.

    Records are keyed by ``id``: re-adding an id replaces the stored record + vector (no
    duplicates). With ``max_records`` set, the oldest-inserted records are evicted once the index
    grows past that size, so a long run can't grow memory without bound.
    """

    def __init__(self, embedder: Embedder, *, max_records: int | None = None) -> None:
        if max_records is not None and max_records < 1:
            raise ValueError("max_records must be >= 1")
        self._embedder = embedder
        self._max_records = max_records
        self._entries: dict[str, tuple[MemoryRecord, list[float]]] = {}

    async def add(self, records: list[MemoryRecord]) -> None:
        if not records:
            return
        vectors = await self._embedder.embed([r.text for r in records])
        for record, vector in zip(records, vectors, strict=True):
            if len(vector) != self._embedder.dim:
                raise ValueError(
                    f"embedder {self._embedder.name!r} returned a dim={len(vector)} vector; "
                    f"declared dim={self._embedder.dim}"
                )
            stamped = record.model_copy(
                update={
                    "embedding_model": self._embedder.name,
                    "embedding_version": self._embedder.version,
                }
            )
            self._entries.pop(record.id, None)  # re-add moves the id to the newest position
            self._entries[record.id] = (stamped, vector)
        if self._max_records is not None:
            while len(self._entries) > self._max_records:
                del self._entries[next(iter(self._entries))]

    def remove(self, ids: list[str]) -> None:
        """Drop ``ids`` from the index (used for size-bounded eviction; unknown ids ignored)."""
        for record_id in ids:
            self._entries.pop(record_id, None)

    def invalidate(self, ids: list[str]) -> int:
        """Soft-forget ``ids`` (``valid=False``); they are never returned again. Returns count."""
        count = 0
        for record_id in ids:
            entry = self._entries.get(record_id)
            if entry is not None and entry[0].valid:
                entry[0].valid = False
                count += 1
        return count

    async def query(self, text: str, *, k: int = 5) -> list[RetrievalHit]:
        if not self._entries:
            return []
        query_vec = (await self._embedder.embed([text]))[0]
        scored = [
            RetrievalHit(record=record, score=cosine(query_vec, vector))
            for record, vector in self._entries.values()
            if record.valid
            and record.embedding_model == self._embedder.name
            and record.embedding_version == self._embedder.version
        ]
        scored.sort(key=lambda hit: hit.score, reverse=True)
        return scored[:k]

    def __len__(self) -> int:
        return len(self._entries)
