"""The Voyage embedder (``LHA_MEMORY_EMBEDDER=voyage``): probe, request bodies and batching,
``input_type`` for documents vs queries, retries on 429/5xx, 401/403 without retry, key
redaction, egress (https, public addresses, pinned dialling), pgvector width handling and
degradation to lexical — over a mocked Voyage API (no network)."""

from __future__ import annotations

import json
import math
import sys
import types
from collections.abc import Iterable
from pathlib import Path

import httpcore
import httpx
import pytest

from lha.config import Settings
from lha.contracts.state import ChecklistItem, SituationSnapshot
from lha.memory.embeddings import (
    VOYAGE_BATCH_TEXTS,
    VOYAGE_ENDPOINT,
    HashEmbedder,
    PaddedEmbedder,
    VoyageEmbedder,
    VoyageUnavailableError,
    embed_queries,
    parse_voyage_response,
)
from lha.memory.semantic_memory import InMemorySemanticIndex, cosine
from lha.memory.service import CycleObservation, embedding_model, open_mission_memory
from lha.obs.events import TraceRecorder
from lha.obs.redact import redact_text
from lha.persistence.sqlite import SqliteStore
from lha.state import git_ops

KEY = "pa-" + "k3y" * 12  # looks like a Voyage key (redact_text masks it)
PUBLIC = "93.184.216.34"
_AXES = {
    "port": 0, "socket": 0, "listen": 0,
    "config": 1, "settings": 1, "parser": 1,
    "database": 2, "sql": 2,
}  # fmt: skip


def _vector(text: str, dim: int) -> list[float]:
    vec = [0.0] * dim
    for word in text.lower().replace(".", " ").replace("_", " ").split():
        if word in _AXES:
            vec[_AXES[word]] += 1.0
    vec[dim - 1] = 0.01  # never all-zero
    norm = math.sqrt(sum(v * v for v in vec))
    return [v / norm for v in vec]


class FakeVoyage:
    """``POST /v1/embeddings`` like the Voyage API, with scripted failures."""

    def __init__(self, dim: int = 8) -> None:
        self.dim = dim
        self.bodies: list[dict[str, object]] = []
        self.headers: list[httpx.Headers] = []
        self.urls: list[str] = []
        self.statuses: list[int] = []  # served (then dropped) before answering 200
        self.always: int | None = None  # a status every request gets
        self.down = False
        self.reverse = False  # answer data[] in reverse index order

    def handler(self, request: httpx.Request) -> httpx.Response:
        if self.down:
            raise httpx.ConnectError("connection refused")
        body = json.loads(request.content)
        self.bodies.append(body)
        self.headers.append(request.headers)
        self.urls.append(str(request.url))
        status = self.always or (self.statuses.pop(0) if self.statuses else 200)
        if status != 200:
            return httpx.Response(status, json={"detail": "nope"}, headers={"retry-after": "7"})
        data = [
            {"object": "embedding", "index": i, "embedding": _vector(t, self.dim)}
            for i, t in enumerate(body["input"])
        ]
        if self.reverse:
            data.reverse()
        return httpx.Response(200, json={"object": "list", "data": data, "model": body["model"]})

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handler)


class Sleeps:
    def __init__(self) -> None:
        self.calls: list[float] = []

    async def __call__(self, seconds: float) -> None:
        self.calls.append(seconds)


async def _public(host: str, port: int) -> list[str]:
    return [PUBLIC]


async def _connect(fake: FakeVoyage, **kwargs: object) -> VoyageEmbedder:
    options: dict[str, object] = {
        "api_key": KEY,
        "resolver": _public,
        "transport": fake.transport(),
        "sleep": Sleeps(),
    }
    options.update(kwargs)
    return await VoyageEmbedder.connect(**options)  # type: ignore[arg-type]


# --- the embedder -----------------------------------------------------------------------------
async def test_connect_probes_the_key_and_dimension() -> None:
    fake = FakeVoyage(dim=8)
    embedder = await _connect(fake)
    assert (embedder.name, embedder.version, embedder.dim) == ("voyage:voyage-4", "voyage-4", 8)
    assert fake.bodies == [
        {"input": ["dimension probe"], "model": "voyage-4", "input_type": "document"}
    ]
    assert fake.urls == [VOYAGE_ENDPOINT]
    assert fake.headers[0]["authorization"] == f"Bearer {KEY}"
    assert fake.headers[0]["content-type"] == "application/json"
    assert KEY not in repr(embedder) and "voyage-4" in repr(embedder)
    await embedder.aclose()
    assert embedder._client.is_closed


