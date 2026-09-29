"""Tiered memory wired into the agent: prompt budget, episodic recall, skills, consolidation,
degradation — at the service level and through the real run paths."""

from __future__ import annotations

import subprocess
import sys
import types
from pathlib import Path

import pytest

from lha.agent.prompt import MEMORY_HEADER, build_messages, render_memory_block
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.memory import MemoryRecord
from lha.contracts.model import ModelMessage, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.contracts.verify import Check
from lha.memory import service as memsvc
from lha.memory.embeddings import HashEmbedder, PaddedEmbedder
from lha.memory.service import (
    CONSOLIDATION_KIND,
    EPISODE_KIND,
    CycleObservation,
    MemoryConfig,
    MissionMemory,
    open_mission_memory,
)
from lha.model.stub import StubModel
from lha.obs.events import TraceRecorder
from lha.ops.degradation import (
    DependencyStatus,
    Health,
    decide_memory_mode,
    decide_safe_park,
)
from lha.persistence.sqlite import SqliteStore
from lha.state import git_ops

_PASS = Check(name="green", command=[sys.executable, "-c", "pass"])
_FAIL = Check(name="red", command=[sys.executable, "-c", "raise SystemExit(1)"])


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 20,
        "max_turns_per_cycle": 1,
        "stall_limit": 50,
    }
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


class _Recording(StubModel):
    """A scripted stub that remembers every prompt it was sent."""

    def __init__(self, *script: TurnResult) -> None:
        super().__init__(script=list(script))
        self.prompts: list[list[ModelMessage]] = []

    async def complete(self, messages: list[ModelMessage], **kwargs: object) -> TurnResult:
        # Only the lead's cycle prompts (not e.g. the replanner's calls on a blocked item).
        if len(messages) > 1 and messages[1].content.startswith("Active checklist item"):
            self.prompts.append(list(messages))
        return await super().complete(messages)


def _memory_block(prompt: list[ModelMessage]) -> str:
    """The memory block of a cycle's task message ('' when there is none)."""
    task = prompt[1].content
    if MEMORY_HEADER not in task:
        return ""
    return task[task.index(MEMORY_HEADER) : task.index("Recent commits:")].strip()


def _repo(path: Path, files: dict[str, str]) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    git_ops.init_repo(path)
    for name, text in files.items():
        (path / name).write_text(text, encoding="utf-8")
    git_ops.commit_all(path, "init")
    return path


def _item(item_id: str = "01", description: str = "fix the config parser") -> ChecklistItem:
    return ChecklistItem(id=item_id, description=description)


def _snapshot() -> SituationSnapshot:
    return SituationSnapshot(head_sha="")


async def _memory(
    tmp_path: Path, workdir: Path, *, recorder: TraceRecorder | None = None, **cfg: object
) -> MissionMemory:
    store = SqliteStore(tmp_path / "mem.sqlite3")
    await store.open()
    return MissionMemory(
        store=store,
        workdir=workdir,
        config=MemoryConfig(**cfg),  # type: ignore[arg-type]
        mode=decide_memory_mode([]),
        embedder=HashEmbedder(),
        recorder=recorder,
    )


def _obs(cycle: str, *, verified: bool, item_id: str = "01", **kw: object) -> CycleObservation:
    base: dict[str, object] = {
        "mission_id": "m1",
        "cycle_id": cycle,
        "item_id": item_id,
        "item_description": "fix the config parser",
        "verdict": "passed" if verified else "failed",
        "verified": verified,
        "status": "done" if verified else "in_progress",
        "attempts": 1,
        "head_sha": "",
        "failure": "" if verified else "- pytest FAILED (exit 1): KeyError 'port'",
        "done_summary": "handled missing port key",
        "tools": ["read_file", "write_file"],
    }
    base.update(kw)
    return CycleObservation(**base)  # type: ignore[arg-type]


