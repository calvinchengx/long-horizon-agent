"""Trusted checks outside the sandbox (``lha.verify.trusted``), against real git repos."""

from __future__ import annotations

import subprocess
import time
from pathlib import Path

import pytest

from lha.contracts.sandbox import ExecResult
from lha.contracts.verify import Check, CheckResult, VerificationResult
from lha.state.git_ops import GitError, commit_all, head_sha, init_repo, run_git
from lha.verify.trusted import (
    CommandTrustedRunner,
    TrustedAwareVerifier,
    TrustedRunner,
    candidate_commit,
)
from lha.verify.verifier import DeterministicVerifier


class FakeSession:
    """A sandbox session that answers every exec with exit 0 (and records the argv)."""

    def __init__(self, workdir: str = "/sandbox") -> None:
        self.workdir = workdir
        self.calls: list[list[str]] = []

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        self.calls.append(argv)
        return ExecResult(exit_code=0, stdout="sandbox ok")

    async def write_file(self, relpath: str, content: str) -> None:
        return None

    async def read_file(self, relpath: str) -> str:
        return ""

    async def close(self) -> None:
        return None


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    path = tmp_path / "repo"
    init_repo(path)
    (path / ".gitignore").write_text("*.log\nbuild/\n")
    (path / "tracked.txt").write_text("v1\n")
    (path / "gone.txt").write_text("delete me\n")
    commit_all(path, "base")
    return path


def _state(path: Path) -> tuple[str, str, str]:
    return (
        head_sha(path),
        run_git(path, "status", "--porcelain", "--untracked-files=all"),
        run_git(path, "diff", "--cached", "--name-only"),
    )


def _files(path: Path, commit: str) -> set[str]:
    return set(run_git(path, "ls-tree", "-r", "--name-only", commit).splitlines())


def _worktrees(path: Path) -> list[str]:
    out = run_git(path, "worktree", "list", "--porcelain")
    return [line for line in out.splitlines() if line.startswith("worktree ")]


# --- candidate_commit -----------------------------------------------------------------------
def test_candidate_commit_captures_the_working_tree_without_touching_it(repo: Path) -> None:
    (repo / "tracked.txt").write_text("v2 uncommitted\n")
    (repo / "gone.txt").unlink()
    (repo / "new.txt").write_text("untracked\n")
    (repo / "debug.log").write_text("ignored\n")
    (repo / "build").mkdir()
    (repo / "build" / "out.bin").write_text("ignored\n")
    run_git(repo, "add", "new.txt")  # a staged change must survive, too
    before = _state(repo)

    commit = candidate_commit(repo)

    assert _state(repo) == before
    assert run_git(repo, "rev-parse", f"{commit}^") == before[0]
    assert _files(repo, commit) == {".gitignore", "tracked.txt", "new.txt"}
    assert run_git(repo, "show", f"{commit}:tracked.txt") == "v2 uncommitted"
    assert (repo / "tracked.txt").read_text() == "v2 uncommitted\n"
    # No ref moved and no branch points at the candidate.
    assert commit not in run_git(repo, "for-each-ref", "--format=%(objectname)")


def test_candidate_commit_from_a_subdirectory(repo: Path) -> None:
    sub = repo / "pkg"
    sub.mkdir()
    (sub / "mod.py").write_text("x = 1\n")
    (repo / "top.txt").write_text("top\n")
    commit = candidate_commit(sub)
    assert {"pkg/mod.py", "top.txt"} <= _files(repo, commit)


def test_candidate_commit_without_head_has_no_parent(tmp_path: Path) -> None:
    init_repo(tmp_path)
    (tmp_path / "a.txt").write_text("a\n")
    commit = candidate_commit(tmp_path)
    assert run_git(tmp_path, "rev-list", "--parents", "-n", "1", commit) == commit
    assert _files(tmp_path, commit) == {"a.txt"}
    assert head_sha(tmp_path) == ""


def test_candidate_commit_outside_a_repo_raises(tmp_path: Path) -> None:
    with pytest.raises(GitError, match="not inside a git work tree"):
        candidate_commit(tmp_path)


# --- CommandTrustedRunner -------------------------------------------------------------------
def _check(script: str, **kw: object) -> Check:
    return Check.model_validate(
        {"name": "e2e", "command": ["sh", "-c", script], "where": "trusted", **kw}
    )


