"""The memory contracts.

Tiered memory keeps the agent coherent across thousands of steps and many context resets:
- **episodic** — what happened (append-only event log; the git anchor already holds this);
- **semantic** — distilled facts about the repo/domain, retrieved by similarity;
- **procedural** — reusable, self-verified *skills* (code + preconditions).

An ``Embedder`` turns text into vectors; a ``SemanticIndex`` stores + retrieves by similarity.
Every record carries the embedding model+version so vectors are never compared across versions
(a silent-corruption trap over a long run).
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from pydantic import BaseModel, Field


@runtime_checkable
class Embedder(Protocol):
    """Turns text into vectors. Implementations: hash (offline/$0), Voyage (paid), ..."""

    name: str
    version: str
    dim: int

    async def embed(self, texts: list[str]) -> list[list[float]]:
        """Return one vector per input text (each of length ``dim``)."""
        ...


class MemoryRecord(BaseModel):
    """A single memory item."""

    id: str
    kind: str  # "episodic" | "semantic" | "procedural"
    text: str
    metadata: dict[str, str] = Field(default_factory=dict)
    embedding_model: str | None = None
    embedding_version: str | None = None
    valid: bool = True  # soft-invalidation (conservative forgetting; never hard-delete)


class RetrievalHit(BaseModel):
    """A retrieved record with its similarity score."""

    record: MemoryRecord
    score: float


@runtime_checkable
class SemanticIndex(Protocol):
    """Stores records and retrieves them by semantic similarity."""

    async def add(self, records: list[MemoryRecord]) -> None: ...

    async def query(self, text: str, *, k: int = 5) -> list[RetrievalHit]: ...
