"""Regression tests for the state / coordination review fixes.

1. ``init_repo`` inits a NESTED repo instead of hijacking an enclosing one; ``HEAD:`` lookups are
   workdir-relative.
2. The mission anchor commits its files even when ``.lha/`` is gitignored, and never duplicates
   pending events.
3. Decision-log verification / anchor decision reads split only on ``\\n`` (U+2028 etc. are data)
   and ``verify`` reports bad lines instead of raising.
4. ``FileOwnershipMap`` keys are case-folded (one owner per physical file on case-insensitive
   filesystems) and ``assign`` never silently steals a file.
5. The orchestrator's loop detector counts CONSECUTIVE failures (a verified cycle resets it).
6. ``commit_all`` detects a clean tree by exit code, independent of the git locale.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from lha.agents.orchestrator import Orchestrator
from lha.config import Settings
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, DecisionRecord, EventRecord
from lha.contracts.verify import Check
from lha.coordination.decision_log import DecisionLog
from lha.coordination.ownership import FileOwnershipMap, OwnershipConflictError
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor


def _git(cwd: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-c", "user.name=u", "-c", "user.email=e@x", *args],
        cwd=cwd,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()


def _parent_repo(root: Path, *, ignore_lha: bool = False) -> Path:
    root.mkdir(parents=True, exist_ok=True)
    _git(root, "init", "-q", "-b", "main")
    if ignore_lha:
        (root / ".gitignore").write_text(".lha/\n", encoding="utf-8")
        _git(root, "add", ".gitignore")
    _git(root, "commit", "-q", "--allow-empty", "-m", "base")
    return root


def _checklist() -> Checklist:
    return Checklist(items=[ChecklistItem(id="01", description="x")])


# --- 1. nested repo ----------------------------------------------------------------------
def test_is_repo_requires_the_toplevel(tmp_path: Path) -> None:
    parent = _parent_repo(tmp_path / "parent")
    nested = parent / "sub" / "work"
    nested.mkdir(parents=True)
    assert git_ops.is_repo(parent)
    assert not git_ops.is_repo(nested)  # inside a work tree, but not its top level


@pytest.mark.asyncio
async def test_anchor_in_nested_dir_gets_its_own_repo(tmp_path: Path) -> None:
    parent = _parent_repo(tmp_path / "parent")
    (parent / "unrelated.txt").write_text("user work\n", encoding="utf-8")
    parent_head = _git(parent, "rev-parse", "HEAD")
    workdir = parent / ".lha" / "workspaces" / "durable"

    anchor = GitMissionAnchor(workdir)
    await anchor.initialize(title="t", description="d", items=_checklist())

    # The enclosing repo is untouched: no commit, no identity change, user file still untracked.
    assert _git(parent, "rev-parse", "HEAD") == parent_head
    assert "?? unrelated.txt" in _git(parent, "status", "--porcelain")
    local_cfg = subprocess.run(
        ["git", "config", "--local", "user.name"], cwd=parent, capture_output=True, text=True
    )
    assert local_cfg.stdout.strip() != "LHA Agent"
    # The workdir is its own repo and its anchor is committed + read from HEAD.
    assert (workdir / ".git").exists()
    assert git_ops.exists_at_head(workdir, ".lha/checklist.json")
    assert len(git_ops.log_oneline(workdir, 10)) == 1


def test_head_lookups_are_relative_to_cwd(tmp_path: Path) -> None:
    repo = _parent_repo(tmp_path / "repo")
    sub = repo / "sub"
    sub.mkdir()
    (sub / "f.txt").write_text("hello\n", encoding="utf-8")
    _git(repo, "add", "-A")
    _git(repo, "commit", "-q", "-m", "f")
    assert git_ops.exists_at_head(sub, "f.txt")
    assert git_ops.show_at_head(sub, "f.txt") == "hello"
    assert not git_ops.exists_at_head(sub, "missing.txt")


# --- 2. gitignored anchor ---------------------------------------------------------------
@pytest.mark.asyncio
async def test_anchor_is_committed_even_when_gitignored(tmp_path: Path) -> None:
    repo = _parent_repo(tmp_path / "repo", ignore_lha=True)
    anchor = GitMissionAnchor(repo)
    await anchor.initialize(title="t", description="d", items=_checklist())
    assert git_ops.exists_at_head(repo, ".lha/checklist.json")

    # Agent tampering with the working tree is ignored by reads and discarded on commit.
    forged = _checklist()
    forged.items[0].status = "done"
    (repo / ".lha" / "checklist.json").write_text(forged.model_dump_json(), encoding="utf-8")
    assert not (await anchor.read_situational_awareness()).is_complete
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
    )
    assert not (await GitMissionAnchor(repo).read_checklist()).is_complete


@pytest.mark.asyncio
async def test_pending_events_are_not_duplicated_when_anchor_untracked(tmp_path: Path) -> None:
    repo = _parent_repo(tmp_path / "repo", ignore_lha=True)
    anchor = GitMissionAnchor(repo)
    await anchor.initialize(title="t", description="d", items=_checklist())
    await anchor.append_event(EventRecord(kind="started", cycle_id="c1"))
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=_checklist(),
            events=[EventRecord(kind="cycle", cycle_id="c1")],
        )
    )
    committed = git_ops.show_at_head(repo, ".lha/events.ndjson")
    kinds = [EventRecord.model_validate_json(ln).kind for ln in committed.split("\n") if ln]
    assert kinds == ["started", "cycle"]


@pytest.mark.asyncio
async def test_rebuilt_logs_do_not_duplicate_even_without_restore(tmp_path: Path) -> None:
    """Even if the restore step is skipped, the log write is idempotent (rebuilt from HEAD)."""
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="t", description="d", items=_checklist())
    anchor._restore_anchor_dir = lambda: None  # type: ignore[method-assign]
    await anchor.append_event(EventRecord(kind="started", cycle_id="c1"))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
    )
    lines = (tmp_path / ".lha" / "events.ndjson").read_text(encoding="utf-8").split("\n")
    assert [EventRecord.model_validate_json(ln).kind for ln in lines if ln] == ["started"]


# --- 3. line splitting --------------------------------------------------------------------
_SEPARATORS = "a\u2028b\u2029c\x85d"


def test_decision_log_verify_tolerates_unicode_line_separators(tmp_path: Path) -> None:
    log = DecisionLog(str(tmp_path / "d.jsonl"))
    log.append(DecisionRecord(decision="first", rationale="r"))
    log.append(DecisionRecord(decision=_SEPARATORS, rationale="r"))
    assert [r.decision for r in log.read()] == ["first", _SEPARATORS]
    result = log.verify()
    assert result.ok and result.checked == 2


def test_decision_log_verify_reports_bad_lines_instead_of_raising(tmp_path: Path) -> None:
    path = tmp_path / "d.jsonl"
    log = DecisionLog(str(path))
    log.append(DecisionRecord(decision="first", rationale="r"))
    good = path.read_text(encoding="utf-8")
    path.write_text("{not json\n" + good, encoding="utf-8")
    result = log.verify()
    assert not result.ok
    assert "unreadable" in result.problem

    # A structurally-bad (non-final) envelope is a failure result too, never an exception.
    bad = json.dumps({"prev": "0" * 64, "hash": "x", "record": [1]})
    path.write_text(bad + "\n" + good, encoding="utf-8")
    result = log.verify()
    assert not result.ok and "unreadable" in result.problem


@pytest.mark.asyncio
async def test_anchor_reads_decisions_with_unicode_line_separators(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="t", description="d", items=_checklist())
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=_checklist(),
            decisions=[DecisionRecord(decision=_SEPARATORS, rationale="r")],
        )
    )
    snap = await GitMissionAnchor(tmp_path).read_situational_awareness()
    assert [d.decision for d in snap.last_decisions] == [_SEPARATORS]


# --- 4. ownership -------------------------------------------------------------------------
def test_ownership_keys_are_case_insensitive() -> None:
    owners = FileOwnershipMap()
    owners.assign("src/app/Models.py", "impl-a")
    with pytest.raises(OwnershipConflictError):
        owners.assign("src/app/models.py", "impl-b")
    assert owners.owner_of("SRC/app/models.py") == "impl-a"
    assert owners.permits(writer="impl-a", path="src/app/models.py")
    assert not owners.permits(writer="impl-b", path="src/app/Models.py")


def test_assign_never_silently_steals_ownership() -> None:
    owners = FileOwnershipMap()
    owners.assign("src/x.py", "impl-a")
    owners.assign("./src/x.py", "impl-a")  # same owner: idempotent
    with pytest.raises(OwnershipConflictError):
        owners.assign("src/x.py", "impl-b")
    assert owners.owner_of("src/x.py") == "impl-a"
    assert owners.reassign("src/x.py", "impl-b") == "impl-a"  # explicit transfer
    assert owners.owner_of("src/x.py") == "impl-b"
    with pytest.raises(ValueError, match="lead"):
        owners.reassign("pyproject.toml", "impl-b")


# --- 5. consecutive-failure loop detection -----------------------------------------------
_DONE = TurnResult(text='{"done": true, "summary": "did it"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
_BLOCK = TurnResult(text='{"done": true, "verdict": "block", "blocking_issues": ["redo"]}')


@pytest.mark.asyncio
async def test_loop_detector_counts_consecutive_failures_only(tmp_path: Path) -> None:
    """fail, pass (review blocks), fail, pass: 2 failures but never 2 IN A ROW."""
    counter = tmp_path / "counter"
    script = (
        "import pathlib,sys;p=pathlib.Path(sys.argv[1]);"
        "n=int(p.read_text() if p.exists() else 0)+1;p.write_text(str(n));"
        "sys.exit(1 if n in (1,3) else 0)"
    )
    check = Check(name="flaky", command=[sys.executable, "-c", script, str(counter)])
    settings = Settings(
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=100.0,
        max_cycles=20,
        stall_limit=2,
        flaky_retries=0,  # the check flips on purpose; quarantine would change the scenario
    )
    orchestrator = Orchestrator(
        settings,
        research_per_item=0,
        do_review=True,
        models={
            "lead": StubModel(script=[_DONE]),
            "researcher": StubModel(script=[_DONE]),
            "reviewer": StubModel(script=[_BLOCK, _APPROVE]),
        },
    )
    workdir = tmp_path / "work"
    summary = await orchestrator.run_mission(
        workdir=str(workdir),
        title="t",
        description="d",
        checklist=_checklist(),
        checks=[check],
    )
    assert int(counter.read_text()) >= 4  # the scenario really had two separated failures
    assert not summary.stopped_reason.startswith("loop"), summary.stopped_reason
    assert summary.completed


# --- 6. locale-independent clean-tree detection ------------------------------------------
def test_commit_all_clean_tree_is_noop_under_any_locale(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("LC_ALL", "de_DE.UTF-8")
    monkeypatch.setenv("LANG", "de_DE.UTF-8")
    monkeypatch.setenv("LANGUAGE", "de")
    git_ops.init_repo(tmp_path)
    (tmp_path / "a.txt").write_text("a", encoding="utf-8")
    first = git_ops.commit_all(tmp_path, "one")
    assert first
    assert git_ops.commit_all(tmp_path, "nothing changed") == first
    assert len(git_ops.log_oneline(tmp_path, 10)) == 1
    assert git_ops._git_env()["LC_ALL"] == "C"
