"""Regression tests for memory fixes: pg ids/dims (C1/C2), hybrid dedupe/invalidation/eviction
(C3), and batched, lossless consolidation (C4). No database or network needed."""

from __future__ import annotations

from typing import Any

import pytest

from lha.contracts.memory import MemoryRecord
from lha.contracts.model import ModelMessage, TurnResult
from lha.memory.consolidation import consolidate
from lha.memory.embeddings import HashEmbedder
from lha.memory.hybrid import BM25Index, HybridRetriever
from lha.memory.semantic_memory import InMemorySemanticIndex
from lha.memory.semantic_pg import EmbeddingDimensionError, PgSemanticIndex
from lha.model.stub import StubModel

# --- C1 / C2: pgvector index -------------------------------------------------------------


class _FakeCursor:
    def __init__(self, rows: list[tuple[Any, ...]]) -> None:
        self._rows = rows

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self._rows


class _FakeConn:
    def __init__(self, pool: _FakePool) -> None:
        self._pool = pool

    async def execute(self, sql: str, params: tuple[Any, ...]) -> _FakeCursor:
        self._pool.calls.append((sql, params))
        return _FakeCursor(self._pool.rows)


class _FakeConnCtx:
    def __init__(self, pool: _FakePool) -> None:
        self._pool = pool

    async def __aenter__(self) -> _FakeConn:
        return _FakeConn(self._pool)

    async def __aexit__(self, *exc: object) -> None:
        return None


class _FakePool:
    def __init__(self, rows: list[tuple[Any, ...]] | None = None) -> None:
        self.calls: list[tuple[str, tuple[Any, ...]]] = []
        self.rows = rows or []

    def connection(self) -> _FakeConnCtx:
        return _FakeConnCtx(self)

    async def close(self) -> None:
        return None


@pytest.mark.asyncio
async def test_pg_index_inserts_record_id_and_upserts() -> None:
    pool = _FakePool()
    index = PgSemanticIndex(dsn="unused", embedder=HashEmbedder(dim=8), column_dim=8, pool=pool)
    await index.add([MemoryRecord(id="fact_abc", kind="semantic", text="postgres")])
    sql, params = pool.calls[0]
    assert params[0] == "fact_abc"  # not a fresh uuid4: dense hits must map back to the record
    assert "ON CONFLICT (id) DO UPDATE" in sql


@pytest.mark.asyncio
async def test_pg_index_query_returns_stored_id() -> None:
    pool = _FakePool(rows=[("fact_abc", "postgres", "hash", "1", True, 0.9)])
    index = PgSemanticIndex(dsn="unused", embedder=HashEmbedder(dim=8), column_dim=8, pool=pool)
    hits = await index.query("postgres", k=1)
    assert hits[0].record.id == "fact_abc"
    assert hits[0].score == pytest.approx(0.9)


def test_pg_index_rejects_dimension_mismatch_at_construction() -> None:
    # The migration's column is vector(1024); the offline HashEmbedder defaults to 256.
    with pytest.raises(EmbeddingDimensionError, match="vector\\(1024\\)"):
        PgSemanticIndex(dsn="unused", embedder=HashEmbedder(), pool=_FakePool())


class _LyingEmbedder:
    name = "liar"
    version = "1"
    dim = 4

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [[1.0, 0.0] for _ in texts]


@pytest.mark.asyncio
async def test_indexes_reject_vectors_of_the_wrong_width() -> None:
    record = MemoryRecord(id="1", kind="semantic", text="x")
    with pytest.raises(ValueError, match="dim=2"):
        await InMemorySemanticIndex(_LyingEmbedder()).add([record])
    pg = PgSemanticIndex(dsn="unused", embedder=_LyingEmbedder(), column_dim=4, pool=_FakePool())
    with pytest.raises(EmbeddingDimensionError):
        await pg.add([record])


# --- C3: hybrid retrieval ----------------------------------------------------------------


def _rec(record_id: str, text: str, *, valid: bool = True) -> MemoryRecord:
    return MemoryRecord(id=record_id, kind="semantic", text=text, valid=valid)


