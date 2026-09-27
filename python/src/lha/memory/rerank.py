"""Reranking (the second stage of two-stage retrieval).

After fusion, a reranker rescores the top candidates by joint query-document relevance — much
sharper than bi-encoder cosine, at higher cost, so it's applied only to a small candidate set
(the rerank depth is a governor-throttleable knob). ``NoopReranker`` is the $0/offline default;
``CrossEncoderReranker`` uses sentence-transformers (the ``embeddings`` extra);
``SystemOneReranker`` asks a System One model (``lha.systemone``) whether each passage helps.
"""

from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING

import structlog
from pydantic import JsonValue

from lha.contracts.memory import RetrievalHit
from lha.contracts.system_one import (
    Noul,
    NoulAnswer,
    Question,
    SystemOneError,
    SystemOneModel,
)
from lha.obs.redact import redact_text

if TYPE_CHECKING:
    from sentence_transformers.base.modality_types import PairInput


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
        pairs: list[PairInput] = [(query, hit.record.text) for hit in hits]
        scores = await asyncio.to_thread(lambda: self._model.predict(pairs))
        rescored = [
            RetrievalHit(record=hit.record, score=float(score))
            for hit, score in zip(hits, scores, strict=True)
        ]
        rescored.sort(key=lambda hit: hit.score, reverse=True)
        return rescored[:k] if k is not None else rescored


#: Characters of each passage sent to a System One reranker.
SYSTEM_ONE_PASSAGE_CHARS = 1500
_RELEVANCE = (
    "Does `passage` contain information that helps to carry out `task` (a fact, a past attempt "
    "or its outcome, a decision, or code it needs)?"
)


def system_one_rerank_request(
    query: str, hits: list[RetrievalHit]
) -> tuple[dict[str, JsonValue], dict[str, Question]]:
    """The state (the task) and one relevance ``Noul`` per hit (``p0``, ``p1``, ...)."""
    questions: dict[str, Question] = {
        f"p{i}": Noul(
            instructions={
                "passage": redact_text(hit.record.text[:SYSTEM_ONE_PASSAGE_CHARS]),
                "question": _RELEVANCE,
            }
        )
        for i, hit in enumerate(hits)
    }
    return {"task": redact_text(query)}, questions


def apply_relevance(
    hits: list[RetrievalHit], relevance: list[float], *, k: int | None, min_p: float
) -> list[RetrievalHit]:
    """Hits rescored by relevance, most relevant first (ties keep fusion order), without those
    below ``min_p``, cut to ``k``."""
    ranked = sorted(
        (
            (p, i, RetrievalHit(record=hit.record, score=p))
            for i, (hit, p) in enumerate(zip(hits, relevance, strict=True))
            if p >= min_p
        ),
        key=lambda t: (-t[0], t[1]),
    )
    kept = [hit for _p, _i, hit in ranked]
    return kept[:k] if k is not None else kept


class SystemOneReranker:
    """Reranks by a System One model's probability that each passage helps with the task.

    One request per recall, all passages as parallel questions. Any failure keeps fusion order
    (logged): recall must never fail a cycle.
    """

    def __init__(self, model: SystemOneModel, *, min_p: float = 0.0) -> None:
        self.name = f"system-one:{model.name}"
        self._model = model
        self._min_p = min_p

    async def rerank(
        self, query: str, hits: list[RetrievalHit], *, k: int | None = None
    ) -> list[RetrievalHit]:
        if not hits:
            return []
        state, questions = system_one_rerank_request(query, hits)
        try:
            result = await self._model.evaluate(state, questions)
        except SystemOneError as exc:
            structlog.get_logger("lha.memory").warning("memory_rerank_failed", error=str(exc))
            return hits[:k] if k is not None else hits
        relevance = [
            answer.noul if isinstance(answer, NoulAnswer) else 0.0
            for answer in (result.answers[qid] for qid in questions)
        ]
        return apply_relevance(hits, relevance, k=k, min_p=self._min_p)
