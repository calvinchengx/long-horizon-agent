"""Embedders.

- ``HashEmbedder``: deterministic, offline, $0. A hashed bag-of-tokens vector — lexical, NOT
  semantic; it exists so the memory subsystem runs and is testable with no network/key. It is
  explicitly named ``hash`` so it's never mistaken for a real embedding model.
- ``VoyageEmbedder``: the real, paid semantic embedder (Voyage AI), via httpx.
- ``OllamaEmbedder``: a real local semantic embedder served by Ollama (``POST /api/embed``), $0
  and no Python extra; ``connect`` probes the server, the model and its digest, and the memory
  service turns an unreachable server or an unpulled model into lexical-only retrieval.
- ``SentenceTransformerEmbedder``: a real local semantic embedder; needs the ``embeddings`` extra
  (``sentence-transformers``). Constructing it without the extra raises ``ModuleNotFoundError``,
  which the memory service turns into lexical-only retrieval.
- ``PaddedEmbedder``: zero-pads a narrower embedder to pgvector's ``vector(1024)`` column.
"""

from __future__ import annotations

import asyncio
import hashlib
import math
import re
from typing import TYPE_CHECKING, Any

import httpx

if TYPE_CHECKING:
    from lha.contracts.memory import Embedder

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


class OllamaUnavailableError(RuntimeError):
    """Ollama is unreachable, or the embedding model is not pulled."""


class OllamaEmbedder:
    """A local semantic embedder served by Ollama (``POST /api/embed``): real embeddings at $0.

    Build it with ``await OllamaEmbedder.connect(...)``: that checks the server is reachable and
    the model is pulled (``GET /api/tags``), pins the model's digest into ``version`` (so a
    re-pulled model with different weights is never compared with vectors from the old one), and
    measures ``dim`` by embedding a probe text. Any failure raises ``OllamaUnavailableError``;
    the memory service then runs lexical-only.
    """

    BATCH = 64

    def __init__(
        self,
        *,
        model: str,
        dim: int,
        digest: str = "",
        base_url: str = "http://localhost:11434",
        client: httpx.AsyncClient | None = None,
        timeout_s: float = 30.0,
    ) -> None:
        self.name = f"ollama:{model}"
        self.version = f"{model}@{digest[:12]}" if digest else model
        self.dim = dim
        self._model = model
        self._base = base_url.rstrip("/")
        self._client = client or httpx.AsyncClient(timeout=timeout_s, trust_env=False)
        self._owned = client is None

    @classmethod
    async def connect(
        cls,
        *,
        model: str,
        base_url: str = "http://localhost:11434",
        client: httpx.AsyncClient | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        timeout_s: float = 30.0,
    ) -> OllamaEmbedder:
        """Probe the server and model; raise ``OllamaUnavailableError`` if unusable.

        A ``client`` stays the caller's; otherwise the embedder owns (and ``aclose`` closes) the
        client it opens here (over ``transport``, a test seam).
        """
        http = client or httpx.AsyncClient(timeout=timeout_s, trust_env=False, transport=transport)
        base = base_url.rstrip("/")
        try:
            try:
                resp = await http.get(f"{base}/api/tags")
                resp.raise_for_status()
                tags = resp.json().get("models", [])
            except (httpx.HTTPError, ValueError, AttributeError) as exc:
                raise OllamaUnavailableError(
                    f"ollama unreachable at {base} ({type(exc).__name__}: {exc})"
                ) from exc
            digest = _find_digest(tags, model)
            if digest is None:
                raise OllamaUnavailableError(
                    f"ollama model {model!r} is not pulled at {base} (run `ollama pull {model}`)"
                )
            probe = cls(model=model, dim=0, digest=digest, base_url=base, client=http)
            try:
                (vector,) = await probe.embed(["dimension probe"])
            except (httpx.HTTPError, ValueError, KeyError, TypeError) as exc:
                raise OllamaUnavailableError(
                    f"ollama could not embed with {model!r} ({type(exc).__name__}: {exc})"
                ) from exc
            if not vector:
                raise OllamaUnavailableError(f"ollama returned an empty vector for {model!r}")
        except BaseException:
            if client is None:
                await http.aclose()
            raise
        embedder = cls(model=model, dim=len(vector), digest=digest, base_url=base, client=http)
        embedder._owned = client is None
        return embedder

    async def embed(self, texts: list[str]) -> list[list[float]]:
        out: list[list[float]] = []
        for start in range(0, len(texts), self.BATCH):
            batch = texts[start : start + self.BATCH]
            resp = await self._client.post(
                f"{self._base}/api/embed", json={"model": self._model, "input": batch}
            )
            resp.raise_for_status()
            vectors = resp.json()["embeddings"]
            if len(vectors) != len(batch):
                raise ValueError(f"ollama returned {len(vectors)} vectors for {len(batch)} texts")
            out.extend([float(x) for x in vector] for vector in vectors)
        return out

    async def aclose(self) -> None:
        if self._owned:
            await self._client.aclose()


def _find_digest(tags: object, model: str) -> str | None:
    """The digest of ``model`` in an ``/api/tags`` listing (``name`` or ``name:latest``)."""
    wanted = {model, f"{model}:latest"} if ":" not in model else {model}
    for entry in tags if isinstance(tags, list) else []:
        if not isinstance(entry, dict):
            continue
        names = {str(entry.get("name", "")), str(entry.get("model", ""))}
        if names & wanted:
            return str(entry.get("digest", ""))
    return None


class PaddedEmbedder:
    """Zero-pads another embedder's vectors to ``dim`` (pgvector's fixed ``vector(N)`` column).

    Appending zeros changes neither dot products nor norms, so cosine similarity between padded
    vectors equals that between the originals. Name and version are the inner embedder's: every
    vector a store holds for that model+version is padded the same way.
    """

    def __init__(self, inner: Embedder, dim: int) -> None:
        if inner.dim > dim:
            raise ValueError(f"cannot pad dim={inner.dim} vectors down to {dim}")
        self.inner = inner
        self.name = inner.name
        self.version = inner.version
        self.dim = dim

    async def embed(self, texts: list[str]) -> list[list[float]]:
        vectors = await self.inner.embed(texts)
        return [[*v, *([0.0] * (self.dim - len(v)))] for v in vectors]

    async def aclose(self) -> None:
        close = getattr(self.inner, "aclose", None)
        if close is not None:
            await close()


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