# --- prompt budget ----------------------------------------------------------------------------
def test_render_memory_block_respects_the_budget_and_is_deterministic() -> None:
    sections = [
        ("Earlier attempts", [f"- attempt {i} " + "x" * 300 for i in range(10)]),
        ("Skills", []),
        ("Related", [f"- fact {i} " + "y" * 300 for i in range(10)]),
    ]
    for budget in (200, 800, 2000, 4000):
        block = render_memory_block(sections, budget_chars=budget, weights=(0.4, 0.2, 0.4))
        assert len(block) <= budget
        assert block == render_memory_block(sections, budget_chars=budget, weights=(0.4, 0.2, 0.4))
        assert block.startswith(MEMORY_HEADER)
        assert "Skills:" not in block  # empty sections are omitted
    # The empty skills share rolls over: both non-empty sections get room.
    block = render_memory_block(sections, budget_chars=2000, weights=(0.4, 0.2, 0.4))
    assert "Earlier attempts:" in block and "Related:" in block
    assert render_memory_block([("A", [])], budget_chars=4000) == ""
    assert render_memory_block([("A", ["- a"])], budget_chars=10) == ""
    with pytest.raises(ValueError):
        render_memory_block([("A", ["- a"])], budget_chars=500, weights=(1.0, 2.0))


def test_build_messages_places_memory_in_the_task_and_is_unchanged_without_it() -> None:
    kwargs = {
        "anchor_text": "",
        "mission_text": "Mission: m",
        "snapshot": SituationSnapshot(head_sha="h", recent_commits=["abc init"]),
        "item": _item(),
        "specs": [],
    }
    plain = build_messages(**kwargs)  # type: ignore[arg-type]
    assert build_messages(**kwargs, memory_text="") == plain  # type: ignore[arg-type]
    with_memory = build_messages(**kwargs, memory_text=f"{MEMORY_HEADER}\nA:\n- a")  # type: ignore[arg-type]
    assert with_memory[0] == plain[0]  # system prompt untouched
    task = with_memory[1].content
    assert task.index("Active checklist item") < task.index(MEMORY_HEADER)
    assert task.index(MEMORY_HEADER) < task.index("Recent commits:")


# --- degradation rules ------------------------------------------------------------------------
def test_decide_memory_mode_drops_dense_when_a_memory_dep_is_down() -> None:
    assert decide_memory_mode([]).dense and not decide_memory_mode([]).git_grep
    ok = decide_memory_mode([DependencyStatus("embeddings", Health.OK)])
    assert ok.label == "hybrid"
    for dep in ("postgres", "pgvector", "embeddings"):
        mode = decide_memory_mode([DependencyStatus(dep, Health.DOWN, "gone")])
        assert (mode.dense, mode.git_grep, mode.label) == (False, True, "lexical")
        assert mode.degraded == [dep] and "gone" in mode.reason and "git grep" in mode.reason
    # Non-memory deps never affect memory; memory deps never park the mission.
    assert decide_memory_mode([DependencyStatus("langfuse", Health.DOWN)]).dense
    park = decide_safe_park([DependencyStatus("pgvector", Health.DOWN)])
    assert not park.park and park.degraded == ["pgvector"]


# --- episodic recall through the real loop ----------------------------------------------------
async def test_past_failures_are_recalled_into_the_next_cycles_prompt(tmp_path: Path) -> None:
    _repo(tmp_path / "ws", {"config_parser.py": "def parse_config(text):\n    return {}\n"})
    model = _Recording(TurnResult(text='{"done": true, "summary": "tried a regex"}'))
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[_item()]),
        checks=[_FAIL],
        settings=_settings(),
        model=model,
    )
    assert summary.stopped_reason.startswith("deadlocked")
    first, second, third = (_memory_block(p) for p in model.prompts[:3])
    # Cycle 1: no history yet, but the repo is indexed (semantic tier).
    assert "Earlier attempts" not in first
    assert "config_parser.py" in first
    # Cycle 2 sees cycle 1's attempt: what was tried and how verification failed.
    assert "c1 [01] failed" in second
    assert "claimed: tried a regex" in second and "red FAILED" in second
    assert "c1 [01] failed" in third and "c2 [01] failed" in third
    for block in (first, second, third):
        assert len(block) <= _settings().memory_prompt_budget_chars


