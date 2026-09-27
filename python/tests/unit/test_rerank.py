"""Cross-encoder reranking, with a fake model standing in for sentence-transformers."""

from __future__ import annotations

from lha.contracts.memory import MemoryRecord, RetrievalHit
from lha.memory.rerank import CrossEncoderReranker, NoopReranker


class _LengthScorer:
    """Scores a (query, text) pair by the text's length; records what it was asked."""

    def __init__(self) -> None:
        self.seen: list[object] = []

    def predict(self, pairs: list[tuple[str, str]]) -> list[float]:
        self.seen.append(pairs)
        return [float(len(text)) for _query, text in pairs]


def _hit(text: str, score: float = 0.5) -> RetrievalHit:
    return RetrievalHit(record=MemoryRecord(id=text, kind="semantic", text=text), score=score)


def _reranker(model: _LengthScorer) -> CrossEncoderReranker:
    reranker = CrossEncoderReranker.__new__(CrossEncoderReranker)  # skip loading a real model
    reranker.name = "cross-encoder:fake"
    reranker._model = model  # type: ignore[assignment]
    return reranker


async def test_cross_encoder_rescores_and_sorts_by_its_own_scores() -> None:
    model = _LengthScorer()
    hits = [_hit("bb", 0.9), _hit("dddd", 0.1), _hit("a", 0.8)]
    ranked = await _reranker(model).rerank("q", hits, k=2)
    assert [(h.record.text, h.score) for h in ranked] == [("dddd", 4.0), ("bb", 2.0)]
    assert model.seen == [[("q", "bb"), ("q", "dddd"), ("q", "a")]]


async def test_cross_encoder_skips_the_model_for_no_hits() -> None:
    model = _LengthScorer()
    assert await _reranker(model).rerank("q", []) == []
    assert model.seen == []


async def test_noop_keeps_fusion_order() -> None:
    hits = [_hit("b"), _hit("a")]
    assert await NoopReranker().rerank("q", hits, k=1) == hits[:1]