async def test_documents_and_queries_use_their_input_type_and_batches() -> None:
    fake = FakeVoyage()
    fake.reverse = True  # vectors are put back in input order by data[].index
    embedder = await _connect(fake, model="voyage-3.5-lite")
    texts = [f"port {i}" if i % 2 else f"sql {i}" for i in range(VOYAGE_BATCH_TEXTS + 2)]
    vectors = await embedder.embed(texts)
    assert [len(b["input"]) for b in fake.bodies[1:]] == [VOYAGE_BATCH_TEXTS, 2]  # type: ignore[arg-type]
    assert {b["input_type"] for b in fake.bodies[1:]} == {"document"}
    assert vectors[1] == _vector("port 1", 8) and vectors[0] == _vector("sql 0", 8)
    (query,) = await embedder.embed_query(["listen socket"])
    assert fake.bodies[-1] == {
        "input": ["listen socket"],
        "model": "voyage-3.5-lite",
        "input_type": "query",
    }
    assert cosine(query, vectors[1]) > 0.99
    assert await embedder.embed([]) == []  # nothing to send
    await embedder.aclose()


async def test_embed_queries_falls_back_to_embed_and_padding_keeps_the_input_type() -> None:
    assert await embed_queries(HashEmbedder(dim=4), ["a"]) == await HashEmbedder(dim=4).embed(["a"])
    fake = FakeVoyage(dim=8)
    padded = PaddedEmbedder(await _connect(fake), 16)
    (vector,) = await padded.embed_query(["listen port"])
    assert len(vector) == 16 and fake.bodies[-1]["input_type"] == "query"
    (doc,) = await padded.embed(["listen port"])
    assert len(doc) == 16 and fake.bodies[-1]["input_type"] == "document"
    index = InMemorySemanticIndex(padded)
    assert await index.query("anything") == []  # empty index: no request
    await padded.aclose()


async def test_transient_errors_are_retried_with_backoff() -> None:
    fake = FakeVoyage()
    fake.statuses = [429, 503]
    sleeps = Sleeps()
    embedder = await _connect(fake, sleep=sleeps)
    assert embedder.dim == 8 and len(fake.bodies) == 3
    assert sleeps.calls == [7.0, 7.0]  # the server's Retry-After, not blind doubling
    await embedder.aclose()


@pytest.mark.parametrize(
    ("setup", "message", "requests"),
    [
        (lambda f: setattr(f, "always", 401), "rejected the API key for 'voyage-4' (HTTP 401)", 1),
        (lambda f: setattr(f, "always", 403), "rejected the API key for 'voyage-4' (HTTP 403)", 1),
        (lambda f: setattr(f, "always", 400), "could not embed with 'voyage-4' (HTTP 400)", 1),
        (lambda f: setattr(f, "always", 429), "could not embed with 'voyage-4' (HTTP 429)", 4),
        (lambda f: setattr(f, "always", 500), "could not embed with 'voyage-4' (HTTP 500)", 4),
        (lambda f: setattr(f, "down", True), f"voyage unreachable at {VOYAGE_ENDPOINT}", 0),
    ],
)
async def test_connect_reports_why_voyage_is_unusable(
    setup: object, message: str, requests: int
) -> None:
    fake = FakeVoyage()
    setup(fake)  # type: ignore[operator]
    with pytest.raises(VoyageUnavailableError) as caught:
        await _connect(fake)
    assert message in str(caught.value) and KEY not in str(caught.value)
    assert caught.value.__cause__ is None  # nothing chained that could quote the key
    assert len(fake.bodies) == requests


async def test_a_malformed_answer_makes_voyage_unusable() -> None:
    def short(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"data": []})

    with pytest.raises(VoyageUnavailableError, match="0 vectors for 1 texts"):
        await VoyageEmbedder.connect(
            api_key=KEY, resolver=_public, transport=httpx.MockTransport(short)
        )
    fake = FakeVoyage(dim=8)
    embedder = await _connect(fake)
    fake.dim = 4  # the API changed width mid-run: never mix widths in one index
    with pytest.raises(ValueError, match="dim is not 8"):
        await embedder.embed(["port"])
    await embedder.aclose()
    with pytest.raises(ValueError, match="no data list"):
        parse_voyage_response(None, 1)


