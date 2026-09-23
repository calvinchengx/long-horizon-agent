"""Embedders.

- ``HashEmbedder``: deterministic, offline, $0. A hashed bag-of-tokens vector — lexical, NOT
  semantic; it exists so the memory subsystem runs and is testable with no network/key. It is
  explicitly named ``hash`` so it's never mistaken for a real embedding model.
- ``VoyageEmbedder``: the real, paid semantic embedder (Voyage AI), via httpx.
- ``SentenceTransformerEmbedder``: a real local semantic embedder; needs the ``embeddings`` extra
  (``sentence-transformers``). Constructing it without the extra raises ``ModuleNotFoundError``,
  which the memory service turns into lexical-only retrieval.
"""

from __future__ import annotations

import asyncio
import hashlib
import math
import re
from typing import Any

import httpx

_TOKEN = re.compile(r"[a-z0-9]+")


class HashEmbedder:
    """A deterministic, offline embedder (lexical hashing). For dev/CI/offline only."""

    def __init__(self, dim: int = 256) -> None:
        self.name = "hash"
        self.version = "1"
        self.dim = dim

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [self._embed_one(text) for text in texts]

    def _embed_one(self, text: str) -> list[float]:
        vec = [0.0] * self.dim
        for token in _TOKEN.findall(text.lower()):
            bucket = int(hashlib.sha1(token.encode("utf-8")).hexdigest(), 16) % self.dim
            vec[bucket] += 1.0
        norm = math.sqrt(sum(value * value for value in vec)) or 1.0
        return [value / norm for value in vec]


class VoyageEmbedder:
    """The real Voyage AI embedder."""

    ENDPOINT = "https://api.voyageai.com/v1/embeddings"

    def __init__(
        self,
        *,
        api_key: str,
        model_name: str = "voyage-3",
        dim: int = 1024,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.name = f"voyage:{model_name}"
        self.version = model_name
        self.dim = dim
        self._api_key = api_key
        self._model = model_name
        self._client = client or httpx.AsyncClient(timeout=60.0)

    async def embed(self, texts: list[str]) -> list[list[float]]:
        resp = await self._client.post(
            self.ENDPOINT,
            headers={"Authorization": f"Bearer {self._api_key}"},
            json={"input": texts, "model": self._model},
        )
        resp.raise_for_status()
        data = resp.json()
        items = sorted(data["data"], key=lambda d: d["index"])
        return [list(item["embedding"]) for item in items]


class SentenceTransformerEmbedder:
    """A local semantic embedder via sentence-transformers (the ``embeddings`` extra)."""

    def __init__(self, model_name: str = "BAAI/bge-m3") -> None:
        from sentence_transformers import SentenceTransformer

        self._model: Any = SentenceTransformer(model_name)
        self.name = f"st:{model_name}"
        self.version = model_name
        self.dim = int(self._model.get_sentence_embedding_dimension() or 0)

    async def embed(self, texts: list[str]) -> list[list[float]]:
        vectors = await asyncio.to_thread(self._model.encode, texts, normalize_embeddings=True)
        return [[float(x) for x in vector] for vector in vectors]