async def test_runner_runs_in_a_worktree_of_the_commit(repo: Path) -> None:
    (repo / "tracked.txt").write_text("candidate\n")
    commit = candidate_commit(repo)
    (repo / "tracked.txt").write_text("edited after the snapshot\n")
    script = (
        'cat tracked.txt; echo "$LHA_CHECK_NAME $LHA_CHECK_COMMIT"; pwd; echo "$LHA_CHECK_WORKTREE"'
    )
    result = await CommandTrustedRunner().run(_check(script), workdir=str(repo), commit=commit)
    assert result.passed and result.exit_code == 0 and result.gating
    lines = result.output_tail.splitlines()
    assert lines[0] == "candidate"
    assert lines[1] == f"e2e {commit}"
    assert lines[2] == lines[3] and lines[2] != str(repo)
    assert not Path(lines[3]).exists()
    assert _worktrees(repo) == [f"worktree {repo.resolve()}"]


async def test_runner_cwd_follows_a_subdirectory_workdir(repo: Path) -> None:
    sub = repo / "svc"
    sub.mkdir()
    (sub / "marker").write_text("here\n")
    commit = candidate_commit(sub)
    result = await CommandTrustedRunner().run(_check("cat marker"), workdir=str(sub), commit=commit)
    assert result.passed and result.output_tail == "here"


async def test_runner_failure_removes_the_worktree(repo: Path) -> None:
    commit = candidate_commit(repo)
    result = await CommandTrustedRunner().run(
        _check("echo broken >&2; exit 3", gating=False), workdir=str(repo), commit=commit
    )
    assert not result.passed and result.exit_code == 3 and not result.gating
    assert "broken" in result.output_tail
    assert len(_worktrees(repo)) == 1


async def test_runner_timeout_kills_the_process_group_and_removes_the_worktree(
    repo: Path, tmp_path: Path
) -> None:
    commit = candidate_commit(repo)
    witness = tmp_path / "survivor"
    # The grandchild would write the file after 3s if the process group survived the kill.
    script = f"echo started; (sleep 3; touch {witness}) & sleep 30"
    started = time.monotonic()
    result = await CommandTrustedRunner(timeout_s=1).run(
        _check(script), workdir=str(repo), commit=commit
    )
    assert time.monotonic() - started < 10
    assert result.timed_out and not result.passed
    assert "started" in result.output_tail and "timed out after 1s" in result.output_tail
    assert len(_worktrees(repo)) == 1
    time.sleep(3.5)
    assert not witness.exists()


async def test_runner_check_timeout_overrides_default(repo: Path) -> None:
    commit = candidate_commit(repo)
    result = await CommandTrustedRunner(timeout_s=60).run(
        _check("sleep 10", timeout_s=1), workdir=str(repo), commit=commit
    )
    assert result.timed_out


async def test_runner_output_tail_is_clipped(repo: Path) -> None:
    commit = candidate_commit(repo)
    result = await CommandTrustedRunner(output_tail=50).run(
        _check("i=0; while [ $i -lt 200 ]; do echo line$i; i=$((i+1)); done"),
        workdir=str(repo),
        commit=commit,
    )
    assert result.output_tail.startswith("...[truncated]...")
    assert result.output_tail.endswith("line199")


async def test_runner_missing_executable(repo: Path) -> None:
    commit = candidate_commit(repo)
    check = Check(name="nope", command=["/definitely/not/here"], where="trusted")
    result = await CommandTrustedRunner().run(check, workdir=str(repo), commit=commit)
    assert not result.passed and "could not execute" in result.output_tail
    assert len(_worktrees(repo)) == 1


async def test_runner_bad_commit(repo: Path) -> None:
    result = await CommandTrustedRunner().run(_check("true"), workdir=str(repo), commit="0" * 40)
    assert not result.passed and "could not create worktree" in result.output_tail
    assert len(_worktrees(repo)) == 1


async def test_runner_outside_a_repo(tmp_path: Path) -> None:
    result = await CommandTrustedRunner().run(_check("true"), workdir=str(tmp_path), commit="abc")
    assert not result.passed and "not a git work tree" in result.output_tail


def test_command_runner_satisfies_protocol() -> None:
    assert isinstance(CommandTrustedRunner(), TrustedRunner)


