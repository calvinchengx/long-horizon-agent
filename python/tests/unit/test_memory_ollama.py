"""The Ollama embedder (``LHA_MEMORY_EMBEDDER=ollama``): probe, model+version gating, batching,
degradation to lexical when Ollama is absent, and pgvector width handling — over a mocked Ollama
HTTP API (no Ollama in CI)."""

from __future__ import annotations

import json
import math
import sys
import types
from pathlib import Path

import httpx
import pytest

from lha.config import Settings
from lha.contracts.state import ChecklistItem, SituationSnapshot
from lha.memory import embeddings
from lha.memory.embeddings import OllamaEmbedder, OllamaUnavailableError, PaddedEmbedder
from lha.memory.semantic_memory import cosine
from lha.memory.service import CycleObservation, embedding_model, open_mission_memory
from lha.obs.events import TraceRecorder
from lha.persistence.sqlite import SqliteStore
from lha.state import git_ops

DIGEST = "sha256:0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f"
# A tiny "semantic" space: synonyms share an axis, so meaning (not spelling) decides similarity.
_AXES = {
    "port": 0, "socket": 0, "listen": 0,
    "config": 1, "settings": 1, "parser": 1,
    "database": 2, "sql": 2,
}  # fmt: skip


def _vector(text: str) -> list[float]:
    vec = [0.0] * 8
    for word in text.lower().replace(".", " ").replace("_", " ").split():
        if word in _AXES:
            vec[_AXES[word]] += 1.0
    vec[7] = 0.01  # never all-zero
    norm = math.sqrt(sum(v * v for v in vec))
    return [v / norm for v in vec]


class FakeOllama:
    """``GET /api/tags`` + ``POST /api/embed`` like a local Ollama with one pulled model."""

    def __init__(self, models: tuple[str, ...] = ("nomic-embed-text:latest",)) -> None:
        self.models = models
        self.embed_calls: list[list[str]] = []
        self.down = False
        self.embed_status = 200

    def handler(self, request: httpx.Request) -> httpx.Response:
        if self.down:
            raise httpx.ConnectError("connection refused")
        if request.url.path == "/api/tags":
            return httpx.Response(
                200,
                json={"models": [{"name": m, "model": m, "digest": DIGEST} for m in self.models]},
            )
        if request.url.path == "/api/embed":
            body = json.loads(request.content)
            self.embed_calls.append(list(body["input"]))
            if self.embed_status != 200:
                return httpx.Response(self.embed_status, json={"error": "boom"})
            return httpx.Response(
                200,
                json={"model": body["model"], "embeddings": [_vector(t) for t in body["input"]]},
            )
        return httpx.Response(404)

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handler)


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "memory_embedder": "ollama",
        "ollama_base_url": "http://ollama.test:11434/",
        "model_backend": "stub",
    }
    base.update(overrides)
    return Settings(_env_file=None, **base)  # type: ignore[call-arg, arg-type]