async def test_memory_prompt_is_deterministic_across_identical_runs(tmp_path: Path) -> None:
    blocks: list[list[str]] = []
    for run in ("a", "b"):
        _repo(tmp_path / run, {"app.py": "def main():\n    return 1\n"})
        model = _Recording(TurnResult(text='{"done": true, "summary": "s"}'))
        await run_mission_local(
            workdir=str(tmp_path / run),
            title="t",
            description="d",
            checklist=Checklist(items=[_item()]),
            checks=[_FAIL],
            settings=_settings(sqlite_path=str(tmp_path / f"{run}.sqlite3")),
            model=model,
        )
        blocks.append([_memory_block(p) for p in model.prompts])
    assert blocks[0] == blocks[1]
    assert len(blocks[0]) >= 2 and blocks[0][1]  # later cycles do carry memory


async def test_memory_disabled_leaves_the_prompt_untouched(tmp_path: Path) -> None:
    _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    model = _Recording(TurnResult(text='{"done": true}'))
    await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[_item()]),
        checks=[_FAIL],
        settings=_settings(memory_enabled=False),
        model=model,
    )
    assert model.prompts and all(_memory_block(p) == "" for p in model.prompts)


async def test_memory_budget_setting_bounds_the_block(tmp_path: Path) -> None:
    big = "\n".join(f"def parse_config_{i}(text):\n    return {i}" for i in range(200))
    _repo(tmp_path / "ws", {"config_parser.py": big})
    model = _Recording(TurnResult(text='{"done": true, "summary": "' + "z" * 900 + '"}'))
    await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[_item()]),
        checks=[_FAIL],
        settings=_settings(memory_prompt_budget_chars=600),
        model=model,
    )
    sizes = [len(_memory_block(p)) for p in model.prompts]
    assert sizes and max(sizes) <= 600 and all(sizes)


# --- skills -----------------------------------------------------------------------------------
async def test_verified_success_stores_a_skill_recalled_for_a_related_item(
    tmp_path: Path,
) -> None:
    ws = _repo(tmp_path / "ws", {"greet.py": "def greet(name):\n    return name\n"})
    recorder = TraceRecorder()
    memory = await _memory(tmp_path, ws, recorder=recorder)
    await memory.observe_cycle(
        _obs(
            "c1",
            verified=True,
            item_description="add a greeting helper",
            done_summary="wrote greet() in greet.py returning 'Hello, <name>'",
        )
    )
    assert [e.kind for e in recorder.events] == ["skill_stored"]
    skills = await memory.store.list_skills(memory.namespace)
    assert len(skills) == 1 and skills[0].verified
    assert "wrote greet()" in skills[0].code and "write_file" in skills[0].code

    # A different mission in the same repo namespace: the skill is recalled for a related item.
    block = await memory.recall(
        mission_id="m2",
        cycle_id="c1",
        item=_item("07", "add a greeting helper for admins"),
        snapshot=_snapshot(),
    )
    assert "Skills that worked before:" in block and "wrote greet()" in block

    # Failures never become skills; an unrelated repo namespace never sees this one.
    await memory.observe_cycle(_obs("c2", verified=False, item_description="break things"))
    assert len(await memory.store.list_skills(memory.namespace)) == 1
    assert await memory.store.list_skills("/some/other/repo") == []
    await memory.store.close()


async def test_expired_skills_are_not_recalled(tmp_path: Path) -> None:
    ws = _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    memory = await _memory(tmp_path, ws)
    skill = await memory.store_skill(
        _obs("c1", verified=True, item_description="add a greeting helper"), []
    )
    assert [s.id for s in await memory.find_skills("greeting helper")] == [skill.id]
    await memory.store.put_skill(skill.model_copy(update={"expires_at": "2000-01-01"}))
    assert await memory.find_skills("greeting helper") == []
    await memory.store.close()


async def test_run_path_admits_a_skill_only_after_verification(tmp_path: Path) -> None:
    _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[_item()]),
        checks=[_PASS],
        settings=_settings(),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "fixed it"}')]),
    )
    assert summary.completed
    assert "skill_stored" in summary.trace_jsonl


