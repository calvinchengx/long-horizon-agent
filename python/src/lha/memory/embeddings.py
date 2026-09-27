"""Embedders.

- ``HashEmbedder``: deterministic, offline, $0. A hashed bag-of-tokens vector — lexical, NOT
  semantic; it exists so the memory subsystem runs and is testable with no network/key. It is
  explicitly named ``hash`` so it's never mistaken for a real embedding model.
- ``VoyageEmbedder``: the paid Voyage AI API (``POST /v1/embeddings``) over DNS-pinned,
  egress-checked HTTPS; ``connect`` probes the key, the endpoint and the dimension, and the memory
  service turns an unreachable API or a refused key into lexical-only retrieval.
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
import functools
import hashlib
import json
import math
import re
from typing import TYPE_CHECKING, Any

import httpcore
import httpx

from lha.model.retry import Sleep, with_retries
from lha.obs.redact import REDACTED
from lha.safety.egress import (
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    Resolver,
    check_resolved_addresses,
    parse_url,
    system_resolver,
)
from lha.safety.pinned_http import PinnedNetworkBackend, PinnedTransport

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


VOYAGE_ENDPOINT = "https://api.voyageai.com/v1/embeddings"
#: ``LHA_MEMORY_EMBEDDING_MODEL`` default for ``voyage``: Voyage's general-purpose model, 1024-wide
#: by default (exactly pgvector's ``vector(1024)`` column; narrower outputs are zero-padded).
VOYAGE_DEFAULT_MODEL = "voyage-4"
#: Texts per request. The API accepts up to 1000; smaller batches keep one failure cheap.
VOYAGE_BATCH_TEXTS = 128
#: Characters (code points) per request. The API caps tokens per request (120K for the ``-large``
#: and code models, 320K / 1M for the others); ordinary text has fewer tokens than characters, so
#: this stays under every model's cap.
VOYAGE_BATCH_CHARS = 100_000
#: HTTP timeout of the Voyage embedder (probe and every embed call).
VOYAGE_TIMEOUT_S = 30.0
_VOYAGE_KEY_PLACEHOLDER = "{{LHA_VOYAGE_API_KEY}}"


class VoyageUnavailableError(RuntimeError):
    """The Voyage API is unreachable, refuses the key, or egress refuses its endpoint."""


def voyage_batches(
    texts: list[str],
    *,
    max_texts: int = VOYAGE_BATCH_TEXTS,
    max_chars: int = VOYAGE_BATCH_CHARS,
) -> list[list[str]]:
    """Split ``texts`` in order into batches of at most ``max_texts`` texts and ``max_chars``
    characters (code points); a single longer text travels alone (the API truncates it)."""
    batches: list[list[str]] = []
    current: list[str] = []
    chars = 0
    for text in texts:
        if current and (len(current) >= max_texts or chars + len(text) > max_chars):
            batches.append(current)
            current, chars = [], 0
        current.append(text)
        chars += len(text)
    if current:
        batches.append(current)
    return batches


def voyage_request_body(model: str, texts: list[str], input_type: str) -> dict[str, object]:
    """The JSON body of one ``POST /v1/embeddings`` (``input_type``: ``document`` or ``query``)."""
    return {"input": texts, "model": model, "input_type": input_type}


def parse_voyage_response(data: object, count: int) -> list[list[float]]:
    """The vectors of a ``/v1/embeddings`` answer, in input order (by ``data[].index``).

    Raises ``ValueError`` for anything but exactly ``count`` non-empty numeric vectors.
    """
    items = data.get("data") if isinstance(data, dict) else None
    if not isinstance(items, list):
        raise ValueError("voyage response has no data list")
    by_index: dict[int, list[float]] = {}
    for item in items:
        if not isinstance(item, dict):
            raise ValueError("voyage response item is not an object")
        index, vector = item.get("index"), item.get("embedding")
        if not isinstance(index, int) or isinstance(index, bool) or not 0 <= index < count:
            raise ValueError(f"voyage response has an invalid index {index!r}")
        if not isinstance(vector, list) or not vector:
            raise ValueError(f"voyage response has no embedding at index {index}")
        if not all(isinstance(x, int | float) and not isinstance(x, bool) for x in vector):
            raise ValueError(f"voyage response has a non-numeric embedding at index {index}")
        if index in by_index:
            raise ValueError(f"voyage response repeats index {index}")
        by_index[index] = [float(x) for x in vector]
    if len(by_index) != count:
        raise ValueError(f"voyage returned {len(by_index)} vectors for {count} texts")
    return [by_index[i] for i in range(count)]


def voyage_status_message(model: str, status: int) -> str:
    """Why an HTTP ``status`` from the API makes the embedder unusable (the probe's error)."""
    if status in (401, 403):
        return f"voyage rejected the API key for {model!r} (HTTP {status})"
    return f"voyage could not embed with {model!r} (HTTP {status})"


class VoyageEmbedder:
    """The Voyage AI embedder (``POST /v1/embeddings``): paid, over public HTTPS only.

    Build it with ``await VoyageEmbedder.connect(...)``: that checks the endpoint against a
    one-host egress policy and measures ``dim`` by embedding a probe text. A missing or refused
    key, an unreachable API or a refused endpoint raises ``VoyageUnavailableError``; the memory
    service then runs lexical-only.

    Every request re-resolves the endpoint host, refuses it unless every address is public, and
    dials only those vetted addresses (``PinnedTransport``: no DNS rebinding, no proxy from the
    environment); TLS and the ``Host`` header keep the hostname. The key is bound by a
    ``CredentialBroker`` to the endpoint host, never in ``repr``, and scrubbed from error
    messages. Documents are embedded with ``input_type="document"`` (``embed``), search text with
    ``input_type="query"`` (``embed_query``). Transient failures (408/409/429/5xx, timeouts,
    connection errors) are retried with backoff (``Retry-After`` honoured); 401/403 are not.
    The version is the model name: Voyage publishes no weight digest, and a model name's weights
    are fixed.
    """

    MAX_RETRIES = 3
    RETRY_BASE_DELAY_S = 1.0
    RETRY_MAX_DELAY_S = 30.0

    def __init__(
        self,
        *,
        api_key: str,
        model: str = VOYAGE_DEFAULT_MODEL,
        dim: int = 1024,
        endpoint: str | None = None,
        timeout_s: float = VOYAGE_TIMEOUT_S,
        resolver: Resolver | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        network_backend: httpcore.AsyncNetworkBackend | None = None,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        self.name = f"voyage:{model}"
        self.version = model
        self.dim = dim
        self._model = model
        self._endpoint = endpoint or VOYAGE_ENDPOINT
        target = parse_url(self._endpoint)  # EgressDenied for a malformed endpoint
        if target.scheme != "https":
            raise EgressDenied(f"the voyage endpoint must use https: {self._endpoint!r}")
        # The embedder may reach exactly its endpoint (scheme, host and port), nothing else.
        self._policy = EgressPolicy(
            allow_hosts={target.host}, allow_schemes={"https"}, allow_ports={target.port}
        )
        self._api_key = api_key
        self._broker = CredentialBroker()
        self._broker.register(_VOYAGE_KEY_PLACEHOLDER, api_key, hosts=[target.host])
        self._resolver = resolver or system_resolver
        self._sleep = sleep
        self._backend: PinnedNetworkBackend | None = None
        if transport is None:  # ``transport`` is a test seam (an ``httpx.MockTransport``)
            self._backend = PinnedNetworkBackend(network_backend)
            transport = PinnedTransport(self._backend)
        self._client = httpx.AsyncClient(
            timeout=timeout_s, follow_redirects=False, trust_env=False, transport=transport
        )

    def __repr__(self) -> str:
        return f"VoyageEmbedder(model={self._model!r}, dim={self.dim}, endpoint={self._endpoint!r})"

    @classmethod
    async def connect(
        cls,
        *,
        api_key: str,
        model: str = VOYAGE_DEFAULT_MODEL,
        endpoint: str | None = None,
        timeout_s: float = VOYAGE_TIMEOUT_S,
        resolver: Resolver | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        network_backend: httpcore.AsyncNetworkBackend | None = None,
        sleep: Sleep = asyncio.sleep,
    ) -> VoyageEmbedder:
        """Check the key, endpoint and model; raise ``VoyageUnavailableError`` if unusable."""
        if not api_key.strip():
            raise VoyageUnavailableError("voyage: LHA_VOYAGE_API_KEY is not set")
        try:
            embedder = cls(
                api_key=api_key,
                model=model,
                dim=0,
                endpoint=endpoint,
                timeout_s=timeout_s,
                resolver=resolver,
                transport=transport,
                network_backend=network_backend,
                sleep=sleep,
            )
        except EgressDenied as exc:
            raise VoyageUnavailableError(f"voyage endpoint refused: {exc}") from exc
        try:
            (vector,) = await embedder.embed(["dimension probe"])
        except Exception as exc:
            await embedder.aclose()
            raise VoyageUnavailableError(embedder._describe(exc)) from None
        except BaseException:
            await embedder.aclose()
            raise
        embedder.dim = len(vector)
        return embedder

    def _describe(self, exc: Exception) -> str:
        """Why the probe failed (no key in it; the cause is not chained, it may quote one)."""
        if isinstance(exc, httpx.HTTPStatusError):
            why = voyage_status_message(self._model, exc.response.status_code)
        elif isinstance(exc, EgressDenied):
            why = f"voyage endpoint refused: {exc}"
        elif isinstance(exc, httpx.TransportError | OSError):
            why = f"voyage unreachable at {self._endpoint} ({type(exc).__name__}: {exc})"
        else:
            why = f"voyage could not embed with {self._model!r} ({type(exc).__name__}: {exc})"
        return self._scrub(why)

    def _scrub(self, text: str) -> str:
        return text.replace(self._api_key, REDACTED) if self._api_key else text

    async def embed(self, texts: list[str]) -> list[list[float]]:
        """Vectors for documents (``input_type="document"``)."""
        return await self._embed(texts, "document")

    async def embed_query(self, texts: list[str]) -> list[list[float]]:
        """Vectors for search text (``input_type="query"``)."""
        return await self._embed(texts, "query")

    async def _embed(self, texts: list[str], input_type: str) -> list[list[float]]:
        out: list[list[float]] = []
        for batch in voyage_batches(texts):
            body = voyage_request_body(self._model, batch, input_type)
            data = await with_retries(
                functools.partial(self._post, body),
                max_retries=self.MAX_RETRIES,
                base_delay_s=self.RETRY_BASE_DELAY_S,
                max_delay_s=self.RETRY_MAX_DELAY_S,
                sleep=self._sleep,
            )
            vectors = parse_voyage_response(data, len(batch))
            if self.dim and any(len(v) != self.dim for v in vectors):
                raise ValueError(f"voyage returned a vector whose dim is not {self.dim}")
            out.extend(vectors)
        return out

    async def _post(self, body: dict[str, object]) -> object:
        parsed = self._policy.check(self._endpoint)
        addresses = await check_resolved_addresses(parsed.host, parsed.port, self._resolver)
        if self._backend is not None:
            self._backend.pin(parsed.host, addresses)  # the connection dials only these
        headers = self._broker.resolve_headers(
            {
                "authorization": f"Bearer {_VOYAGE_KEY_PLACEHOLDER}",
                "content-type": "application/json",
            },
            host=parsed.host,
        )
        content = json.dumps(body, separators=(",", ":")).encode("utf-8")
        resp = await self._client.post(self._endpoint, content=content, headers=headers)
        resp.raise_for_status()
        return resp.json()

    async def aclose(self) -> None:
        await self._client.aclose()


async def embed_queries(embedder: Embedder, texts: list[str]) -> list[list[float]]:
    """Vectors for search text: ``embed_query`` when the embedder tells queries from documents
    (Voyage's ``input_type``), else ``embed``."""
    embed_query = getattr(embedder, "embed_query", None)
    if embed_query is None:
        return await embedder.embed(texts)
    return await embed_query(texts)


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
        return self._pad(await self.inner.embed(texts))

    async def embed_query(self, texts: list[str]) -> list[list[float]]:
        return self._pad(await embed_queries(self.inner, texts))

    def _pad(self, vectors: list[list[float]]) -> list[list[float]]:
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
