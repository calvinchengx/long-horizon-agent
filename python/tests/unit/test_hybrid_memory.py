"""Tests for hybrid retrieval (BM25 + dense + RRF), rerank passthrough, and consolidation."""

from __future__ import annotations

import pytest

from lha.contracts.memory import MemoryRecord, RetrievalHit
from lha.contracts.model import TurnResult
from lha.memory.consolidation import consolidate, soft_invalidate
from lha.memory.embeddings import HashEmbedder
from lha.memory.hybrid import BM25Index, HybridRetriever, reciprocal_rank_fusion
from lha.memory.rerank import NoopReranker
from lha.memory.semantic_memory import InMemorySemanticIndex
from lha.model.stub import StubModel


def _records() -> list[MemoryRecord]:
    return [
        MemoryRecord(
            id="1", kind="semantic", text="postgres database with pgvector for embeddings"
        ),
        MemoryRecord(id="2", kind="semantic", text="react frontend written in typescript"),
        MemoryRecord(id="3", kind="semantic", text="temporal workflow durable execution spine"),
    ]


def test_bm25_ranks_keyword_match_first() -> None:
    index = BM25Index()
    index.add(_records())
    ranked = index.query("pgvector embeddings", k=3)
    assert ranked[0][0] == "1"


def test_rrf_fuses_rankings() -> None:
    fused = reciprocal_rank_fusion([["a", "b", "c"], ["b", "a", "d"]])
    ids = [doc for doc, _ in fused]
    assert ids[0] in {"a", "b"}
    assert set(ids) == {"a", "b", "c", "d"}


@pytest.mark.asyncio
async def test_hybrid_retriever_combines_signals() -> None:
    retriever = HybridRetriever(semantic=InMemorySemanticIndex(HashEmbedder(dim=512)))
    await retriever.add(_records())
    hits = await retriever.query("durable temporal workflow", k=2)
    assert hits[0].record.id == "3"


@pytest.mark.asyncio
async def test_noop_reranker_is_passthrough() -> None:
    hits = [RetrievalHit(record=_records()[0], score=0.5)]
    assert await NoopReranker().rerank("q", hits) == hits


@pytest.mark.asyncio
async def test_consolidate_parses_facts() -> None:
    model = StubModel(script=[TurnResult(text='["fact one", "fact two"]')])
    result = await consolidate(model=model, episodes=["e1", "e2"])
    assert result.ok
    assert [f.text for f in result.facts] == ["fact one", "fact two"]
    assert all(f.kind == "semantic" for f in result.facts)


@pytest.mark.asyncio
async def test_consolidate_falls_back_on_garbage() -> None:
    model = StubModel(script=[TurnResult(text="no json here")])
    result = await consolidate(model=model, episodes=["only episode"])
    assert not result.ok
    assert result.failed_batches == 1
    assert [f.text for f in result.facts] == ["only episode"]
    assert result.facts[0].metadata["consolidation"] == "verbatim"


def test_soft_invalidate_marks_matching() -> None:
    records = _records()
    count = soft_invalidate(records, predicate=lambda r: r.id == "2")
    assert count == 1
    assert not next(r for r in records if r.id == "2").valid