# --- consolidation ----------------------------------------------------------------------------
async def test_extractive_consolidation_compacts_episodes_into_facts(tmp_path: Path) -> None:
    ws = _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    recorder = TraceRecorder()
    memory = await _memory(tmp_path, ws, recorder=recorder, consolidate_every=2)
    store = memory.store
    await memory.observe_cycle(_obs("c1", verified=False))
    assert await store.list_events("m1", kinds=(CONSOLIDATION_KIND,)) == []
    await memory.observe_cycle(_obs("c2", verified=True, attempts=2))

    marks = await store.list_events("m1", kinds=(CONSOLIDATION_KIND,))
    assert len(marks) == 1 and marks[0].payload["episodes"] == 2
    assert marks[0].payload["mode"] == "extractive"
    records = await store.list_memory("m1")
    facts = [r for r in records if r.kind == "fact"]
    assert len(facts) == 1
    assert "verified in c2 after 2 attempt(s)" in facts[0].text
    assert "handled missing port key" in facts[0].text and "KeyError" in facts[0].text
    # The progress notes it summarizes are soft-invalidated (not deleted, not recalled).
    assert not [r for r in records if r.kind == "progress"]
    assert "memory_consolidated" in [e.kind for e in recorder.events]

    # The next pass covers only NEW episodes and replaces the item's fact (stable id).
    await memory.observe_cycle(_obs("c3", verified=False, item_id="02"))
    await memory.observe_cycle(_obs("c4", verified=False, item_id="02"))
    assert len(await store.list_events("m1", kinds=(CONSOLIDATION_KIND,))) == 2
    fact_ids = sorted(r.id for r in await store.list_memory("m1") if r.kind == "fact")
    assert fact_ids == ["fact:m1:item:01", "fact:m1:item:02"]
    await store.close()


async def test_model_consolidation_uses_the_model_and_survives_bad_output(tmp_path: Path) -> None:
    ws = _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    memory = await _memory(tmp_path, ws, consolidate_every=1, consolidation="model")
    memory.model = StubModel(script=[TurnResult(text='["the parser needs a port default"]')])
    await memory.observe_cycle(_obs("c1", verified=False))
    facts = [r.text for r in await memory.store.list_memory("m1") if r.kind == "fact"]
    assert facts == ["the parser needs a port default"]

    memory.model = StubModel(script=[TurnResult(text="not json")])  # unusable reply
    await memory.observe_cycle(_obs("c2", verified=False))
    mark = (await memory.store.list_events("m1", kinds=(CONSOLIDATION_KIND,)))[-1]
    assert mark.payload["failed_batches"] == 1  # kept verbatim, never dropped
    assert len([r for r in await memory.store.list_memory("m1") if r.kind == "fact"]) == 2
    await memory.store.close()


async def test_consolidation_failure_is_recorded_not_raised(tmp_path: Path) -> None:
    class _Boom(StubModel):
        async def complete(self, messages: list[ModelMessage], **kwargs: object) -> TurnResult:
            raise RuntimeError("budget refused")

    ws = _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    recorder = TraceRecorder()
    memory = await _memory(
        tmp_path, ws, recorder=recorder, consolidate_every=1, consolidation="model"
    )
    memory.model = _Boom()
    await memory.observe_cycle(_obs("c1", verified=False))  # must not raise
    assert "memory_error" in [e.kind for e in recorder.events]
    assert await memory.store.list_events("m1", kinds=(CONSOLIDATION_KIND,)) == []
    assert len(await memory.store.list_events("m1", kinds=(EPISODE_KIND,))) == 1
    await memory.store.close()