async def test_a_missing_key_or_a_refused_endpoint_never_calls_out() -> None:
    fake = FakeVoyage()
    with pytest.raises(VoyageUnavailableError, match="LHA_VOYAGE_API_KEY is not set"):
        await _connect(fake, api_key="  ")
    with pytest.raises(VoyageUnavailableError, match="must use https"):
        await _connect(fake, endpoint="http://api.voyageai.com/v1/embeddings")
    with pytest.raises(VoyageUnavailableError, match="endpoint refused"):
        await _connect(fake, endpoint="https://user:pw@api.voyageai.com/v1/embeddings")

    async def private(host: str, port: int) -> list[str]:
        return ["10.0.0.5"]

    with pytest.raises(VoyageUnavailableError, match="non-public address"):
        await _connect(fake, resolver=private)

    async def unresolvable(host: str, port: int) -> list[str]:
        raise OSError("nodename nor servname provided")

    with pytest.raises(VoyageUnavailableError, match="cannot resolve"):
        await _connect(fake, resolver=unresolvable)
    assert fake.bodies == []


class _RecordingBackend(httpcore.AsyncNetworkBackend):
    """A socket layer that records where it is asked to connect, and refuses."""

    def __init__(self) -> None:
        self.targets: list[tuple[str, int]] = []

    async def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: Iterable[object] | None = None,
    ) -> httpcore.AsyncNetworkStream:
        self.targets.append((host, port))
        raise httpcore.ConnectError("refused by the test")

    async def sleep(self, seconds: float) -> None:
        return None


async def test_connections_dial_only_the_vetted_addresses() -> None:
    backend = _RecordingBackend()
    sleeps = Sleeps()
    with pytest.raises(VoyageUnavailableError, match="unreachable") as caught:
        await VoyageEmbedder.connect(
            api_key=KEY,
            endpoint="https://voyage.example:8443/v1/embeddings",
            resolver=_public,
            network_backend=backend,
            sleep=sleeps,
        )
    # The name was resolved by the egress check; the socket went to that address only (and was
    # retried as a transient connection error).
    assert backend.targets == [(PUBLIC, 8443)] * 4 and len(sleeps.calls) == 3
    assert KEY not in str(caught.value)


def test_the_key_is_a_secret_everywhere() -> None:
    settings = _settings(voyage_api_key=KEY)
    assert KEY not in repr(settings) and settings.redacted()["voyage_api_key"] == "***"
    assert redact_text(f"sent {KEY} to voyage") == "sent *** to voyage"


# --- through the memory service ---------------------------------------------------------------
def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "memory_embedder": "voyage",
        "voyage_api_key": KEY,
        "model_backend": "stub",
    }
    base.update(overrides)
    return Settings(_env_file=None, **base)  # type: ignore[call-arg, arg-type]


def _repo(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    git_ops.init_repo(path)
    (path / "net.py").write_text("SOCKET = 8080  # the socket number\n", encoding="utf-8")
    (path / "db.py").write_text("DATABASE = 'sqlite'  # sql storage\n", encoding="utf-8")
    git_ops.commit_all(path, "init")
    return path


def test_voyage_settings_and_default_model() -> None:
    assert embedding_model(_settings()) == "voyage-4"
    assert embedding_model(_settings(memory_embedding_model="voyage-code-3")) == "voyage-code-3"
    assert Settings(_env_file=None).voyage_api_key is None  # type: ignore[call-arg]
    assert Settings(_env_file=None).voyage_endpoint is None  # type: ignore[call-arg]


async def test_voyage_memory_recalls_by_meaning_and_gates_by_model_version(
    tmp_path: Path,
) -> None:
    fake = FakeVoyage()
    store = SqliteStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(voyage_endpoint="https://voyage.example/v1/embeddings"),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        embedder_transport=fake.transport(),
        embedder_resolver=_public,
    )
    assert memory is not None and memory.mode.label == "hybrid"
    assert memory.embedder is not None and memory.embedder.name == "voyage:voyage-4"
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c1",
        item=ChecklistItem(id="01", description="make the listen port configurable"),
        snapshot=SituationSnapshot(head_sha=""),
    )
    assert "net.py" in block  # meaning, not spelling: only the dense channel links them
    assert set(fake.urls) == {"https://voyage.example/v1/embeddings"}
    # The item text went as a query; repo chunks as documents.
    queries = [b["input"] for b in fake.bodies if b["input_type"] == "query"]
    assert queries == [["make the listen port configurable"]]
    await memory.observe_cycle(
        CycleObservation(
            mission_id="m1", cycle_id="c1", item_id="01", item_description="listen port",
            verdict="passed", verified=True, status="done", attempts=1, head_sha="",
            done_summary="moved the socket number to settings",
        )
    )  # fmt: skip
    rows = await store.memory_vectors(
        "m1", embedding_model="voyage:voyage-4", embedding_version="voyage-4"
    )
    assert rows and all(len(vector) == 8 for _, vector in rows)
    other = await store.memory_vectors(
        "m1", embedding_model="voyage:voyage-3.5", embedding_version="voyage-3.5"
    )
    assert other == []  # another model's vectors are never compared with these
    client = memory.embedder._client  # type: ignore[attr-defined]
    await memory.close()
    assert client.is_closed
    await store.close()