@pytest.mark.asyncio
async def test_hybrid_never_returns_invalidated_records() -> None:
    retriever = HybridRetriever(semantic=InMemorySemanticIndex(HashEmbedder(dim=128)))
    stale = _rec("old", "the database is mysql", valid=False)
    await retriever.add([stale, _rec("new", "the database is postgres")])
    ids = [hit.record.id for hit in await retriever.query("database mysql", k=5)]
    assert ids == ["new"]


@pytest.mark.asyncio
async def test_hybrid_invalidate_hides_record_from_both_parts() -> None:
    semantic = InMemorySemanticIndex(HashEmbedder(dim=128))
    retriever = HybridRetriever(semantic=semantic)
    await retriever.add([_rec("a", "temporal workflow"), _rec("b", "react frontend")])
    assert retriever.invalidate(["a"]) == 1
    assert [h.record.id for h in await retriever.query("temporal workflow", k=5)] == ["b"]
    assert all(h.record.id != "a" for h in await semantic.query("temporal workflow", k=5))


@pytest.mark.asyncio
async def test_readding_same_id_replaces_without_duplicates() -> None:
    semantic = InMemorySemanticIndex(HashEmbedder(dim=128))
    bm25 = BM25Index()
    retriever = HybridRetriever(semantic=semantic, bm25=bm25)
    await retriever.add([_rec("x", "alpha beta")])
    await retriever.add([_rec("x", "gamma delta"), _rec("x", "gamma delta")])
    assert len(retriever) == len(semantic) == len(bm25) == 1
    hits = await retriever.query("gamma", k=5)
    assert [h.record.text for h in hits] == ["gamma delta"]
    assert bm25.query("alpha") == []  # old text is gone from the lexical part
    assert [h.record.text for h in await semantic.query("alpha", k=5)] == ["gamma delta"]


@pytest.mark.asyncio
async def test_max_records_evicts_oldest_everywhere() -> None:
    semantic = InMemorySemanticIndex(HashEmbedder(dim=128))
    bm25 = BM25Index()
    retriever = HybridRetriever(semantic=semantic, bm25=bm25, max_records=2)
    for i, word in enumerate(["apple", "banana", "cherry"]):
        await retriever.add([_rec(str(i), f"fruit {word}")])
    assert len(retriever) == len(semantic) == len(bm25) == 2
    assert all(h.record.id != "0" for h in await retriever.query("fruit apple", k=5))


@pytest.mark.asyncio
async def test_semantic_index_max_records() -> None:
    index = InMemorySemanticIndex(HashEmbedder(dim=64), max_records=1)
    await index.add([_rec("1", "one"), _rec("2", "two")])
    assert len(index) == 1
    assert [h.record.id for h in await index.query("two", k=5)] == ["2"]


# --- C4: consolidation --------------------------------------------------------------------


class _RecordingModel(StubModel):
    def __init__(self, replies: list[str]) -> None:
        super().__init__(script=[TurnResult(text=r) for r in replies])
        self.prompts: list[str] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.prompts.append(messages[-1].content)
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


@pytest.mark.asyncio
async def test_consolidation_covers_every_episode_in_batches() -> None:
    episodes = [f"episode-{i}" for i in range(5)]
    model = _RecordingModel(['["f1"]', '["f2"]', '["f3"]'])
    result = await consolidate(model=model, episodes=episodes, batch_size=2)
    assert result.batches == 3
    assert result.ok
    assert [f.text for f in result.facts] == ["f1", "f2", "f3"]
    # The first episode is not silently dropped (the old code only looked at the last 200).
    assert "episode-0" in model.prompts[0]
    assert "episode-4" in model.prompts[2]


@pytest.mark.asyncio
async def test_consolidation_parse_failure_keeps_whole_batch_and_signals() -> None:
    episodes = [f"episode-{i}" for i in range(15)]
    model = _RecordingModel(["garbage", '["distilled"]'])
    result = await consolidate(model=model, episodes=episodes, batch_size=12)
    assert result.failed_batches == 1
    assert not result.ok
    texts = [f.text for f in result.facts]
    # All 12 episodes of the failed batch survive verbatim (old code kept only the last 10).
    assert texts[:12] == episodes[:12]
    assert texts[12:] == ["distilled"]


@pytest.mark.asyncio
async def test_consolidation_empty_list_reply_is_not_a_failure() -> None:
    result = await consolidate(model=_RecordingModel(["[]"]), episodes=["noise"])
    assert result.ok
    assert result.facts == []
