"""Trusted checks: operator-defined commands run OUTSIDE the sandbox, on the candidate commit.

Some acceptance evidence cannot be produced inside the sandbox: an e2e suite that needs a Docker
daemon, a real SQL Server, a GPU, or a CI job. A ``Check`` with ``where="trusted"`` is run by a
``TrustedRunner`` instead of the sandbox session, against a clean, detached ``git worktree`` of
the *candidate commit* — a commit object of the current working tree (tracked + untracked,
non-ignored files) made WITHOUT touching HEAD, the real index or the working tree.

Security trade-off (read before enabling): a trusted check executes code the AGENT wrote (tests,
build scripts, ``conftest.py``, ``Makefile`` targets...) outside the sandbox, as the operator's
user — for example with access to a Docker socket, which is root-equivalent on the host. The
sandbox's egress and filesystem limits do not apply. Defining ``LHA_TRUSTED_CHECKS`` is the
explicit opt-in: with it unset, nothing runs on the host. Mitigations built in here:

* only operator-defined commands can be trusted checks: items reference them by NAME
  (``trusted:<name>`` witnesses resolved against ``LHA_TRUSTED_CHECKS``), never define argv;
* the command runs in a throwaway worktree of a pinned commit, which is always removed;
* the command gets a MINIMAL environment, never the operator's: ``PATH``, the locale, a fresh
  empty ``HOME`` and ``TMPDIR`` (removed afterwards), the ``LHA_CHECK_*`` variables, and only the
  extra names the operator lists in ``LHA_TRUSTED_CHECK_ENV`` (``trusted_env``). API keys, cloud
  credentials, ``SSH_AUTH_SOCK`` and ``LHA_*`` settings are not passed unless listed, and
  ``LHA_*`` names cannot be listed at all;
* the check has a timeout, after which its whole process group is killed.

That does not make agent code safe to run: the process still runs as the operator's user and can
read any file that user can (``~/.aws/credentials`` by absolute path, for example). The runner
should be a dedicated, disposable machine or VM (no secrets, no production credentials), or the
trusted command should be a small script that hands the commit (``LHA_CHECK_COMMIT``) off to CI
and waits for its verdict.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import re
import shutil
import signal
import subprocess
import tempfile
import time
from collections.abc import Callable, Iterable, Mapping
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

# Host variables every trusted check inherits (the Windows ones are needed to start processes).
_BASE_ENV = (
    "PATH",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "TZ",
    "SYSTEMROOT",
    "SYSTEMDRIVE",
    "COMSPEC",
    "PATHEXT",
    "WINDIR",
)
_ENV_NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def validate_env_allow_list(names: Iterable[str]) -> list[str]:
    """Check ``LHA_TRUSTED_CHECK_ENV`` names; return them (raises ``ValueError``).

    A name must be a plain environment variable name, and may not start with ``LHA_`` (LHA's own
    settings carry its API keys; the ``LHA_CHECK_*`` variables are set by the runner).
    """
    out: list[str] = []
    for name in names:
        if not _ENV_NAME.fullmatch(name):
            raise ValueError(f"LHA_TRUSTED_CHECK_ENV: {name!r} is not an environment variable name")
        if name.upper().startswith("LHA_"):
            raise ValueError(
                f"LHA_TRUSTED_CHECK_ENV: {name!r} cannot be passed to trusted checks (LHA_* "
                "settings hold LHA's own credentials)"
            )
        out.append(name)
    return out


def trusted_env(
    *,
    home: str,
    tmpdir: str,
    allow: Iterable[str] = (),
    check: Mapping[str, str] | None = None,
    base: Mapping[str, str] | None = None,
) -> dict[str, str]:
    """The environment of a trusted check: minimal, never the operator's whole environment.

    ``PATH`` and the locale (plus the Windows process essentials) come from ``base`` (default
    ``os.environ``); ``HOME`` is ``home`` and ``TMPDIR`` is ``tmpdir`` (both fresh and empty; on
    Windows also ``USERPROFILE`` / ``TEMP`` / ``TMP``); then each ``allow`` name present in
    ``base`` is copied verbatim (the operator's explicit choice, so it may override those
    defaults); ``check`` (the ``LHA_CHECK_*`` values) is applied last.
    """
    source = os.environ if base is None else base
    env = {k: source[k] for k in _BASE_ENV if k in source}
    env.setdefault("PATH", os.defpath)
    env.setdefault("LANG", "C.UTF-8")
    env.update({"HOME": home, "TMPDIR": tmpdir, "GIT_TERMINAL_PROMPT": "0"})
    if os.name == "nt":  # pragma: no cover - Windows only
        env.update({"USERPROFILE": home, "TEMP": tmpdir, "TMP": tmpdir})
    for name in validate_env_allow_list(allow):
        if name in source:
            env[name] = source[name]
    env.update(check or {})
    return env


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


def candidate_commit(workdir: str | Path, *, message: str = CANDIDATE_MESSAGE) -> str:
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
        args = ["commit-tree", "--no-gpg-sign", tree, "-m", message]
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

    The command gets a minimal environment (``trusted_env``): ``PATH``, the locale, a fresh
    ``HOME`` and ``TMPDIR``, the names in ``env_allow`` (``LHA_TRUSTED_CHECK_ENV``) and
    ``LHA_CHECK_COMMIT``, ``LHA_CHECK_WORKTREE`` and ``LHA_CHECK_NAME``; its cwd is the worktree
    directory that corresponds to ``workdir``. On timeout the whole process group is killed. The
    worktree and the temporary home are removed whatever happens. Raises ``ValueError`` for an
    ``env_allow`` name ``validate_env_allow_list`` refuses.
    """

    def __init__(
        self,
        *,
        timeout_s: int = 3600,
        output_tail: int = OUTPUT_TAIL_CHARS,
        env_allow: Iterable[str] = (),
    ) -> None:
        self._timeout = timeout_s
        self._tail = output_tail
        self._env_allow = validate_env_allow_list(env_allow)

    async def run(self, check: Check, *, workdir: str, commit: str) -> CheckResult:
        started = time.monotonic()
        root = await asyncio.to_thread(toplevel, workdir)
        if root is None:
            return _failed(check, f"[trusted] {workdir} is not a git work tree", started=started)
        worktree = Path(tempfile.mkdtemp(prefix="lha-trusted-")).resolve()
        home = Path(tempfile.mkdtemp(prefix="lha-trusted-home-")).resolve()
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
            tmpdir = home / "tmp"
            tmpdir.mkdir()
            env = trusted_env(
                home=str(home),
                tmpdir=str(tmpdir),
                allow=self._env_allow,
                check={
                    "LHA_CHECK_COMMIT": commit,
                    "LHA_CHECK_WORKTREE": str(worktree),
                    "LHA_CHECK_NAME": check.name,
                },
            )
            return await self._exec(check, cwd=cwd, env=env, started=started)
        finally:
            await asyncio.to_thread(_remove_worktree, root, worktree)
            await asyncio.to_thread(shutil.rmtree, home, True)

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
                _kill_tree(proc.pid, proc.kill)
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


def _kill_tree(pid: int, kill: Callable[[], None]) -> None:
    """SIGKILL the check's process group (it leads a new session); a plain kill elsewhere."""
    killpg = getattr(os, "killpg", None)
    with contextlib.suppress(ProcessLookupError, PermissionError):
        if killpg is not None:
            killpg(pid, getattr(signal, "SIGKILL", signal.SIGTERM))
        else:  # pragma: no cover - Windows has no process groups to signal
            kill()


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
