"""Trusted checks: operator-defined commands run OUTSIDE the sandbox, on the candidate commit.

Some acceptance evidence cannot be produced inside the sandbox: an e2e suite that needs a Docker
daemon, a real SQL Server, a GPU, or a CI job. A ``Check`` with ``where="trusted"`` is run by a
``TrustedRunner`` instead of the sandbox session, against a clean, detached ``git worktree`` of
the *candidate commit* — a commit object of the current working tree (tracked + untracked,
non-ignored files) made WITHOUT touching HEAD, the real index or the working tree.

Security trade-off (read before enabling): a trusted check executes code the AGENT wrote (tests,
build scripts, ``conftest.py``, ``Makefile`` targets...) outside the sandbox, with the runner's
privileges — for example access to a Docker socket, which is root-equivalent on the host. The
sandbox's egress and filesystem limits do not apply. Mitigations built in here:

* only operator-defined commands can be trusted checks: items reference them by NAME
  (``trusted:<name>`` witnesses resolved against ``LHA_TRUSTED_CHECKS``), never define argv;
* the command runs in a throwaway worktree of a pinned commit, which is always removed.

That does not make agent code safe to run. The runner should be a dedicated, disposable machine
or VM (no secrets, no production credentials), or ``CommandTrustedRunner`` should invoke a small
script that hands the commit (``LHA_CHECK_COMMIT``) off to CI and waits for its verdict.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import shutil
import signal
import subprocess
import tempfile
import time
from pathlib import Path
from typing import Protocol, runtime_checkable

from lha.contracts.sandbox import SandboxSession
from lha.contracts.verify import Check, CheckResult, VerificationResult, Verifier
from lha.state.git_ops import GIT_TIMEOUT_S, GitError, _git_env, has_commits, run_git, toplevel
from lha.verify.verifier import OUTPUT_TAIL_CHARS, clip_output_tail

CANDIDATE_MESSAGE = "lha: candidate commit for trusted checks"
_IDENTITY = {
    "GIT_AUTHOR_NAME": "LHA Verifier",
    "GIT_AUTHOR_EMAIL": "verifier@lha.local",
    "GIT_COMMITTER_NAME": "LHA Verifier",
    "GIT_COMMITTER_EMAIL": "verifier@lha.local",
}


def _git_with_env(cwd: str | Path, args: list[str], extra_env: dict[str, str]) -> str:
    env = _git_env()
    env.update(extra_env)
    try:
        proc = subprocess.run(
            ["git", *args],
            cwd=str(cwd),
            capture_output=True,
            text=True,
            env=env,
            timeout=GIT_TIMEOUT_S,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git {' '.join(args)} timed out after {exc.timeout}s") from exc
    if proc.returncode != 0:
        raise GitError(f"git {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip()}")
    return proc.stdout.strip()


def candidate_commit(workdir: str | Path) -> str:
    """Commit the CURRENT working tree of ``workdir``'s repo without touching HEAD/index/tree.

    Stages everything (tracked edits, deletions, untracked non-ignored files) into a TEMPORARY
    index (``GIT_INDEX_FILE``), writes its tree and creates a commit whose parent is HEAD (no
    parent if the repo has no commits). No ref points at the result. Raises ``GitError``.
    """
    root = toplevel(workdir)
    if root is None:
        raise GitError(f"{workdir} is not inside a git work tree")
    tmpdir = tempfile.mkdtemp(prefix="lha-candidate-index-")
    try:
        env = {"GIT_INDEX_FILE": str(Path(tmpdir) / "index"), **_IDENTITY}
        parent = has_commits(root)
        if parent:
            _git_with_env(root, ["read-tree", "HEAD"], env)
        _git_with_env(root, ["add", "-A"], env)
        tree = _git_with_env(root, ["write-tree"], env)
        args = ["commit-tree", "--no-gpg-sign", tree, "-m", CANDIDATE_MESSAGE]
        if parent:
            args[1:1] = ["-p", "HEAD"]
        return _git_with_env(root, args, env)
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


@runtime_checkable
class TrustedRunner(Protocol):
    """Runs a ``where="trusted"`` check outside the sandbox against ``commit``."""

    async def run(self, check: Check, *, workdir: str, commit: str) -> CheckResult:
        """Run ``check`` on ``commit`` of the repository at ``workdir``; never raise."""
        ...


def _failed(check: Check, message: str, *, started: float, timed_out: bool = False) -> CheckResult:
    return CheckResult(
        name=check.name,
        passed=False,
        exit_code=-1,
        gating=check.gating,
        duration_s=time.monotonic() - started,
        timed_out=timed_out,
        output_tail=message,
    )


class CommandTrustedRunner:
    """Runs a trusted check's argv on the HOST in a detached worktree of the candidate commit.

    The command gets the host environment plus ``LHA_CHECK_COMMIT``, ``LHA_CHECK_WORKTREE`` and
    ``LHA_CHECK_NAME``; its cwd is the worktree directory that corresponds to ``workdir``. On
    timeout the whole process group is killed. The worktree is removed whatever happens.
    """

    def __init__(self, *, timeout_s: int = 3600, output_tail: int = OUTPUT_TAIL_CHARS) -> None:
        self._timeout = timeout_s
        self._tail = output_tail

    async def run(self, check: Check, *, workdir: str, commit: str) -> CheckResult:
        started = time.monotonic()
        root = await asyncio.to_thread(toplevel, workdir)
        if root is None:
            return _failed(check, f"[trusted] {workdir} is not a git work tree", started=started)
        worktree = Path(tempfile.mkdtemp(prefix="lha-trusted-")).resolve()
        try:
            try:
                await asyncio.to_thread(
                    run_git, root, "worktree", "add", "--detach", str(worktree), commit
                )
            except GitError as exc:
                return _failed(
                    check, f"[trusted] could not create worktree: {exc}", started=started
                )
            relative = Path(workdir).resolve().relative_to(root)
            cwd = worktree / relative
            env = dict(os.environ)
            env.update(
                {
                    "LHA_CHECK_COMMIT": commit,
                    "LHA_CHECK_WORKTREE": str(worktree),
                    "LHA_CHECK_NAME": check.name,
                }
            )
            return await self._exec(check, cwd=cwd, env=env, started=started)
        finally:
            await asyncio.to_thread(_remove_worktree, root, worktree)

    async def _exec(
        self, check: Check, *, cwd: Path, env: dict[str, str], started: float
    ) -> CheckResult:
        timeout = check.timeout_s or self._timeout
        # Output goes to a file so a timed-out check still shows what it printed.
        with tempfile.TemporaryFile() as log:
            try:
                proc = await asyncio.create_subprocess_exec(
                    *check.command,
                    cwd=str(cwd),
                    env=env,
                    stdin=subprocess.DEVNULL,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                    start_new_session=True,
                )
            except OSError as exc:
                return _failed(check, f"[trusted] could not execute check: {exc}", started=started)
            timed_out = False
            try:
                exit_code = await asyncio.wait_for(proc.wait(), timeout=timeout)
            except TimeoutError:
                timed_out = True
                with contextlib.suppress(ProcessLookupError, PermissionError):
                    os.killpg(proc.pid, signal.SIGKILL)
                exit_code = await proc.wait()
            log.seek(0)
            output = log.read().decode("utf-8", errors="replace")
        tail = clip_output_tail(output, "", limit=self._tail)
        if timed_out:
            tail = f"{tail}\n[trusted] timed out after {timeout}s".lstrip("\n")
        return CheckResult(
            name=check.name,
            passed=exit_code == 0 and not timed_out,
            exit_code=exit_code,
            gating=check.gating,
            duration_s=time.monotonic() - started,
            timed_out=timed_out,
            output_tail=tail,
        )


def _remove_worktree(root: Path, worktree: Path) -> None:
    with contextlib.suppress(GitError):
        run_git(root, "worktree", "remove", "--force", str(worktree), check=False)
        run_git(root, "worktree", "prune", check=False)
    shutil.rmtree(worktree, ignore_errors=True)


class TrustedAwareVerifier:
    """A ``Verifier`` that sends sandbox checks to ``inner`` and trusted checks to ``runner``.

    Trusted checks all run against ONE candidate commit of ``host_workdir`` (the host-side copy
    of the workspace). Results are merged in order: sandbox results, then trusted results. A
    failure to build the candidate commit turns every trusted check into a failed gating result
    — this verifier never raises for it.
    """

    def __init__(self, inner: Verifier, runner: TrustedRunner, host_workdir: str) -> None:
        self._inner = inner
        self._runner = runner
        self._host_workdir = host_workdir

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        sandbox = [c for c in checks if c.where == "sandbox"]
        trusted = [c for c in checks if c.where == "trusted"]
        results: list[CheckResult] = []
        if sandbox or not trusted:
            results.extend((await self._inner.verify(session, sandbox)).results)
        if trusted:
            results.extend(await self._run_trusted(trusted))
        return VerificationResult.from_results(results)

    async def _run_trusted(self, checks: list[Check]) -> list[CheckResult]:
        started = time.monotonic()
        try:
            commit = await asyncio.to_thread(candidate_commit, self._host_workdir)
        except Exception as exc:
            message = (
                f"[trusted] could not create the candidate commit: {type(exc).__name__}: {exc}"
            )
            return [_failed(c, message, started=started) for c in checks]
        results: list[CheckResult] = []
        for check in checks:
            try:
                results.append(
                    await self._runner.run(check, workdir=self._host_workdir, commit=commit)
                )
            except Exception as exc:  # a runner failure is a failed check, never a pass
                results.append(
                    _failed(
                        check,
                        f"[trusted] runner failed: {type(exc).__name__}: {exc}",
                        started=time.monotonic(),
                    )
                )
        return results
