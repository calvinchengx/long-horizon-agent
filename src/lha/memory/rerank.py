"""Cross-encoder reranking (the second stage of two-stage retrieval).

After fusion, a cross-encoder rescates the top candidates by joint query-document relevance —
much sharper than bi-encoder cosine, at higher cost, so it's applied only to a small candidate
set (the rerank depth is a governor-throttleable knob). ``NoopReranker`` is the $0/offline default;
``CrossEncoderReranker`` uses sentence-transformers (the ``embeddings`` extra).
"""

from __future__ import annotations

import asyncio

from lha.contracts.memory import RetrievalHit


class NoopReranker:
    """Pass-through reranker (keeps fusion order). The $0 default."""

    name = "noop"

    async def rerank(
        self, query: str, hits: list[RetrievalHit], *, k: int | None = None
    ) -> list[RetrievalHit]:
        return hits[:k] if k is not None else hits


class CrossEncoderReranker:
    """A real cross-encoder reranker (e.g. bge-reranker-v2-m3)."""

    def __init__(self, model_name: str = "BAAI/bge-reranker-v2-m3") -> None:
        from sentence_transformers import CrossEncoder

        self.name = f"cross-encoder:{model_name}"
        self._model = CrossEncoder(model_name)

    async def rerank(
        self, query: str, hits: list[RetrievalHit], *, k: int | None = None
    ) -> list[RetrievalHit]:
        if not hits:
            return []
        pairs = [(query, hit.record.text) for hit in hits]
        scores = await asyncio.to_thread(self._model.predict, pairs)
        rescored = [
            RetrievalHit(record=hit.record, score=float(score))
            for hit, score in zip(hits, scores, strict=True)
        ]
        rescored.sort(key=lambda hit: hit.score, reverse=True)
        return rescored[:k] if k is not None else rescored
