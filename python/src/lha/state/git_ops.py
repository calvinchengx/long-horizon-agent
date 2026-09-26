"""A thin, synchronous wrapper over the ``git`` CLI.

Kept deliberately small and dependency-free (no pygit2/dulwich): git itself is the durable
substrate, and shelling out to it is the most faithful, debuggable representation. Callers in
async code wrap these via ``asyncio.to_thread``.

Every invocation runs with a locale-independent, non-interactive environment (``LC_ALL=C``,
``GIT_TERMINAL_PROMPT=0``) and a timeout, and decisions are made from exit codes / plumbing
output — never from (localizable) porcelain messages.

Hardening: the work tree a harness git command runs in was just written by an untrusted agent,
so nothing in it (nor anything in the operator's environment) may make git execute code:

* a MINIMAL environment (``PATH``, a private empty ``HOME``, C locale, no prompts): no ``GIT_*``
  variable of the operator's passes through (``GIT_DIR``, ``GIT_WORK_TREE``, ``GIT_CONFIG_*``
  ...), and neither the system nor the global config is read (``GIT_CONFIG_NOSYSTEM``,
  ``GIT_CONFIG_GLOBAL=/dev/null``, ``GIT_ATTR_NOSYSTEM``);
* fixed ``-c`` overrides of every exec-capable scalar key (hooks, fsmonitor, ssh/editor/pager/
  askpass/credential helpers, external diff, signing, transports) — ``-c`` beats every file;
* attribute-driven drivers (``filter.<x>.clean|smudge|process``, ``diff.<x>.textconv|command``,
  ``merge.<x>.driver``) are NAMED by ``.gitattributes``, which the agent writes, but DEFINED
  only in config. Before every command that may run one, the remaining config (the repository's
  own, which the sandbox cannot write) is enumerated and each defined driver is overridden with
  an empty command, so none runs (an attribute naming an undefined driver runs nothing). A
  config value that comes from a file inside the work tree (an ``include.path`` into it) is
  refused outright;
* a ``.git`` FILE (linked worktree) is re-validated before anything runs (``git_link``).
"""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
from pathlib import Path

from lha.state import git_link
from lha.state.git_link import GitLinkError

# Upper bound for any single git invocation (a hung credential helper or lock must not wedge a
# worker forever).
GIT_TIMEOUT_S = 120.0

# Paths ``reset_to_head`` never deletes even though they are untracked/ignored: dependency
# environments that are expensive to rebuild, local env/secret files the operator placed there,
# and the harness's own local object store.
RESET_KEEP: tuple[str, ...] = (".venv", "venv", "node_modules", ".env", ".env.*", ".lha/objects")


class GitError(RuntimeError):
    """A git command exited non-zero (or timed out)."""


# Exec-capable config with a scalar key, always overridden on the command line.
HARDENING_CONFIG: tuple[tuple[str, str], ...] = (
    ("core.hooksPath", os.devnull),  # no hook can exist under /dev/null
    ("core.fsmonitor", "false"),
    ("core.sshCommand", ""),
    ("core.gitProxy", ""),
    ("core.askPass", ""),
    ("core.editor", ":"),
    ("sequence.editor", ":"),
    ("core.pager", "cat"),
    ("core.alternateRefsCommand", ""),
    ("credential.helper", ""),
    ("diff.external", ""),
    ("commit.gpgSign", "false"),
    ("tag.gpgSign", "false"),
    ("gpg.program", ""),
    ("protocol.allow", "never"),  # push/fetch-free: no transport, no remote helper
)

# Attribute-driven drivers: the attribute (agent-writable) only names one; config defines it.
_DRIVER_KEY = re.compile(
    r"^(filter\..+\.(clean|smudge|process)|diff\..+\.(textconv|command)|merge\..+\.driver)$",
    re.IGNORECASE,
)

# Subcommands (as LHA invokes them) that never run a filter, textconv or merge driver: they
# skip the per-command config enumeration.
_NO_DRIVER_SUBCOMMANDS: frozenset[str] = frozenset(
    {
        "branch",
        "cat-file",
        "commit-tree",
        "config",
        "for-each-ref",
        "init",
        "ls-files",
        "read-tree",
        "rev-list",
        "rev-parse",
        "symbolic-ref",
        "update-ref",
        "write-tree",
    }
)

