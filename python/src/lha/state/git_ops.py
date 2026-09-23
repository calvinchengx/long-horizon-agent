"""A thin, synchronous wrapper over the ``git`` CLI.

Kept deliberately small and dependency-free (no pygit2/dulwich): git itself is the durable
substrate, and shelling out to it is the most faithful, debuggable representation. Callers in
async code wrap these via ``asyncio.to_thread``.

Every invocation runs with a locale-independent, non-interactive environment (``LC_ALL=C``,
``GIT_TERMINAL_PROMPT=0``) and a timeout, and decisions are made from exit codes / plumbing
output — never from (localizable) porcelain messages.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

# Upper bound for any single git invocation (a hung credential helper or lock must not wedge a
# worker forever).
GIT_TIMEOUT_S = 120.0

# Paths ``reset_to_head`` never deletes even though they are untracked/ignored: dependency
# environments that are expensive to rebuild, local env/secret files the operator placed there,
# and the harness's own local object store.
RESET_KEEP: tuple[str, ...] = (".venv", "venv", "node_modules", ".env", ".env.*", ".lha/objects")


class GitError(RuntimeError):
    """A git command exited non-zero (or timed out)."""


def _git_env() -> dict[str, str]:
    env = dict(os.environ)
    env.update(
        {
            "LC_ALL": "C",
            "LANG": "C",
            "GIT_TERMINAL_PROMPT": "0",  # never block on a credential prompt
            "GIT_ASKPASS": "",
            "SSH_ASKPASS": "",
        }
    )
    return env


def _run(
    cwd: str | Path, args: list[str], *, timeout: float | None = None
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            ["git", *args],
            cwd=str(cwd),
            capture_output=True,
            text=True,
            env=_git_env(),
            timeout=GIT_TIMEOUT_S if timeout is None else timeout,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git {' '.join(args)} timed out after {exc.timeout}s") from exc


def run_git(cwd: str | Path, *args: str, check: bool = True, timeout: float | None = None) -> str:
    """Run ``git <args>`` in ``cwd`` and return stdout (stripped)."""
    proc = _run(cwd, list(args), timeout=timeout)
    if check and proc.returncode != 0:
        raise GitError(f"git {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip()}")
    return proc.stdout.strip()


def toplevel(cwd: str | Path) -> Path | None:
    """The (resolved) top-level directory of the work tree containing ``cwd``, or ``None``."""
    proc = _run(cwd, ["rev-parse", "--show-toplevel"])
    out = proc.stdout.strip()
    if proc.returncode != 0 or not out:
        return None
    return Path(out).resolve()


def is_repo(cwd: str | Path) -> bool:
    """True if ``cwd`` is the TOP LEVEL of a git work tree.

    Merely being *inside* some work tree is not enough: a workdir nested in another repository
    (e.g. ``.lha/workspaces/x`` under the user's project) must get its own repo — otherwise
    ``git config`` / ``git add -A`` / ``git commit`` would act on the enclosing repository.
    """
    root = toplevel(cwd)
    return root is not None and root == Path(cwd).resolve()


def init_repo(
    cwd: str | Path,
    *,
    author_name: str = "LHA Agent",
    author_email: str = "agent@lha.local",
) -> None:
    """Initialize a repo at ``cwd`` (idempotent) and set a local identity so commits work."""
    path = Path(cwd)
    path.mkdir(parents=True, exist_ok=True)
    if not is_repo(path):
        run_git(path, "init", "-b", "main")
    run_git(path, "config", "user.name", author_name)
    run_git(path, "config", "user.email", author_email)
    run_git(path, "config", "commit.gpgsign", "false")


def has_commits(cwd: str | Path) -> bool:
    """True if the repo has at least one commit."""
    return _run(cwd, ["rev-parse", "--verify", "--quiet", "HEAD"]).returncode == 0


def head_sha(cwd: str | Path) -> str:
    """Return the current HEAD sha, or '' if there are no commits yet."""
    if not has_commits(cwd):
        return ""
    return run_git(cwd, "rev-parse", "HEAD")


def commit_all(cwd: str | Path, message: str, *, force_paths: tuple[str, ...] = ()) -> str:
    """Stage everything and commit; return the resulting HEAD sha.

    ``force_paths`` are additionally staged with ``git add -f`` so they are committed even when
    the repository's ``.gitignore`` excludes them (e.g. the harness's own ``.lha/`` anchor files).

    If there is nothing to commit, this is a no-op that returns the current HEAD (so callers
    can treat "checkpoint with no changes" as benign and idempotent).
    """
    run_git(cwd, "add", "-A")
    existing = [p for p in force_paths if (Path(cwd) / p).exists()]
    if existing:
        run_git(cwd, "add", "-f", "--", *existing)
    # Exit code, not porcelain text: 0 = index matches HEAD (nothing staged), 1 = changes.
    staged = _run(cwd, ["diff", "--cached", "--quiet"])
    if staged.returncode == 0:
        return head_sha(cwd)
    if staged.returncode != 1:
        raise GitError(f"git diff --cached failed ({staged.returncode}): {staged.stderr.strip()}")
    run_git(cwd, "commit", "-m", message)
    return head_sha(cwd)


def exists_at_head(cwd: str | Path, relpath: str) -> bool:
    """True if ``relpath`` (relative to ``cwd``, not the repo root) exists in ``HEAD``."""
    if not has_commits(cwd):
        return False
    return _run(cwd, ["cat-file", "-e", f"HEAD:./{relpath}"]).returncode == 0


def show_at_head(cwd: str | Path, relpath: str) -> str:
    """The content of ``relpath`` (relative to ``cwd``) at ``HEAD`` (raises ``GitError``)."""
    return run_git(cwd, "show", f"HEAD:./{relpath}")


def show_at_head_bytes(cwd: str | Path, relpath: str) -> bytes:
    """The exact bytes of ``relpath`` at ``HEAD`` — unlike ``show_at_head``, nothing is stripped
    (a hash-chained log's trailing newline is significant). Raises ``GitError``."""
    try:
        proc = subprocess.run(
            ["git", "show", f"HEAD:./{relpath}"],
            cwd=str(cwd),
            capture_output=True,
            env=_git_env(),
            timeout=GIT_TIMEOUT_S,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git show HEAD:./{relpath} timed out after {exc.timeout}s") from exc
    if proc.returncode != 0:
        stderr = proc.stderr.decode("utf-8", errors="replace").strip()
        raise GitError(f"git show HEAD:./{relpath} failed ({proc.returncode}): {stderr}")
    return proc.stdout


def git_dir(cwd: str | Path) -> Path:
    """Absolute path of the repository's ``.git`` directory."""
    return Path(run_git(cwd, "rev-parse", "--absolute-git-dir"))


def reset_to_head(cwd: str | Path, *, keep: tuple[str, ...] = RESET_KEEP) -> None:
    """Discard ALL uncommitted work: tracked edits, staged changes, untracked and ignored files.

    Used at the start of every durable cycle attempt so a crashed attempt's partial edits can
    never leak into the next commit. ``keep`` lists paths (dependency envs, the harness's local
    object store) that survive the clean. No-op on a repo without commits.
    """
    if not has_commits(cwd):
        return
    run_git(cwd, "reset", "--hard", "--quiet", "HEAD")
    excludes = [arg for path in keep for arg in ("-e", path)]
    run_git(cwd, "clean", "-ffdxq", *excludes)


def list_branches(cwd: str | Path, *, include_remote: bool = False) -> list[str]:
    """Return branch names (empty if no commits yet).

    With ``include_remote`` the remote-tracking branches are included too, as ``<remote>/<name>``
    (the symbolic ``<remote>/HEAD`` alias is skipped).
    """
    if not has_commits(cwd):
        return []
    refs = ["refs/heads"] + (["refs/remotes"] if include_remote else [])
    out = run_git(cwd, "for-each-ref", "--format=%(refname:short)", *refs)
    return [
        line.strip() for line in out.splitlines() if line.strip() and not line.endswith("/HEAD")
    ]


def commits_ahead(cwd: str | Path, base: str, ref: str) -> int:
    """Number of commits reachable from ``ref`` but not from ``base``."""
    return int(run_git(cwd, "rev-list", "--count", f"{base}..{ref}", "--") or "0")


def log_oneline(cwd: str | Path, n: int = 10) -> list[str]:
    """Return up to ``n`` recent commits as ``<short-sha> <subject>`` lines."""
    if not has_commits(cwd):
        return []
    out = run_git(cwd, "log", f"-{n}", "--pretty=format:%h %s")
    return [line for line in out.splitlines() if line.strip()]