# --- degradation ------------------------------------------------------------------------------
async def test_missing_embedder_extra_falls_back_to_lexical_and_git_grep(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def _no_extra(*_a: object, **_k: object) -> None:
        raise ModuleNotFoundError("No module named 'sentence_transformers'")

    monkeypatch.setattr(memsvc, "SentenceTransformerEmbedder", _no_extra)
    ws = _repo(
        tmp_path / "ws",
        {"server.py": "PORT = 8080\n\ndef listen(port=PORT):\n    return port\n"},
    )
    store = SqliteStore(tmp_path / "s.sqlite3")
    recorder = TraceRecorder()
    memory = await open_mission_memory(
        _settings(memory_embedder="sentence_transformers", memory_index_repo_files=False),
        store=store,
        workdir=ws,
        mission_id="m1",
        recorder=recorder,
    )
    assert memory is not None and memory.embedder is None
    assert memory.mode.label == "lexical" and memory.mode.git_grep
    degraded = [e for e in recorder.events if e.kind == "memory_degraded"]
    assert degraded and "sentence-transformers unavailable" in str(degraded[0].data["reason"])

    block = await memory.recall(
        mission_id="m1",
        cycle_id="c1",
        item=_item("01", "make the listen port configurable"),
        snapshot=_snapshot(),
    )
    assert "(grep) server.py" in block  # found by git grep, not by the (off) repo index
    await store.close()


async def test_postgres_fallback_store_means_lexical_memory(tmp_path: Path) -> None:
    store = SqliteStore(tmp_path / "s.sqlite3")
    store.degraded_reason = "postgres unavailable (OperationalError: refused)"
    memory = await open_mission_memory(_settings(), store=store, workdir=tmp_path, mission_id="m1")
    assert memory is not None and memory.mode.label == "lexical"
    assert memory.mode.degraded == ["postgres"] and "refused" in memory.mode.reason
    await store.close()


class _FakePgStore(SqliteStore):
    """A store that claims to be Postgres (for the open-time pgvector checks only)."""

    backend = "postgres"


async def test_postgres_memory_needs_the_pgvector_adapter(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "pgvector", None)  # adapter not installed
    store = _FakePgStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(_settings(), store=store, workdir=tmp_path, mission_id="m")
    assert memory is not None and memory.mode.label == "lexical"
    assert memory.mode.degraded == ["pgvector"]


async def test_postgres_memory_rejects_an_embedder_wider_than_the_column(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class _Wide(HashEmbedder):
        def __init__(self, *_a: object, **_k: object) -> None:
            super().__init__(dim=2048)

    monkeypatch.setattr(memsvc, "SentenceTransformerEmbedder", _Wide)
    store = _FakePgStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(memory_embedder="sentence_transformers"),
        store=store,
        workdir=tmp_path,
        mission_id="m",
    )
    assert memory is not None and memory.mode.label == "lexical"
    assert "2048" in memory.mode.reason and "vector(1024)" in memory.mode.reason


async def test_postgres_memory_pads_a_narrower_embedder_to_the_column(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class _Small(HashEmbedder):
        def __init__(self, *_a: object, **_k: object) -> None:
            super().__init__(dim=384)

    monkeypatch.setattr(memsvc, "SentenceTransformerEmbedder", _Small)
    monkeypatch.setitem(sys.modules, "pgvector", types.ModuleType("pgvector"))
    store = _FakePgStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(memory_embedder="sentence_transformers"),
        store=store,
        workdir=tmp_path,
        mission_id="m",
    )
    assert memory is not None and memory.mode.label == "hybrid"
    assert isinstance(memory.embedder, PaddedEmbedder) and memory.embedder.dim == 1024
    assert memory.embedder.name == "hash" and memory.embedder.inner.dim == 384
    (vector,) = await memory.embedder.embed(["listen port"])
    assert len(vector) == 1024 and vector[384:] == [0.0] * 640
    await memory.close()


async def test_postgres_memory_is_hybrid_with_a_1024_wide_embedder(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "pgvector", types.ModuleType("pgvector"))
    store = _FakePgStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(_settings(), store=store, workdir=tmp_path, mission_id="m")
    assert memory is not None and memory.mode.label == "hybrid"
    assert memory.embedder is not None and memory.embedder.dim == 1024


async def test_unavailable_cross_encoder_keeps_fusion_order(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def _missing(*_a: object, **_k: object) -> None:
        raise ModuleNotFoundError("No module named 'sentence_transformers'")

    monkeypatch.setattr(memsvc, "CrossEncoderReranker", _missing)
    store = SqliteStore(tmp_path / "s.sqlite3")
    memory = await open_mission_memory(
        _settings(memory_rerank="cross_encoder"), store=store, workdir=tmp_path, mission_id="m"
    )
    assert memory is not None and type(memory.reranker).__name__ == "NoopReranker"
    assert (
        await open_mission_memory(
            _settings(memory_enabled=False), store=store, workdir=tmp_path, mission_id="m"
        )
        is None
    )
    await store.close()


async def test_dense_failure_mid_run_degrades_and_the_mission_continues(tmp_path: Path) -> None:
    class _Flaky(HashEmbedder):
        broken = False

        async def embed(self, texts: list[str]) -> list[list[float]]:
            if self.broken:
                raise ConnectionError("vector backend went away")
            return await super().embed(texts)

    ws = _repo(tmp_path / "ws", {"config_parser.py": "def parse_config(text):\n    return {}\n"})
    recorder = TraceRecorder()
    memory = await _memory(tmp_path, ws, recorder=recorder)
    embedder = _Flaky()
    memory.embedder = embedder
    await memory.observe_cycle(_obs("c1", verified=False))
    embedder.broken = True
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c2",
        item=_item("02", "fix the config parser"),
        snapshot=_snapshot(),
    )
    degraded = [e for e in recorder.events if e.kind == "memory_degraded"]
    assert len(degraded) == 1 and "vector backend went away" in str(degraded[0].data["reason"])
    assert memory.mode.label == "lexical" and memory.errors == 0
    assert "c1 [01] failed" in block
    # Later cycles keep retrieving lexically (BM25 + git grep), no further errors.
    block = await memory.recall(
        mission_id="m1",
        cycle_id="c3",
        item=_item("02", "fix the config parser"),
        snapshot=_snapshot(),
    )
    assert memory.mode.label == "lexical"
    assert "config_parser.py" in block and "c1 [01] failed" in block
    await memory.store.close()


async def test_lexical_only_setting_still_runs_a_mission(tmp_path: Path) -> None:
    _repo(tmp_path / "ws", {"a.py": "x = 1\n"})
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[_item()]),
        checks=[_PASS],
        settings=_settings(memory_embedder="none"),
        model=StubModel(script=[TurnResult(text='{"done": true}')]),
    )
    assert summary.completed
    assert "memory_degraded" in summary.trace_jsonl and "LHA_MEMORY_EMBEDDER=none" in (
        summary.trace_jsonl
    )