# The only operator variables a harness git inherits; every GIT_* and everything else is dropped.
_PASSTHROUGH_ENV: tuple[str, ...] = ("PATH", "TMPDIR", "SYSTEMROOT")

_home: list[str] = []


def _safe_home() -> str:
    """A private, empty directory used as git's ``HOME`` (no ``~/.gitconfig``, no XDG config)."""
    if not _home or not Path(_home[0]).is_dir():
        _home[:] = [tempfile.mkdtemp(prefix="lha-git-home-")]
    return _home[0]


def _git_env() -> dict[str, str]:
    """The minimal environment every harness git runs with (nothing ``GIT_*`` inherited)."""
    env = {k: os.environ[k] for k in _PASSTHROUGH_ENV if k in os.environ}
    env.setdefault("PATH", os.defpath)
    home = _safe_home()
    env.update(
        {
            "HOME": home,
            "XDG_CONFIG_HOME": home,
            "LC_ALL": "C",
            "LANG": "C",
            "GIT_TERMINAL_PROMPT": "0",  # never block on a credential prompt
            "GIT_ASKPASS": "",
            "SSH_ASKPASS": "",
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_ATTR_NOSYSTEM": "1",
        }
    )
    return env


def _config_args(pairs: list[tuple[str, str]]) -> list[str]:
    return [arg for key, value in pairs for arg in ("-c", f"{key}={value}")]


def _subcommand(args: list[str]) -> str:
    return next((a for a in args if not a.startswith("-")), "")


def _discovered_root(cwd: str | Path) -> Path:
    """The work tree git will use from ``cwd``: the nearest ancestor holding a ``.git``.

    (Git's own discovery, which the minimal environment leaves unconfigured.) ``cwd`` itself,
    resolved, when there is none. Relative config origins are relative to this directory.
    """
    here = Path(cwd).resolve()
    for candidate in (here, *here.parents):
        if os.path.lexists(candidate / ".git"):
            return candidate
    return here


def _untrusted_origin(root: Path, origin: str) -> Path | None:
    """The config file behind ``origin`` if it lies in the agent-writable part of ``root``."""
    if not origin.startswith("file:"):
        return None
    source = (root / origin[len("file:") :]).resolve()
    if not source.is_relative_to(root):
        return None
    dotgit = root / ".git"
    protected = (
        dotgit.is_dir()
        and not dotgit.is_symlink()
        and source.is_relative_to(dotgit)
        and not source.is_relative_to(dotgit / "lha-worktrees")  # implementers write there
    )
    return None if protected else source


