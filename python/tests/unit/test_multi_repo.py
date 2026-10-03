"""Multi-repo workspaces: a mission over several repositories held as git submodules ("members").

The anchor and every harness commit live in the workspace; a member's changes are committed inside
the member first; resets reach into members; the review diff expands each member's changes; a
member's ``.git`` pointer must link back to it; ``lha workspace init`` builds one.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agent.prompt import build_messages
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.state import git_ops
from lha.state.git_ops import GitError
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.harness_integrity import snapshot_harness
from tests.durability._support import SETTINGS

runner = CliRunner()


def _upstream(path: Path, name: str) -> Path:
    git_ops.init_repo(path)
    (path / "README.md").write_text(f"# {name}\n", encoding="utf-8")
    (path / "tests").mkdir()
    (path / "tests" / "test_a.py").write_text("def test_a():\n    assert True\n", encoding="utf-8")
    git_ops.commit_all(path, "init")
    return path


def _workspace(tmp_path: Path) -> Path:
    svc = _upstream(tmp_path / "upstream" / "svc.git", "svc")
    lib = _upstream(tmp_path / "upstream" / "lib", "lib")
    ws = tmp_path / "ws"
    result = runner.invoke(
        cli.app, ["workspace", "init", str(ws), "--repo", str(svc), "--repo", f"lib={lib}@main"]
    )
    assert result.exit_code == 0, result.output
    assert result.output.splitlines()[0].startswith(f"svc <- {svc} (")
    assert result.output.splitlines()[1].startswith(f"lib <- {lib}@main (")
    assert result.output.splitlines()[-1] == f"workspace {ws}: 2 members"
    return ws


def test_workspace_init_builds_a_repo_of_members(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    assert git_ops.member_paths(ws) == ["lib", "svc"]
    assert git_ops.is_workspace(ws) and not git_ops.is_workspace(tmp_path / "upstream" / "lib")
    assert git_ops.log_oneline(ws, 1)[0].endswith("lha: workspace members svc, lib")
    assert (ws / "svc" / ".git").is_file() and (ws / "svc" / "tests" / "test_a.py").is_file()
    # A second run keeps the members it has and adds the new one.
    extra = _upstream(tmp_path / "upstream" / "docs", "docs")
    again = runner.invoke(
        cli.app, ["workspace", "init", str(ws), "--repo", str(extra), "--repo", str(ws / "svc")]
    )
    assert again.exit_code == 0, again.output
    assert "svc: already a member, kept" in again.output
    assert git_ops.member_paths(ws) == ["docs", "lib", "svc"]
    # Bad specs are refused before anything is created.
    bad = runner.invoke(
        cli.app, ["workspace", "init", str(tmp_path / "nope"), "--repo", "a=", "--repo", "x"]
    )
    assert bad.exit_code == 2 and "missing a URL" in bad.output
    dup = runner.invoke(
        cli.app, ["workspace", "init", str(tmp_path / "nope"), "--repo", "a=x", "--repo", "a=y"]
    )
    assert dup.exit_code == 2 and "--repo names repeat: a" in dup.output
    assert not (tmp_path / "nope").exists()
    missing = runner.invoke(
        cli.app, ["workspace", "init", str(tmp_path / "ws2"), "--repo", str(tmp_path / "absent")]
    )
    assert missing.exit_code == 1 and "cannot add member 'absent'" in missing.output


def test_parse_member_spec() -> None:
    assert cli.parse_member_spec("git@host:org/svc.git") == ("svc", "git@host:org/svc.git", "")
    assert cli.parse_member_spec("https://h/x/lib.git@v2") == ("lib", "https://h/x/lib.git", "v2")
    assert cli.parse_member_spec("api=https://h/x/y@main") == ("api", "https://h/x/y", "main")
    assert cli.parse_member_spec("/tmp/repos/thing/") == ("thing", "/tmp/repos/thing/", "")
    with pytest.raises(ValueError):
        cli.parse_member_spec("..=/x")


def test_commit_all_commits_inside_members_first(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    before = git_ops.head_sha(ws)
    svc_before = git_ops.head_sha(ws / "svc")
    (ws / "svc" / "app.py").write_text("x = 1\n", encoding="utf-8")
    (ws / "lib" / "README.md").write_text("# lib\nmore\n", encoding="utf-8")
    (ws / "NOTES.md").write_text("workspace note\n", encoding="utf-8")
    sha = git_ops.commit_all(ws, "lha: checkpoint c1")
    assert sha != before and git_ops.head_sha(ws) == sha
    assert git_ops.log_oneline(ws / "svc", 1)[0].endswith("lha: checkpoint c1")
    assert git_ops.log_oneline(ws / "lib", 1)[0].endswith("lha: checkpoint c1")
    assert git_ops.head_sha(ws / "svc") != svc_before
    assert git_ops.run_git(ws, "rev-parse", "HEAD:svc") == git_ops.head_sha(ws / "svc")
    assert git_ops.run_git(ws, "status", "--porcelain") == ""
    # Nothing to commit anywhere: a no-op that returns HEAD.
    assert git_ops.commit_all(ws, "lha: checkpoint c2") == sha
    # The review diff shows the members' code with their paths prefixed, not two gitlink ids.
    diff = git_ops.diff_range(ws, before, sha, exclude=(".lha",))
    assert "+workspace note" in diff
    assert "--- a/svc/app.py" not in diff and "+++ b/svc/app.py" in diff and "+x = 1" in diff
    assert "+++ b/lib/README.md" in diff and "+more" in diff
    assert "Subproject commit" in diff  # the gitlink lines stay as well


def test_reset_and_discard_reach_into_members(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    (ws / "svc" / "README.md").write_text("edited\n", encoding="utf-8")
    (ws / "svc" / "untracked.txt").write_text("u\n", encoding="utf-8")
    (ws / "lib" / "build.out").write_text("ignored?\n", encoding="utf-8")
    (ws / "lib" / ".gitignore").write_text("build.out\n", encoding="utf-8")
    git_ops.discard_changes(ws)
    assert (ws / "svc" / "README.md").read_text(encoding="utf-8") == "# svc\n"
    assert not (ws / "svc" / "untracked.txt").exists()
    assert not (ws / "lib" / ".gitignore").exists()  # untracked, discarded
    assert (
        ws / "lib" / "build.out"
    ).exists()  # was ignored while .gitignore existed? no: tracked state
    (ws / "svc" / "README.md").write_text("edited again\n", encoding="utf-8")
    (ws / "lib" / ".venv").mkdir()
    (ws / "lib" / ".venv" / "bin").write_text("keep\n", encoding="utf-8")
    git_ops.reset_to_head(ws)
    assert (ws / "svc" / "README.md").read_text(encoding="utf-8") == "# svc\n"
    assert not (ws / "lib" / "build.out").exists()  # a full reset cleans ignored files too
    assert (ws / "lib" / ".venv" / "bin").exists()  # but keeps the dependency environments
    # A member moved past the recorded commit is brought back to it.
    (ws / "svc" / "extra.txt").write_text("e\n", encoding="utf-8")
    git_ops.run_git(ws / "svc", "add", "-A")
    git_ops.run_git(
        ws / "svc", "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "stray"
    )
    recorded = git_ops.run_git(ws, "rev-parse", "HEAD:svc")
    git_ops.reset_to_head(ws)
    assert git_ops.head_sha(ws / "svc") == recorded and not (ws / "svc" / "extra.txt").exists()


def test_a_member_pointer_that_does_not_link_back_is_refused(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    git_ops.run_git(ws / "svc", "status")  # the genuine pointer passes
    pointer = ws / "svc" / ".git"
    original = pointer.read_text(encoding="utf-8")
    pointer.write_text("gitdir: ../.git\n", encoding="utf-8")  # the workspace's own repository
    with pytest.raises(GitError, match="is the enclosing repository"):
        git_ops.run_git(ws / "svc", "status")
    pointer.write_text("gitdir: ../.git/modules/lib\n", encoding="utf-8")  # another member's
    with pytest.raises(GitError, match="does not link back"):
        git_ops.run_git(ws / "svc", "status")
    pointer.write_text(original, encoding="utf-8")
    git_ops.run_git(ws / "svc", "status")


def test_harness_snapshot_and_prompt_see_members(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    assert "svc/tests/test_a.py" in snapshot_harness(
        ws
    ) and "lib/tests/test_a.py" in snapshot_harness(ws)
    anchor = GitMissionAnchor(ws)
    items = Checklist(items=[ChecklistItem(id="01", description="add x")])
    asyncio.run(anchor.initialize(title="t", description="d", items=items))
    snapshot = asyncio.run(anchor.read_situational_awareness())
    assert snapshot.members == ["lib", "svc"]
    messages = build_messages(anchor_text="", snapshot=snapshot, item=items.items[0], specs=[])
    assert "member repositories (git submodules)" in messages[1].content
    assert "root: lib, svc" in messages[1].content


def test_a_local_mission_commits_into_the_member(tmp_path: Path) -> None:
    from lha.agent.runner import run_mission_local
    from lha.model.stub import StubModel
    from tests.durability._support import _done, write_turn

    ws = _workspace(tmp_path)
    items = Checklist(items=[ChecklistItem(id="01", description="write work/01.txt in svc")])
    summary = asyncio.run(
        run_mission_local(
            workdir=str(ws),
            title="t",
            description="d",
            checklist=items,
            checks=[Check(name="svc-work", command=["sh", "-c", "test -f svc/work/01.txt"])],
            settings=SETTINGS,
            model=StubModel(script=[write_turn("svc/work/01.txt"), _done()]),
        )
    )
    assert summary.completed, summary
    assert (ws / "svc" / "work" / "01.txt").is_file()
    assert git_ops.log_oneline(ws / "svc", 1)[0].endswith(
        "lha: complete 01 (write work/01.txt in svc)"
    )
    assert git_ops.run_git(ws, "status", "--porcelain") == ""


def test_parallel_waves_are_refused_on_a_workspace(tmp_path: Path) -> None:
    ws = _workspace(tmp_path)
    result = runner.invoke(
        cli.app, ["mission-start", "--task", "x", "--max-parallel", "2", "--workdir", str(ws)]
    )
    assert result.exit_code == 2, result.output
    assert "is a multi-repo workspace (members: lib, svc)" in result.output