# --- the embedder -----------------------------------------------------------------------------
async def test_connect_probes_the_model_digest_and_dimension(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fake = FakeOllama()
    embedder = await OllamaEmbedder.connect(
        model="nomic-embed-text", base_url="http://ollama.test/", transport=fake.transport()
    )
    assert embedder.name == "ollama:nomic-embed-text"
    assert embedder.version == f"nomic-embed-text@{DIGEST[:12]}"  # re-pulled weights = new version
    assert embedder.dim == 8 and fake.embed_calls == [["dimension probe"]]
    monkeypatch.setattr(OllamaEmbedder, "BATCH", 2)
    vectors = await embedder.embed(["port", "socket", "sql"])
    assert fake.embed_calls[1:] == [["port", "socket"], ["sql"]]  # batched
    assert cosine(vectors[0], vectors[1]) > 0.99 and cosine(vectors[0], vectors[2]) < 0.1
    await embedder.aclose()
    assert embedder._client.is_closed


async def test_connect_accepts_an_explicit_tag_and_a_callers_client() -> None:
    fake = FakeOllama(models=("mxbai-embed-large:335m",))
    async with httpx.AsyncClient(transport=fake.transport()) as client:
        embedder = await OllamaEmbedder.connect(model="mxbai-embed-large:335m", client=client)
        await embedder.aclose()
        assert not client.is_closed  # the caller's client stays open
        with pytest.raises(OllamaUnavailableError, match="not pulled"):
            await OllamaEmbedder.connect(model="mxbai-embed-large", client=client)


@pytest.mark.parametrize(
    ("setup", "message"),
    [
        (lambda f: setattr(f, "down", True), "unreachable"),
        (lambda f: setattr(f, "models", ("llama3:8b",)), "ollama pull nomic-embed-text"),
        (lambda f: setattr(f, "embed_status", 500), "could not embed"),
    ],
)
async def test_connect_reports_why_ollama_is_unusable(setup: object, message: str) -> None:
    fake = FakeOllama()
    setup(fake)  # type: ignore[operator]
    with pytest.raises(OllamaUnavailableError, match=message):
        await OllamaEmbedder.connect(model="nomic-embed-text", transport=fake.transport())


async def test_embed_rejects_a_short_answer_and_an_empty_probe_vector() -> None:
    def short(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json={"models": [{"name": "e:latest", "digest": ""}]})
        return httpx.Response(200, json={"embeddings": [[]]})

    with pytest.raises(OllamaUnavailableError, match="empty vector"):
        await OllamaEmbedder.connect(model="e", transport=httpx.MockTransport(short))
    async with httpx.AsyncClient(transport=httpx.MockTransport(short)) as client:
        embedder = OllamaEmbedder(model="e", dim=3, client=client)
        assert embedder.version == "e"  # no digest known: the model name alone
        with pytest.raises(ValueError, match="1 vectors for 2 texts"):
            await embedder.embed(["a", "b"])


def test_embedding_model_defaults_per_embedder() -> None:
    assert embedding_model(_settings()) == "nomic-embed-text"
    assert embedding_model(_settings(memory_embedder="sentence_transformers")) == "BAAI/bge-m3"
    assert embedding_model(_settings(memory_embedding_model=" bge-m3 ")) == "bge-m3"
    assert embedding_model(_settings(memory_embedder="hash")) == ""
    assert Settings(_env_file=None).memory_embedder == "hash"  # type: ignore[call-arg]


async def test_padded_embedder_keeps_cosine_and_refuses_to_shrink() -> None:
    fake = FakeOllama()
    inner = await OllamaEmbedder.connect(model="nomic-embed-text", transport=fake.transport())
    padded = PaddedEmbedder(inner, 16)
    assert (padded.name, padded.version, padded.dim) == (inner.name, inner.version, 16)
    a, b = await padded.embed(["listen port", "socket settings"])
    raw_a, raw_b = await inner.embed(["listen port", "socket settings"])
    assert len(a) == 16 and cosine(a, b) == pytest.approx(cosine(raw_a, raw_b))
    with pytest.raises(ValueError, match="cannot pad"):
        PaddedEmbedder(inner, 4)
    await padded.aclose()
    assert inner._client.is_closed
    await PaddedEmbedder(embeddings.HashEmbedder(dim=4), 8).aclose()  # nothing to close


# --- through the memory service ---------------------------------------------------------------
def _repo(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    git_ops.init_repo(path)
    (path / "net.py").write_text("SOCKET = 8080  # the socket number\n", encoding="utf-8")
    (path / "db.py").write_text("DATABASE = 'sqlite'  # sql storage\n", encoding="utf-8")
    git_ops.commit_all(path, "init")
    return path


async def test_ollama_memory_recalls_by_meaning_and_gates_by_model_version(
    tmp_path: Path,
) -> None:
    fake = FakeOllama()
    store = SqliteStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        embedder_transport=fake.transport(),
    )
    assert memory is not None and memory.mode.label == "hybrid"
    assert memory.embedder is not None and memory.embedder.name == "ollama:nomic-embed-text"
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c1",
        item=ChecklistItem(id="01", description="make the listen port configurable"),
        snapshot=SituationSnapshot(head_sha=""),
    )
    # "listen port" shares no word with net.py but means the same thing: the dense channel ranks
    # it; BM25 alone could not.
    assert "net.py" in block
    assert "db.py" not in block or block.index("net.py") < block.index("db.py")
    await memory.observe_cycle(
        CycleObservation(
            mission_id="m1", cycle_id="c1", item_id="01", item_description="listen port",
            verdict="passed", verified=True, status="done", attempts=1, head_sha="",
            done_summary="moved the socket number to settings",
        )
    )  # fmt: skip
    rows = await store.memory_vectors(
        "m1",
        embedding_model="ollama:nomic-embed-text",
        embedding_version=f"nomic-embed-text@{DIGEST[:12]}",
    )
    assert rows and all(len(vector) == 8 for _, vector in rows)
    other = await store.memory_vectors(
        "m1", embedding_model="ollama:nomic-embed-text", embedding_version="nomic-embed-text@x"
    )
    assert other == []  # a re-pulled model (new digest) never compares against these vectors
    client = memory.embedder._client  # type: ignore[attr-defined]
    await memory.close()
    assert client.is_closed
    await store.close()


async def test_absent_ollama_means_lexical_memory_not_a_failure(tmp_path: Path) -> None:
    fake = FakeOllama()
    fake.down = True
    store = SqliteStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        recorder=recorder,
        embedder_transport=fake.transport(),
    )
    assert memory is not None and memory.embedder is None and memory.mode.label == "lexical"
    (event,) = [e for e in recorder.events if e.kind == "memory_degraded"]
    assert "ollama unreachable" in str(event.data["reason"])
    await memory.close()
    await store.close()


async def test_ollama_failing_mid_run_degrades_to_lexical(tmp_path: Path) -> None:
    fake = FakeOllama()
    store = SqliteStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=_repo(tmp_path / "ws"),
        mission_id="m1",
        recorder=recorder,
        embedder_transport=fake.transport(),
    )
    assert memory is not None and memory.mode.dense
    fake.embed_status = 503  # Ollama stops serving after the run started
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c1",
        item=ChecklistItem(id="01", description="the sql database"),
        snapshot=SituationSnapshot(head_sha=""),
    )
    assert "db.py" in block  # lexical + git grep still answer
    assert memory.embedder is None and memory.mode.label == "lexical"
    assert any(e.kind == "memory_degraded" for e in recorder.events)
    await memory.close()
    await store.close()


class _FakePgStore(SqliteStore):
    backend = "postgres"


async def test_ollama_vectors_are_padded_for_pgvector(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "pgvector", types.ModuleType("pgvector"))
    fake = FakeOllama()
    store = _FakePgStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(),
        store=store,
        workdir=tmp_path,
        mission_id="m",
        embedder_transport=fake.transport(),
    )
    assert memory is not None and memory.mode.label == "hybrid"
    assert isinstance(memory.embedder, PaddedEmbedder) and memory.embedder.dim == 1024
    assert memory.embedder.version == f"nomic-embed-text@{DIGEST[:12]}"
    await memory.close()
    await store.close()