# --- TrustedAwareVerifier -------------------------------------------------------------------
class RecordingRunner:
    def __init__(self, *, fail: bool = False, explode: bool = False) -> None:
        self.calls: list[tuple[str, str, str]] = []
        self._fail = fail
        self._explode = explode

    async def run(self, check: Check, *, workdir: str, commit: str) -> CheckResult:
        self.calls.append((check.name, workdir, commit))
        if self._explode:
            raise RuntimeError("runner host unreachable")
        return CheckResult(name=check.name, passed=not self._fail, exit_code=int(self._fail))


SANDBOX = Check(name="pytest", command=["pytest"])
TRUSTED_A = Check(name="trusted:a", command=["a"], where="trusted")
TRUSTED_B = Check(name="ci:b", command=["b"], where="trusted")


async def test_verifier_routes_and_merges_in_order(repo: Path) -> None:
    runner = RecordingRunner()
    session = FakeSession()
    verifier = TrustedAwareVerifier(DeterministicVerifier(), runner, str(repo))
    result = await verifier.verify(session, [TRUSTED_A, SANDBOX, TRUSTED_B])
    assert [r.name for r in result.results] == ["pytest", "trusted:a", "ci:b"]
    assert result.all_green
    assert session.calls == [["pytest"]]
    commits = {c[2] for c in runner.calls}
    assert len(commits) == 1  # one candidate commit shared by every trusted check
    assert [c[:2] for c in runner.calls] == [("trusted:a", str(repo)), ("ci:b", str(repo))]


async def test_verifier_trusted_failure_fails_the_verdict(repo: Path) -> None:
    verifier = TrustedAwareVerifier(DeterministicVerifier(), RecordingRunner(fail=True), str(repo))
    result = await verifier.verify(FakeSession(), [SANDBOX, TRUSTED_A])
    assert result.verdict == "failed"


async def test_verifier_only_sandbox_checks_skips_the_runner(repo: Path) -> None:
    runner = RecordingRunner()
    verifier = TrustedAwareVerifier(DeterministicVerifier(), runner, str(repo))
    result = await verifier.verify(FakeSession(), [SANDBOX])
    assert result.all_green and runner.calls == []


async def test_verifier_only_trusted_checks_skips_the_sandbox(repo: Path) -> None:
    session = FakeSession()
    verifier = TrustedAwareVerifier(DeterministicVerifier(), RecordingRunner(), str(repo))
    result = await verifier.verify(session, [TRUSTED_A])
    assert result.all_green and session.calls == []


async def test_verifier_no_checks_is_unverified(repo: Path) -> None:
    verifier = TrustedAwareVerifier(DeterministicVerifier(), RecordingRunner(), str(repo))
    result = await verifier.verify(FakeSession(), [])
    assert result == VerificationResult.from_results([])
    assert result.unverified


async def test_verifier_candidate_commit_failure_fails_trusted_checks(tmp_path: Path) -> None:
    runner = RecordingRunner()
    verifier = TrustedAwareVerifier(DeterministicVerifier(), runner, str(tmp_path))
    result = await verifier.verify(FakeSession(), [SANDBOX, TRUSTED_A, TRUSTED_B])
    assert runner.calls == []
    assert [r.passed for r in result.results] == [True, False, False]
    assert all(r.gating for r in result.results)
    assert "could not create the candidate commit" in result.results[1].output_tail
    assert result.verdict == "failed"


async def test_verifier_runner_exception_is_a_failed_check(repo: Path) -> None:
    verifier = TrustedAwareVerifier(
        DeterministicVerifier(), RecordingRunner(explode=True), str(repo)
    )
    result = await verifier.verify(FakeSession(), [TRUSTED_A])
    assert result.verdict == "failed"
    assert "runner host unreachable" in result.results[0].output_tail


async def test_verifier_end_to_end_with_command_runner(repo: Path) -> None:
    (repo / "feature.txt").write_text("done\n")
    verifier = TrustedAwareVerifier(DeterministicVerifier(), CommandTrustedRunner(), str(repo))
    check = Check(name="trusted:feature", command=["cat", "feature.txt"], where="trusted")
    result = await verifier.verify(FakeSession(), [SANDBOX, check])
    assert result.all_green and result.results[1].output_tail == "done"
    assert (
        subprocess.run(
            ["git", "status", "--porcelain"], cwd=repo, capture_output=True, text=True, check=True
        ).stdout.strip()
        == "?? feature.txt"
    )
