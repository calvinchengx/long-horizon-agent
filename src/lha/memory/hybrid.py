"""Hybrid retrieval: BM25 (lexical) + dense (semantic) fused with Reciprocal Rank Fusion.

Lexical and dense retrieval catch different things; RRF fuses their rankings robustly without
score calibration. This is the standard two-stage recipe (fuse, then optionally cross-encoder
rerank). All pure-Python so it runs at $0 and is deterministic.
"""

from __future__ import annotations

import math
import re
from collections import Counter
from typing import Protocol, runtime_checkable

from lha.contracts.memory import MemoryRecord, RetrievalHit, SemanticIndex

_TOKEN = re.compile(r"[a-z0-9]+")


def _tokenize(text: str) -> list[str]:
    return _TOKEN.findall(text.lower())


class BM25Index:
    """A minimal in-memory BM25 keyword index. Re-adding an id replaces its document."""

    def __init__(self, k1: float = 1.5, b: float = 0.75) -> None:
        self._k1 = k1
        self._b = b
        self._docs: dict[str, list[str]] = {}

    def add(self, records: list[MemoryRecord]) -> None:
        for record in records:
            self._docs.pop(record.id, None)
            self._docs[record.id] = _tokenize(record.text)

    def remove(self, ids: list[str]) -> None:
        for doc_id in ids:
            self._docs.pop(doc_id, None)

    def __len__(self) -> int:
        return len(self._docs)

    def query(self, text: str, *, k: int = 10) -> list[tuple[str, float]]:
        if not self._docs:
            return []
        n = len(self._docs)
        avgdl = sum(len(toks) for toks in self._docs.values()) / n or 1.0
        q_terms = set(_tokenize(text))
        df: Counter[str] = Counter()
        for toks in self._docs.values():
            for term in set(toks) & q_terms:
                df[term] += 1

        scored: list[tuple[str, float]] = []
        for doc_id, toks in self._docs.items():
            tf = Counter(toks)
            dl = len(toks)
            score = 0.0
            for term in q_terms:
                if df[term] == 0:
                    continue
                idf = math.log(1 + (n - df[term] + 0.5) / (df[term] + 0.5))
                freq = tf[term]
                denom = freq + self._k1 * (1 - self._b + self._b * dl / avgdl)
                score += idf * (freq * (self._k1 + 1)) / denom if denom else 0.0
            if score > 0:
                scored.append((doc_id, score))
        scored.sort(key=lambda pair: pair[1], reverse=True)
        return scored[:k]


def reciprocal_rank_fusion(rankings: list[list[str]], *, k: int = 60) -> list[tuple[str, float]]:
    """Fuse ranked id-lists via RRF: score = sum 1/(k + rank)."""
    fused: dict[str, float] = {}
    for ranking in rankings:
        for rank, doc_id in enumerate(ranking, start=1):
            fused[doc_id] = fused.get(doc_id, 0.0) + 1.0 / (k + rank)
    return sorted(fused.items(), key=lambda pair: pair[1], reverse=True)


@runtime_checkable
class _Removable(Protocol):
    def remove(self, ids: list[str]) -> None: ...


@runtime_checkable
class _Invalidatable(Protocol):
    def invalidate(self, ids: list[str]) -> int: ...


class HybridRetriever:
    """Combines a dense ``SemanticIndex`` and a ``BM25Index`` via RRF.

    Records are keyed by ``id``: re-adding an id replaces it in both the lexical and dense parts.
    Soft-invalidated records (``valid=False``) are never returned. With ``max_records`` set, the
    oldest-inserted records are evicted (from both parts when the dense index supports
    ``remove``) so the retriever can't grow without bound over a long run.
    """

    def __init__(
        self,
        *,
        semantic: SemanticIndex,
        bm25: BM25Index | None = None,
        max_records: int | None = None,
    ) -> None:
        if max_records is not None and max_records < 1:
            raise ValueError("max_records must be >= 1")
        self._semantic = semantic
        self._bm25 = bm25 if bm25 is not None else BM25Index()
        self._max_records = max_records
        self._records: dict[str, MemoryRecord] = {}

    async def add(self, records: list[MemoryRecord]) -> None:
        # De-duplicate within the batch too (last one wins), so both parts see one copy per id.
        batch = list({record.id: record for record in records}.values())
        for record in batch:
            self._records.pop(record.id, None)
            self._records[record.id] = record
        self._bm25.add(batch)
        await self._semantic.add(batch)
        if self._max_records is not None and len(self._records) > self._max_records:
            overflow = len(self._records) - self._max_records
            evicted = list(self._records)[:overflow]
            for record_id in evicted:
                del self._records[record_id]
            self._bm25.remove(evicted)
            if isinstance(self._semantic, _Removable):
                self._semantic.remove(evicted)

    def invalidate(self, ids: list[str]) -> int:
        """Soft-forget ``ids`` (``valid=False``); they are never returned again. Returns count."""
        count = 0
        for record_id in ids:
            record = self._records.get(record_id)
            if record is not None and record.valid:
                record.valid = False
                count += 1
        # Keep invalidated docs from crowding the lexical candidate set; the dense side filters
        # on ``valid`` itself when it supports soft invalidation.
        self._bm25.remove(ids)
        if isinstance(self._semantic, _Invalidatable):
            self._semantic.invalidate(ids)
        return count

    def __len__(self) -> int:
        return len(self._records)

    async def query(self, text: str, *, k: int = 5, candidate_k: int = 20) -> list[RetrievalHit]:
        dense = [
            hit.record.id
            for hit in await self._semantic.query(text, k=candidate_k)
            if hit.record.valid
        ]
        lexical = [doc_id for doc_id, _ in self._bm25.query(text, k=candidate_k)]
        fused = reciprocal_rank_fusion([dense, lexical])
        hits: list[RetrievalHit] = []
        for doc_id, score in fused:
            record = self._records.get(doc_id)
            if record is not None and record.valid:
                hits.append(RetrievalHit(record=record, score=score))
            if len(hits) >= k:
                break
        return hits
