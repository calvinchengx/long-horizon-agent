"""Unit tests for durable-path helpers: git_ops hardening, workdir lock, heartbeats, spend journal,
check resolution, steering context, and the cycle's exactly-once / idle behaviour."""

from __future__ import annotations

import asyncio
import subprocess
from pathlib import Path
from typing import ClassVar

import pytest
from temporalio.exceptions import ApplicationError
from temporalio.testing import ActivityEnvironment

from lha.config import Settings
from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.durable import activities as acts
from lha.durable.types import ERROR_CONFIG, CycleInput
from lha.governor.cost import CostLedger
from lha.model.stub import StubModel
from lha.ops.degradation import DependencyStatus, Health, decide_safe_park
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

SETTINGS = Settings(
    _env_file=None,  # type: ignore[call-arg]
    model_backend="stub",
    sandbox="local",
    allow_unsafe_local=True,
    max_turns_per_cycle=2,
)


def _repo(tmp_path: Path) -> Path:
    git_ops.init_repo(tmp_path)
    (tmp_path / "a.txt").write_text("a", encoding="utf-8")
    git_ops.commit_all(tmp_path, "init")
    return tmp_path


# --- git_ops ------------------------------------------------------------------------------
def test_commit_all_nothing_to_commit_uses_exit_code_not_text(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    repo = _repo(tmp_path)
    head = git_ops.head_sha(repo)
    monkeypatch.setenv("LANG", "de_DE.UTF-8")  # porcelain text would be localized
    monkeypatch.setenv("LC_ALL", "de_DE.UTF-8")
    assert git_ops.commit_all(repo, "noop") == head
    assert len(git_ops.log_oneline(repo)) == 1


def test_git_env_is_non_interactive_and_c_locale() -> None:
    env = git_ops._git_env()
    assert env["LC_ALL"] == "C"
    assert env["GIT_TERMINAL_PROMPT"] == "0"


def test_run_git_timeout_raises_git_error(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    def _hang(*args: object, **kwargs: object) -> subprocess.CompletedProcess[str]:
        raise subprocess.TimeoutExpired(cmd="git", timeout=kwargs.get("timeout", 0))  # type: ignore[arg-type]

    monkeypatch.setattr(git_ops.subprocess, "run", _hang)
    with pytest.raises(git_ops.GitError, match="timed out"):
        git_ops.run_git(tmp_path, "status")


def test_reset_to_head_discards_all_residue_but_keeps_envs(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    (repo / ".gitignore").write_text("build/\n", encoding="utf-8")
    git_ops.commit_all(repo, "ignore build")
    (repo / "a.txt").write_text("edited", encoding="utf-8")
    (repo / "new.txt").write_text("untracked", encoding="utf-8")
    (repo / "build").mkdir()
    (repo / "build" / "out.o").write_text("ignored", encoding="utf-8")
    (repo / ".venv").mkdir()
    (repo / ".venv" / "keep").write_text("env", encoding="utf-8")
    git_ops.run_git(repo, "add", "new.txt")

    git_ops.reset_to_head(repo)
    assert (repo / "a.txt").read_text(encoding="utf-8") == "a"
    assert not (repo / "new.txt").exists()
    assert not (repo / "build").exists()
    assert (repo / ".venv" / "keep").exists()
    assert git_ops.run_git(repo, "status", "--porcelain") == "?? .venv/"  # kept on purpose


def test_reset_keep_setting_keeps_operator_paths(tmp_path: Path) -> None:
    from lha.config import Settings
    from lha.durable.activities import _reset_workdir

    repo = _repo(tmp_path)
    (repo / ".gitignore").write_text("target/\n.remote.git/\nbuild/\n", encoding="utf-8")
    git_ops.commit_all(repo, "ignore outputs")
    for kept in ("target", ".remote.git", "build"):
        (repo / kept).mkdir()
        (repo / kept / "f").write_text("x", encoding="utf-8")
    settings = Settings(_env_file=None, reset_keep="target, .remote.git")  # type: ignore[call-arg]
    _reset_workdir(str(repo), settings)
    assert (repo / "target" / "f").exists() and (repo / ".remote.git" / "f").exists()
    assert not (repo / "build").exists()  # not listed: cleaned as before


@pytest.mark.parametrize(
    "entry", [".", "*", "**", "*/*", "/abs", "../up", "a/../b", ".git", ".git/hooks", ".lha", "./"]
)
def test_reset_keep_refuses_entries_that_would_keep_the_tree(entry: str) -> None:
    from lha.config import Settings

    with pytest.raises(ValueError, match="LHA_RESET_KEEP"):
        Settings(_env_file=None, reset_keep=entry).reset_keep_paths()  # type: ignore[call-arg]
    ok = Settings(_env_file=None, reset_keep="target,.cache/,build/out")  # type: ignore[call-arg]
    assert ok.reset_keep_paths() == ["target", ".cache/", "build/out"]


def test_list_branches_and_commits_ahead(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    git_ops.run_git(repo, "branch", "empty")
    assert set(git_ops.list_branches(repo)) == {"main", "empty"}
    assert git_ops.commits_ahead(repo, "main", "empty") == 0


# --- degradation --------------------------------------------------------------------------
def test_sandbox_down_parks() -> None:
    decision = decide_safe_park([DependencyStatus("sandbox", Health.DOWN, "docker down")])
    assert decision.park and "sandbox" in decision.reason


# --- activity helpers ---------------------------------------------------------------------
def test_resolve_checks_default_and_empty() -> None:
    assert [c.name for c in acts.resolve_checks(None)]  # default python gate
    names = [c.name for c in acts.resolve_checks([["uv", "run", "a"], ["uv", "run", "b"]])]
    assert len(set(names)) == 2  # unique names even though both start with "uv"
    for empty in ([], [[]]):
        with pytest.raises(ApplicationError) as info:
            acts.resolve_checks(empty)
        assert info.value.non_retryable and info.value.type == ERROR_CONFIG


@pytest.mark.asyncio
async def test_workdir_lock_is_exclusive(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    async with acts.workdir_lock(str(repo)):
        with pytest.raises(acts.WorkdirBusyError):
            async with acts.workdir_lock(str(repo), wait_s=0.2):
                pass
    async with acts.workdir_lock(str(repo), wait_s=0.2):  # released
        pass


@pytest.mark.asyncio
async def test_with_heartbeat_heartbeats_inside_activity(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(acts, "HEARTBEAT_EVERY_S", 0.01)
    beats: list[object] = []
    env = ActivityEnvironment()
    env.on_heartbeat = lambda *details: beats.append(details)

    async def body() -> str:
        return await acts._with_heartbeat(asyncio.sleep(0.1, result="ok"), "c1")

    assert await env.run(body) == "ok"
    assert len(beats) >= 3


def test_spend_journal_roundtrip_and_seeding(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    ledger = CostLedger()
    ledger.record(cycle_id="c1", usage=Usage(model="m"), usd=1.25)
    ledger.record(cycle_id="c1", usage=Usage(model="m"), usd=None)
    acts.record_spend(str(repo), key="k1", cycle_id="c1", ledger=ledger)
    acts.record_spend(str(repo), key="k1", cycle_id="c1", ledger=ledger)  # same attempt twice
    with (git_ops.git_dir(repo) / "lha" / "spend.ndjson").open("a") as fh:
        fh.write('{"key": "torn"')  # torn last line from a crash mid-append
    assert acts.read_prior_spend(str(repo)) == (1.25, 1)

    meter = acts.build_cycle_meter(
        SETTINGS, CycleInput(mission_id="m", workdir=str(repo), cycle_id="c2", budget_usd=2.0)
    )
    assert meter.ledger.total_usd == 1.25
    assert meter.ledger.unknown_cost_entries == 1
    assert meter.governor.ceiling_usd == 2.0
    assert meter.cycle_id == "c2"
    # Seeded rows are not re-journaled as this attempt's spend.
    acts.record_spend(str(repo), key="k2", cycle_id="c2", ledger=meter.ledger)
    assert acts.read_prior_spend(str(repo)) == (1.25, 1)


def test_anchor_text_carries_steering_notes() -> None:
    snap = SituationSnapshot(head_sha="x")
    inp = CycleInput(mission_id="m", workdir=".", cycle_id="c1", steer_notes=["prefer sqlite"])
    text = acts._anchor_text(snap, inp)
    assert "Operator steering" in text and "- prefer sqlite" in text


def test_anchor_text_carries_the_boards_newest_posts_and_the_items_reflection() -> None:
    from lha.agents.waves import BOARD_ENTRIES, board_event, reflection_event
    from lha.contracts.state import ChecklistItem

    item = ChecklistItem(id="02", description="d")
    snap = SituationSnapshot(head_sha="x", active_item=item)
    inp = CycleInput(mission_id="m", workdir=".", cycle_id="c3", steer_notes=["go"])
    events = [
        reflection_event("02", "\nReflection on 02: old\n"),
        *[board_event(f"researcher:0{n}", f"post {n}") for n in range(1, BOARD_ENTRIES + 3)],
        reflection_event("01", "\nReflection on 01: other item\n"),
        reflection_event("02", "\nReflection on 02: newest\n"),
    ]
    text = acts._anchor_text(snap, inp, events)
    mission, first, rest = text.split("\n\n", 2)
    assert mission == "Mission m"  # no mission spec in this snapshot
    assert first == "Reflection on 02: newest"  # the item's LATEST reflection, stripped, first
    assert "Operator steering (most recent last):\n- go" in rest
    board = rest.split("Team board (earlier rounds):\n", 1)[1]
    assert board.startswith("[researcher:03] post 3")  # only the newest entries
    assert board.count("\n---\n") == BOARD_ENTRIES - 1 and "post 2" not in board
    assert "other item" not in text
    assert acts._anchor_text(SituationSnapshot(head_sha="x"), inp, events).startswith(
        "Mission m\n\nOperator steering"
    )  # no active item: no reflection


class _Capture(StubModel):
    seen: ClassVar[list[list[ModelMessage]]] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        _Capture.seen.append(list(messages))
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


async def _init(repo: Path, items: int = 1) -> None:
    await GitMissionAnchor(repo).initialize(
        title="T",
        description="D",
        items=Checklist(
            items=[ChecklistItem(id=f"{i:02d}", description=f"t{i}") for i in range(1, items + 1)]
        ),
    )


@pytest.mark.asyncio
async def test_cycle_prompt_includes_steer_notes_and_resets_residue(tmp_path: Path) -> None:
    await _init(tmp_path)
    (tmp_path / "residue.txt").write_text("from a crashed attempt", encoding="utf-8")
    _Capture.seen = []

    def factory(_s: Settings, _snap: SituationSnapshot) -> ModelProvider:
        return _Capture(script=[TurnResult(text='{"done": true}', stop_reason="end_turn")])

    result = await acts._execute_cycle(
        CycleInput(
            mission_id="m",
            workdir=str(tmp_path),
            cycle_id="c1",
            check_commands=[["false"]],
            steer_notes=["use the stdlib only"],
        ),
        settings=SETTINGS,
        model_factory=factory,
    )
    assert not (tmp_path / "residue.txt").exists()
    assert any("use the stdlib only" in m.content for m in _Capture.seen[0])
    assert result.advanced and result.verdict == "failed"
    assert not result.is_complete and result.items_total == 1

    # Re-running the SAME cycle id is exactly-once: it reports the committed checkpoint.
    calls = len(_Capture.seen)
    again = await acts._execute_cycle(
        CycleInput(
            mission_id="m", workdir=str(tmp_path), cycle_id="c1", check_commands=[["false"]]
        ),
        settings=SETTINGS,
        model_factory=factory,
    )
    assert again.note == "already committed by a previous attempt"
    assert again.head_sha == result.head_sha
    assert len(_Capture.seen) == calls  # no model call on the replayed attempt


@pytest.mark.asyncio
async def test_config_errors_are_non_retryable(tmp_path: Path) -> None:
    await _init(tmp_path)
    unsafe = Settings(_env_file=None, model_backend="stub", sandbox="local")  # type: ignore[call-arg]
    with pytest.raises(ApplicationError) as info:
        await acts._execute_cycle(
            CycleInput(mission_id="m", workdir=str(tmp_path), cycle_id="c1"), settings=unsafe
        )
    assert info.value.non_retryable and info.value.type == ERROR_CONFIG


@pytest.mark.asyncio
async def test_health_probe(tmp_path: Path) -> None:
    await _init(tmp_path)
    from lha.durable.types import HealthInput

    ok = await acts.probe_health(HealthInput("m", str(tmp_path)), settings=SETTINGS)
    assert ok.healthy
    bad = await acts.probe_health(HealthInput("m", str(tmp_path / "nope")), settings=SETTINGS)
    assert not bad.healthy and "git" in bad.reason