def _driver_overrides(cwd: str | Path) -> list[tuple[str, str]]:
    """``(key, "")`` for every filter/textconv/merge driver defined in ``cwd``'s config.

    Raises ``GitError`` if any config value comes from a file inside the work tree (outside its
    ``.git`` directory): that file is agent-writable, so nothing in it can be trusted.
    """
    argv = ["git", *_config_args(list(HARDENING_CONFIG)), "config", "--list", "--show-origin"]
    try:
        proc = subprocess.run(
            [*argv, "--name-only", "-z"],
            cwd=str(cwd),
            capture_output=True,
            env=_git_env(),
            timeout=GIT_TIMEOUT_S,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git config --list timed out after {exc.timeout}s") from exc
    if proc.returncode != 0:
        return []  # not a repository (yet): the command itself fails or creates one
    fields = proc.stdout.decode("utf-8", errors="surrogateescape").split("\0")
    root = _discovered_root(cwd)
    overrides: list[tuple[str, str]] = []
    for origin, key in zip(fields[0::2], fields[1::2], strict=False):
        source = _untrusted_origin(root, origin)
        if source is not None:
            raise GitError(
                f"refusing to run git in {cwd}: config {key!r} comes from {source}, "
                "a file inside the work tree"
            )
        if _DRIVER_KEY.match(key) and (key, "") not in overrides:
            overrides.append((key, ""))
    return overrides


def git_argv(cwd: str | Path, args: list[str]) -> list[str]:
    """The hardened ``git`` argv for ``args`` run in ``cwd``.

    Raises ``GitError`` when ``cwd``'s ``.git`` pointer or its config cannot be trusted.
    """
    check_git_link(_discovered_root(cwd))
    pairs = list(HARDENING_CONFIG)
    if _subcommand(args) not in _NO_DRIVER_SUBCOMMANDS:
        pairs += _driver_overrides(cwd)
    return ["git", *_config_args(pairs), *args]


def _run(
    cwd: str | Path,
    args: list[str],
    *,
    timeout: float | None = None,
    extra_env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    argv = git_argv(cwd, args)
    env = _git_env()
    env.update(extra_env or {})
    try:
        return subprocess.run(
            argv,
            cwd=str(cwd),
            capture_output=True,
            text=True,
            env=env,
            timeout=GIT_TIMEOUT_S if timeout is None else timeout,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git {' '.join(args)} timed out after {exc.timeout}s") from exc


def run_git_bytes(
    cwd: str | Path, *args: str, timeout: float | None = None
) -> subprocess.CompletedProcess[bytes]:
    """Hardened ``git <args>`` in ``cwd``: the completed process, raw bytes, exit code unchecked."""
    argv = git_argv(cwd, list(args))
    try:
        return subprocess.run(
            argv,
            cwd=str(cwd),
            capture_output=True,
            env=_git_env(),
            timeout=GIT_TIMEOUT_S if timeout is None else timeout,
            stdin=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired as exc:
        raise GitError(f"git {' '.join(args)} timed out after {exc.timeout}s") from exc


def run_git_env(cwd: str | Path, args: list[str], extra_env: dict[str, str]) -> str:
    """``run_git`` with ``extra_env`` (e.g. ``GIT_INDEX_FILE``, an identity) added."""
    proc = _run(cwd, args, extra_env=extra_env)
    if proc.returncode != 0:
        raise GitError(f"git {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip()}")
    return proc.stdout.strip()


def check_git_link(cwd: str | Path, *, expected_common_dir: str | Path | None = None) -> None:
    """Refuse (``GitError``) a ``.git`` in ``cwd`` that is not the repository it should be."""
    try:
        git_link.check(cwd, expected_common_dir=expected_common_dir)
    except GitLinkError as exc:
        raise GitError(f"refusing to run git in {cwd}: {exc}") from exc


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


def commit_paths(cwd: str | Path, message: str, paths: tuple[str, ...]) -> str:
    """Commit ONLY ``paths`` (force-added, so ignored files count); return the HEAD sha.

    Every other change in the work tree or the index is left exactly as it was (``git commit
    --only``). A no-op returning the current HEAD when those paths are unchanged.
    """
    existing = [p for p in paths if (Path(cwd) / p).exists()]
    if not existing:
        return head_sha(cwd)
    run_git(cwd, "add", "-f", "--", *existing)
    staged = _run(cwd, ["diff", "--cached", "--quiet", "--", *existing])
    if staged.returncode == 0:
        return head_sha(cwd)
    if staged.returncode != 1:
        raise GitError(f"git diff --cached failed ({staged.returncode}): {staged.stderr.strip()}")
    run_git(cwd, "commit", "--only", "-m", message, "--", *existing)
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
    proc = run_git_bytes(cwd, "show", f"HEAD:./{relpath}")
    if proc.returncode != 0:
        stderr = proc.stderr.decode("utf-8", errors="replace").strip()
        raise GitError(f"git show HEAD:./{relpath} failed ({proc.returncode}): {stderr}")
    return proc.stdout


def git_dir(cwd: str | Path) -> Path:
    """Absolute path of the repository's ``.git`` directory."""
    return Path(run_git(cwd, "rev-parse", "--absolute-git-dir"))


def common_dir(cwd: str | Path) -> Path:
    """The (resolved) shared repository directory — ``git_dir`` for all but a linked worktree."""
    out = run_git(cwd, "rev-parse", "--git-common-dir")
    path = Path(out)
    return (path if path.is_absolute() else Path(cwd) / path).resolve()


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


def discard_changes(cwd: str | Path) -> None:
    """Return the work tree to ``HEAD``: tracked edits and untracked (NOT ignored) files go.

    Unlike ``reset_to_head`` this keeps ignored files (dependency caches, build outputs, local
    remotes), because it runs after an ordinary failed attempt, not after a crash.
    """
    if not has_commits(cwd):
        return
    run_git(cwd, "reset", "--hard", "--quiet", "HEAD")
    run_git(cwd, "clean", "-fdq")


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