async def test_memory_errors_never_fail_a_cycle(tmp_path: Path) -> None:
    class _BrokenStore(SqliteStore):
        async def list_events(self, *a: object, **k: object) -> list:  # type: ignore[override]
            raise OSError("disk gone")

        async def append_event(self, *a: object, **k: object) -> int:  # type: ignore[override]
            raise OSError("disk gone")

    store = _BrokenStore(tmp_path / "s.sqlite3")
    await store.open()
    memory = MissionMemory(
        store=store,
        workdir=tmp_path,
        config=MemoryConfig(),
        mode=decide_memory_mode([]),
        embedder=HashEmbedder(),
    )
    await memory.observe_cycle(_obs("c1", verified=False))
    block = await memory.recall(mission_id="m1", cycle_id="c2", item=_item(), snapshot=_snapshot())
    assert isinstance(block, str) and memory.errors >= 2
    await store.close()


def test_memory_git_is_hardened(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Memory's git is the hardened harness git: an operator GIT_DIR does not redirect it, and a
    work tree whose config the harness refuses reads as no repository evidence."""
    ws = _repo(tmp_path / "ws", {"parser.py": "def parse_config(): pass\n"})
    other = _repo(tmp_path / "other", {"decoy.py": "parse_config = 'decoy'\n"})
    monkeypatch.setenv("GIT_DIR", str(other / ".git"))
    monkeypatch.setenv("GIT_WORK_TREE", str(other))

    chunks = memsvc._repo_chunks(ws, 10)
    assert [c.metadata["path"] for c in chunks] == ["parser.py"]
    assert [h.metadata["path"] for h in memsvc._git_grep(ws, ["parse_config"])] == ["parser.py"]
    assert memsvc._git_grep(ws, ["no_such_term_anywhere"]) == []  # exit 1 is an empty result
    before = git_ops.head_sha(ws)
    (ws / "new.py").write_text("x = 1\n", encoding="utf-8")
    after = git_ops.commit_all(ws, "add new.py")
    assert memsvc._changed_files(ws, before, after) == ["new.py"]

    # A config include from the (agent-writable) work tree: every driver-capable command is
    # refused, i.e. exit 128.
    monkeypatch.delenv("GIT_DIR")
    monkeypatch.delenv("GIT_WORK_TREE")
    (ws / "agent.cfg").write_text("[user]\n\tname = agent\n", encoding="utf-8")
    subprocess.run(["git", "config", "include.path", "../agent.cfg"], cwd=ws, check=True)
    assert memsvc._changed_files(ws, before, after) == []
    with pytest.raises(RuntimeError, match="refusing to run git"):
        memsvc._git_grep(ws, ["parse_config"])


# --- re-embedding -------------------------------------------------------------------------------
class _HashV2(HashEmbedder):
    """The hash embedder under a new model version (e.g. after an ``ollama pull``)."""

    def __init__(self) -> None:
        super().__init__()
        self.version = "2"


def _facts(*texts: str) -> list[MemoryRecord]:
    return [MemoryRecord(id=t, kind="fact", text=t) for t in texts]


async def test_reembed_restores_dense_recall_after_an_embedder_change(tmp_path: Path) -> None:
    recorder = TraceRecorder()
    memory = await _memory(tmp_path, tmp_path, recorder=recorder)
    await memory._store_records("m1", _facts("config parser keys", "port defaults"))
    await memory.store.put_memory("m2", _facts("stored while degraded"))  # no vector
    index = memory._dense_index("m1")
    assert index is not None and await index.query("config parser", k=5)

    memory.embedder = _HashV2()
    memory._dense.clear()
    index = memory._dense_index("m1")
    assert index is not None and await index.query("config parser", k=5) == []

    assert await memory.reembed(None) == {"m1": 2, "m2": 1}
    assert [h.record.id for h in await index.query("config parser", k=1)] == ["config parser keys"]
    assert await memory.reembed(None) == {}
    events = [e.data for e in recorder.events if e.kind == "memory_reembedded"]
    assert events == [
        {"count": 2, "embedding_model": "hash", "embedding_version": "2"},
        {"count": 1, "embedding_model": "hash", "embedding_version": "2"},
    ]
    await memory.store.close()


async def test_reembed_honours_the_limit_and_the_mission(tmp_path: Path) -> None:
    memory = await _memory(tmp_path, tmp_path)
    await memory.store.put_memory("m1", _facts("a", "b", "c"))
    await memory.store.put_memory("m2", _facts("d"))
    assert await memory.reembed("m1", limit=2) == {"m1": 2}
    assert await memory.reembed("m1") == {"m1": 1}
    assert await memory.store.count_stale_memory(
        None, embedding_model="hash", embedding_version="1"
    ) == {"m2": 1}
    await memory.store.close()


async def test_recall_re_embeds_a_bounded_batch_each_cycle(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(memsvc, "REEMBED_PER_RECALL", 2)
    recorder = TraceRecorder()
    ws = _repo(tmp_path / "ws", {"README.md": "x\n"})
    memory = await _memory(tmp_path, ws, recorder=recorder)
    await memory.store.put_memory("m1", _facts("parser one", "parser two", "parser three"))
    for cycle in ("c1", "c2"):
        await memory.recall(mission_id="m1", cycle_id=cycle, item=_item(), snapshot=_snapshot())
    counts = [
        (e.cycle_id, e.data["count"]) for e in recorder.events if e.kind == "memory_reembedded"
    ]
    assert counts == [("c1", 2), ("c2", 1)] and memory.errors == 0
    await memory.store.close()


async def test_reembed_without_an_embedder_does_nothing(tmp_path: Path) -> None:
    memory = await _memory(tmp_path, tmp_path)
    await memory.store.put_memory("m1", _facts("a"))
    memory.embedder = None
    assert await memory.reembed(None) == {}
    await memory.store.close()


async def test_reembed_stops_when_the_store_does_not_restamp(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class _Deaf:
        async def add(self, records: list[MemoryRecord]) -> None:
            return None

    monkeypatch.setattr(memsvc, "_REEMBED_BATCH", 2)
    memory = await _memory(tmp_path, tmp_path)
    await memory.store.put_memory("m1", _facts(*"abc"))
    memory._dense["m1"] = _Deaf()  # type: ignore[assignment]
    with pytest.raises(RuntimeError, match="still stale"):
        await memory.reembed("m1")
    await memory.store.close()