@pytest.mark.parametrize(
    ("overrides", "setup", "reason"),
    [
        ({"voyage_api_key": None}, None, "LHA_VOYAGE_API_KEY is not set"),
        ({}, lambda f: setattr(f, "always", 401), "rejected the API key"),
        ({}, lambda f: setattr(f, "down", True), "voyage unreachable"),
    ],
)
async def test_unusable_voyage_means_lexical_memory_not_a_failure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    overrides: dict[str, object],
    setup: object,
    reason: str,
) -> None:
    monkeypatch.setattr(VoyageEmbedder, "RETRY_BASE_DELAY_S", 0.0)
    fake = FakeVoyage()
    if setup is not None:
        setup(fake)  # type: ignore[operator]
    store = SqliteStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(**overrides),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        recorder=recorder,
        embedder_transport=fake.transport(),
        embedder_resolver=_public,
    )
    assert memory is not None and memory.embedder is None and memory.mode.label == "lexical"
    (event,) = [e for e in recorder.events if e.kind == "memory_degraded"]
    assert reason in str(event.data["reason"]) and KEY not in str(event.data["reason"])
    await memory.close()
    await store.close()


async def test_voyage_failing_mid_run_degrades_to_lexical(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(VoyageEmbedder, "RETRY_BASE_DELAY_S", 0.0)
    monkeypatch.setattr(VoyageEmbedder, "RETRY_MAX_DELAY_S", 0.0)
    fake = FakeVoyage()
    store = SqliteStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        recorder=recorder,
        embedder_transport=fake.transport(),
        embedder_resolver=_public,
    )
    assert memory is not None and memory.mode.dense
    fake.always = 503  # the API stops serving after the run started (retried, then dropped)
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c1",
        item=ChecklistItem(id="01", description="the sql database"),
        snapshot=SituationSnapshot(head_sha=""),
    )
    assert "db.py" in block  # lexical + git grep still answer
    assert memory.embedder is None and memory.mode.label == "lexical"
    assert len(fake.bodies) == 1 + 4  # probe, then one call and its three retries
    assert any(e.kind == "memory_degraded" for e in recorder.events)
    await memory.close()
    await store.close()


class _FakePgStore(SqliteStore):
    backend = "postgres"


@pytest.mark.parametrize(("dim", "label"), [(1024, "hybrid"), (512, "hybrid"), (2048, "lexical")])
async def test_voyage_width_against_the_pgvector_column(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, dim: int, label: str
) -> None:
    monkeypatch.setitem(sys.modules, "pgvector", types.ModuleType("pgvector"))
    fake = FakeVoyage(dim=dim)
    store = _FakePgStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=tmp_path,
        mission_id="m",
        recorder=recorder,
        embedder_transport=fake.transport(),
        embedder_resolver=_public,
    )
    assert memory is not None and memory.mode.label == label
    if dim < 1024:
        assert isinstance(memory.embedder, PaddedEmbedder) and memory.embedder.dim == 1024
        assert memory.embedder.version == "voyage-4"
    elif dim > 1024:  # never truncated: a wider model runs lexical-only
        (event,) = [e for e in recorder.events if e.kind == "memory_degraded"]
        assert "embedder dim 2048 > vector(1024) column" in str(event.data["reason"])
    await memory.close()
    await store.close()
