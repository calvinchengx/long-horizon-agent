"""Tests for the tiered-memory subsystem (offline, deterministic via HashEmbedder)."""

from __future__ import annotations

import pytest

from lha.contracts.memory import MemoryRecord
from lha.memory.embeddings import HashEmbedder
from lha.memory.semantic_memory import InMemorySemanticIndex, cosine
from lha.memory.skills import InMemorySkillStore, Skill, SkillNotVerifiedError


@pytest.mark.asyncio
async def test_hash_embedder_is_deterministic_and_unit_norm() -> None:
    embedder = HashEmbedder(dim=64)
    v1 = (await embedder.embed(["hello world"]))[0]
    v2 = (await embedder.embed(["hello world"]))[0]
    assert v1 == v2
    assert len(v1) == 64
    assert abs(sum(x * x for x in v1) - 1.0) < 1e-9


@pytest.mark.asyncio
async def test_semantic_index_retrieves_most_similar() -> None:
    index = InMemorySemanticIndex(HashEmbedder(dim=1024))
    await index.add(
        [
            MemoryRecord(id="1", kind="semantic", text="the database uses postgres and pgvector"),
            MemoryRecord(id="2", kind="semantic", text="the frontend uses react and typescript"),
        ]
    )
    hits = await index.query("how is data stored in postgres", k=1)
    assert len(hits) == 1
    assert hits[0].record.id == "1"


@pytest.mark.asyncio
async def test_semantic_index_stamps_model_version() -> None:
    index = InMemorySemanticIndex(HashEmbedder())
    await index.add([MemoryRecord(id="1", kind="semantic", text="x")])
    hits = await index.query("x", k=1)
    assert hits[0].record.embedding_model == "hash"
    assert hits[0].record.embedding_version == "1"


@pytest.mark.asyncio
async def test_soft_invalidated_records_excluded() -> None:
    index = InMemorySemanticIndex(HashEmbedder())
    await index.add([MemoryRecord(id="1", kind="semantic", text="postgres", valid=False)])
    assert await index.query("postgres", k=5) == []


def test_cosine_basics() -> None:
    assert cosine([1.0, 0.0], [1.0, 0.0]) == pytest.approx(1.0)
    assert cosine([1.0, 0.0], [0.0, 1.0]) == pytest.approx(0.0)
    assert cosine([0.0, 0.0], [1.0, 0.0]) == 0.0  # degenerate vector
    with pytest.raises(ValueError, match="length mismatch"):
        cosine([1.0, 0.0], [])


@pytest.mark.asyncio
async def test_skill_store_requires_verification_then_finds() -> None:
    store = InMemorySkillStore(HashEmbedder())
    with pytest.raises(SkillNotVerifiedError):
        await store.add(
            Skill(id="s1", name="run tests", description="run pytest", code="...", verified=False)
        )
    await store.add(
        Skill(
            id="s1",
            name="run tests",
            description="run the pytest suite with uv",
            code="uv run pytest",
            verified=True,
        )
    )
    found = await store.find("how do I execute the test suite", k=1)
    assert found
    assert found[0].id == "s1"
