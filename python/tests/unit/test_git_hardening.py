"""Harness git cannot be steered by the work tree it runs in, nor by the operator's environment.

(a) a linked worktree's ``.git`` FILE is bound read-only into the Docker sandbox;
(b) hooks, fsmonitor, ``core.hooksPath``, attribute drivers and config included from the work
    tree never execute on a host-side commit or merge;
(c) a tampered ``gitdir:`` pointer is refused before any host git runs;
(d) the operator's ``GIT_DIR`` / ``GIT_WORK_TREE`` / ``GIT_CONFIG_*`` do not leak in.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

from lha.agents.integrator import add_worktree, commit_worktree
from lha.execution.sandbox_docker import DockerSandbox
from lha.state import git_link, git_ops
from lha.state.git_ops import GitError


def _repo(path: Path) -> Path:
    git_ops.init_repo(path)
    (path / "f.txt").write_text("base\n")
    git_ops.commit_all(path, "base")
    return path


def _hook(path: Path, marker: Path) -> None:
    path.write_text(f"#!/bin/sh\ntouch '{marker}'\n")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def _plain_git(cwd: Path, *args: str) -> None:
    """Git as the attacker would configure it (outside the harness)."""
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


# --- (a) the sandbox mounts -----------------------------------------------------------------


def test_linked_worktree_git_file_is_mounted_read_only(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    wt = add_worktree(repo, branch="lha/implementer-a/c1", base="HEAD")
    assert (wt / ".git").is_file()
    volumes = DockerSandbox(client=object()).run_kwargs(str(wt))["volumes"]
    host = wt.resolve()
    assert volumes[str(host)] == {"bind": "/workspace", "mode": "rw"}
    assert volumes[str(host / ".git")] == {"bind": "/workspace/.git", "mode": "ro"}
    # the real git dir (<repo>/.git/worktrees/<name>) is outside the mounted tree: not mounted
    link = git_link.validate(wt)
    assert not link.git_dir.is_relative_to(host)
    assert set(volumes) == {str(host), str(host / ".git")}


def test_git_dir_inside_the_mounted_tree_is_mounted_read_only(tmp_path: Path) -> None:
    wt = tmp_path / "wt"
    wt.mkdir()
    store = wt / "store"
    shutil.copytree(_repo(tmp_path / "repo") / ".git", store)
    (wt / ".git").write_text("gitdir: store\n")
    volumes = DockerSandbox(client=object()).run_kwargs(str(wt))["volumes"]
    host = wt.resolve()
    assert volumes[str(host / ".git")]["mode"] == "ro"
    assert volumes[str(host / "store")] == {"bind": "/workspace/store", "mode": "ro"}
    # ... and the host refuses to run git through such a pointer anyway
    with pytest.raises(GitError, match="inside the work tree"):
        git_ops.run_git(wt, "status")


def test_absent_protected_names_are_not_mounted(tmp_path: Path) -> None:
    volumes = DockerSandbox(client=object()).run_kwargs(str(tmp_path))["volumes"]
    assert list(volumes) == [str(tmp_path.resolve())]
    assert git_link.git_dirs_in_tree(tmp_path) == []
    (tmp_path / ".git").write_text("garbage")
    assert git_link.git_dirs_in_tree(tmp_path) == []


# --- (b) nothing planted executes -------------------------------------------------------------


def test_planted_hooks_fsmonitor_and_hookspath_do_not_run_on_commit_or_merge(
    tmp_path: Path,
) -> None:
    repo = _repo(tmp_path / "repo")
    marker = tmp_path / "PWNED"
    hooks = repo / ".git" / "hooks"
    for name in ("pre-commit", "commit-msg", "post-commit", "post-merge", "pre-merge-commit"):
        _hook(hooks / name, marker)
    evil_hooks = tmp_path / "evil-hooks"
    evil_hooks.mkdir()
    _hook(evil_hooks / "pre-commit", marker)
    _plain_git(repo, "config", "core.hooksPath", str(evil_hooks))
    _plain_git(repo, "config", "core.fsmonitor", f"touch '{marker}'; echo")
    _plain_git(repo, "config", "filter.evil.clean", f"sh -c \"touch '{marker}'; cat\"")
    _plain_git(repo, "config", "filter.evil.smudge", f"sh -c \"touch '{marker}'; cat\"")
    _plain_git(repo, "config", "merge.evil.driver", f"touch '{marker}'; false")
    _plain_git(repo, "config", "diff.evil.textconv", f"sh -c \"touch '{marker}'; cat\"")
    (repo / ".gitattributes").write_text("* filter=evil merge=evil diff=evil\n")

    (repo / "f.txt").write_text("edited\n")
    git_ops.commit_all(repo, "edit")
    git_ops.run_git(repo, "branch", "other")
    git_ops.run_git(repo, "checkout", "-q", "other")
    (repo / "g.txt").write_text("on other\n")
    git_ops.commit_all(repo, "other")
    git_ops.run_git(repo, "checkout", "-q", "main")
    git_ops.run_git(repo, "merge", "--no-ff", "--no-commit", "other")
    git_ops.commit_all(repo, "merged")
    git_ops.show_at_head(repo, "f.txt")
    git_ops.reset_to_head(repo)

    assert not marker.exists()
    assert git_ops.log_oneline(repo, 1)[0].endswith("merged")


def test_config_included_from_the_work_tree_is_refused(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    _plain_git(repo, "config", "include.path", "../agent.cfg")
    (repo / "agent.cfg").write_text("[core]\n\teditor = vi\n")
    with pytest.raises(GitError, match="inside the work tree"):
        git_ops.commit_all(repo, "x")


def test_config_included_from_outside_the_work_tree_is_allowed(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    (tmp_path / "operator.cfg").write_text("[core]\n\tabbrev = 12\n")
    _plain_git(repo, "config", "include.path", str(tmp_path / "operator.cfg"))
    (repo / "f.txt").write_text("again\n")
    assert git_ops.commit_all(repo, "fine")


def test_harness_argv_carries_the_hardening_overrides(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    _plain_git(repo, "config", "filter.Lfs.clean", "git-lfs clean -- %f")
    argv = git_ops.git_argv(repo, ["add", "-A"])
    assert argv[0] == "git" and argv[-2:] == ["add", "-A"]
    pairs = [argv[i + 1] for i, a in enumerate(argv) if a == "-c"]
    assert f"core.hooksPath={os.devnull}" in pairs
    assert "core.fsmonitor=false" in pairs
    assert "gc.autoDetach=false" in pairs  # no background gc outlives a call
    assert "filter.Lfs.clean=" in pairs
    # plumbing that never runs a driver skips the config enumeration
    assert "filter.Lfs.clean=" not in git_ops.git_argv(repo, ["rev-parse", "HEAD"])


def test_not_a_repository_yet_needs_no_overrides(tmp_path: Path) -> None:
    empty = tmp_path / "empty"
    empty.mkdir()
    assert git_ops._driver_overrides(empty) == []


# --- (c) the gitdir pointer ------------------------------------------------------------------


def test_tampered_gitdir_pointer_is_refused_before_git_runs(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    wt = add_worktree(repo, branch="lha/implementer-a/c1", base="HEAD")
    (wt / "f.txt").write_text("work\n")
    assert commit_worktree(wt, "legit", repo=repo)

    # The sandboxed agent plants a repository of its own, with a hook, and repoints .git at it.
    marker = tmp_path / "PWNED"
    planted = wt / "planted"
    shutil.copytree(repo / ".git", planted, ignore=shutil.ignore_patterns("lha-worktrees"))
    _hook(planted / "hooks" / "pre-commit", marker)
    (wt / ".git").write_text(f"gitdir: {planted}\n")
    with pytest.raises(GitError, match="refusing to run git"):
        commit_worktree(wt, "evil", repo=repo)
    with pytest.raises(GitError, match="refusing to run git"):
        git_ops.run_git(wt, "status")
    assert not marker.exists()


def test_pointer_to_another_repository_is_refused(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    other = _repo(tmp_path / "other")
    wt = add_worktree(other, branch="lha/implementer-b/c1", base="HEAD")
    # structurally a genuine worktree — but of a different repository than the mission's
    git_ops.check_git_link(wt)
    with pytest.raises(GitError, match="expected"):
        commit_worktree(wt, "x", repo=repo)
    with pytest.raises(GitError, match="expected"):
        git_ops.check_git_link(repo, expected_common_dir=other / ".git")
    git_ops.check_git_link(repo, expected_common_dir=repo / ".git")


@pytest.mark.parametrize(
    ("content", "match"),
    [
        ("not a pointer\n", "single 'gitdir"),
        ("gitdir: \n", "empty gitdir"),
        ("gitdir: a\ngitdir: b\n", "single 'gitdir"),
        ("gitdir: /nonexistent/lha/nowhere\n", "not a directory"),
        ("x" * 5000, "too large"),
    ],
)
def test_malformed_pointers_are_refused(tmp_path: Path, content: str, match: str) -> None:
    (tmp_path / ".git").write_text(content)
    with pytest.raises(GitError, match=match):
        git_ops.run_git(tmp_path, "status")


def test_structurally_wrong_worktree_dirs_are_refused(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    wt = add_worktree(repo, branch="lha/implementer-a/c1", base="HEAD")
    link = git_link.validate(wt)
    backlink = link.git_dir / "gitdir"
    good = backlink.read_text()
    backlink.write_text(str(tmp_path / "elsewhere" / ".git") + "\n")
    with pytest.raises(git_link.GitLinkError, match="link back"):
        git_link.validate(wt)
    backlink.write_text(good)
    fake_common = tmp_path / "fake"
    (fake_common / "worktrees").mkdir(parents=True)
    commondir = link.git_dir / "commondir"
    commondir.write_text(str(fake_common) + "\n")
    with pytest.raises(git_link.GitLinkError, match="not a worktree of"):
        git_link.validate(wt)
    # a git dir with no repository behind it
    bare = tmp_path / "bare"
    bare.mkdir()
    loose = tmp_path / "loose"
    loose.mkdir()
    (loose / ".git").write_text(f"gitdir: {bare}\n")
    with pytest.raises(git_link.GitLinkError, match="not a git repository"):
        git_link.validate(loose)


def test_submodule_style_separate_git_dir_is_accepted(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    sep = tmp_path / "separate"
    shutil.move(repo / ".git", sep)
    (repo / ".git").write_text(f"gitdir: {sep}\n")
    assert git_link.validate(repo).common_dir == sep.resolve()
    assert git_ops.head_sha(repo)


def test_symlinked_dot_git_is_refused(tmp_path: Path) -> None:
    repo = _repo(tmp_path / "repo")
    link = tmp_path / "link"
    link.mkdir()
    (link / ".git").symlink_to(repo / ".git")
    with pytest.raises(GitError, match="symlink"):
        git_ops.run_git(link, "status")


def test_unreadable_pointer_is_refused(tmp_path: Path) -> None:
    with pytest.raises(git_link.GitLinkError, match="unreadable"):
        git_link.read_pointer(tmp_path)


# --- (d) the operator's environment -----------------------------------------------------------


def test_operator_git_env_does_not_leak_into_harness_git(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    repo = _repo(tmp_path / "repo")
    decoy = _repo(tmp_path / "decoy")
    marker = tmp_path / "PWNED"
    evil_hooks = tmp_path / "evil-hooks"
    evil_hooks.mkdir()
    _hook(evil_hooks / "pre-commit", marker)
    global_cfg = tmp_path / "global.cfg"
    global_cfg.write_text(f"[core]\n\thooksPath = {evil_hooks}\n")
    monkeypatch.setenv("GIT_DIR", str(decoy / ".git"))
    monkeypatch.setenv("GIT_WORK_TREE", str(decoy))
    monkeypatch.setenv("GIT_INDEX_FILE", str(tmp_path / "index"))
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", str(global_cfg))
    monkeypatch.setenv("GIT_CONFIG_COUNT", "1")
    monkeypatch.setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
    monkeypatch.setenv("GIT_CONFIG_VALUE_0", str(evil_hooks))
    monkeypatch.setenv("GIT_CONFIG_PARAMETERS", f"'core.hooksPath'='{evil_hooks}'")
    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / ".gitconfig").write_text(f"[core]\n\thooksPath = {evil_hooks}\n")

    env = git_ops._git_env()
    assert not {k for k in env if k.startswith("GIT_")} - {
        "GIT_TERMINAL_PROMPT",
        "GIT_ASKPASS",
        "GIT_CONFIG_NOSYSTEM",
        "GIT_CONFIG_GLOBAL",
        "GIT_ATTR_NOSYSTEM",
    }
    assert env["GIT_CONFIG_GLOBAL"] == os.devnull
    assert env["HOME"] != str(tmp_path)

    decoy_head = git_ops.head_sha(decoy)
    (repo / "f.txt").write_text("changed\n")
    head = git_ops.commit_all(repo, "in the right repo")
    assert head and head != decoy_head
    assert git_ops.log_oneline(repo, 1)[0].endswith("in the right repo")
    monkeypatch.delenv("GIT_DIR")
    monkeypatch.delenv("GIT_WORK_TREE")
    assert git_ops.head_sha(decoy) == decoy_head
    assert not marker.exists()


def test_safe_home_is_recreated_when_removed() -> None:
    shutil.rmtree(git_ops._safe_home())
    assert Path(git_ops._safe_home()).is_dir()


def test_every_worktree_operation_waits_for_the_repositorys_worktree_lock(tmp_path: Path) -> None:
    # git does not make worktree add, remove and prune safe to run at once on one repository: a
    # prune deletes the admin directory of a worktree an add is still creating, and an add can
    # read another's half-written one ("failed to read .git/worktrees/..."); CI saw it once with
    # two parallel implementers. So each one holds the repository's lock while git runs.
    import threading

    from lha.agents.integrator import remove_worktree
    from lha.verify.trusted import _add_worktree, _remove_worktree

    repo = _repo(tmp_path / "r")
    existing = add_worktree(repo, branch="lha/implementer-01/c1", base="HEAD")
    detached = tmp_path / "trusted"
    operations = {
        "add": lambda: add_worktree(repo, branch="lha/implementer-02/c2", base="HEAD"),
        "remove": lambda: remove_worktree(repo, path=existing, branch="lha/implementer-01/c1"),
        "trusted add": lambda: _add_worktree(repo, detached, "HEAD"),
        "trusted remove": lambda: _remove_worktree(repo, detached),
    }
    for name, operation in operations.items():
        done = threading.Event()
        worker = threading.Thread(target=lambda op=operation, ev=done: (op(), ev.set()))
        with git_ops.worktree_lock(repo):
            worker.start()
            assert not done.wait(0.5), f"{name} ran while the lock was held"
        assert done.wait(30), f"{name} never finished"
        worker.join()
