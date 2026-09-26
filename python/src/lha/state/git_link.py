"""Validate a linked work tree's ``.git`` FILE (``gitdir: <path>``) before trusting it.

In a linked git worktree (``git worktree add``) ``.git`` is not a directory but a one-line file
pointing at the work tree's private git dir, ``<common>/worktrees/<name>``, whose ``commondir``
file points back at the shared repository. Whoever can rewrite that file decides which
repository — which config, which hooks — the host's next ``git add``/``git commit`` in that work
tree uses. The sandbox mounts it read-only (``DockerSandbox``), and the host re-validates it
here before every harness git invocation, failing closed on anything unexpected.

Dependency-free so both ``lha.state.git_ops`` and ``lha.execution`` can use it.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

_MAX_POINTER_BYTES = 4096


class GitLinkError(ValueError):
    """A ``.git`` pointer that is malformed, dangling, or points somewhere it must not."""


@dataclass(frozen=True)
class GitLink:
    """A parsed ``.git`` file: the work tree's git dir and the repository's common dir."""

    git_dir: Path  # resolved: <common>/worktrees/<name> (or a submodule's modules/<name>)
    common_dir: Path  # resolved: the shared repository (== git_dir for a submodule)


def _resolve_rel(base: Path, value: str) -> Path:
    path = Path(value)
    return (path if path.is_absolute() else base / path).resolve()


def read_pointer(worktree: str | Path) -> Path:
    """The resolved target of ``<worktree>/.git`` (a ``gitdir:`` file); raises ``GitLinkError``."""
    dotgit = Path(worktree) / ".git"
    try:
        raw = dotgit.read_bytes()[: _MAX_POINTER_BYTES + 1]
    except OSError as exc:
        raise GitLinkError(f"{dotgit}: unreadable ({exc.strerror})") from exc
    if len(raw) > _MAX_POINTER_BYTES:
        raise GitLinkError(f"{dotgit}: not a gitdir pointer (too large)")
    text = raw.decode("utf-8", errors="replace")
    lines = [line for line in text.splitlines() if line.strip()]
    if len(lines) != 1 or not lines[0].startswith("gitdir: "):
        raise GitLinkError(f"{dotgit}: not a single 'gitdir: <path>' line")
    value = lines[0][len("gitdir: ") :].strip()
    if not value:
        raise GitLinkError(f"{dotgit}: empty gitdir pointer")
    return _resolve_rel(Path(worktree).resolve(), value)


def validate(worktree: str | Path, *, expected_common_dir: str | Path | None = None) -> GitLink:
    """Check ``<worktree>/.git`` (a FILE) points at a genuine git dir outside the work tree.

    Refused (``GitLinkError``): a malformed pointer; a target that is not a directory; a linked
    worktree git dir whose ``commondir`` does not lead to a repository containing it under
    ``worktrees/``, or whose ``gitdir`` back-link is not this ``.git``; a git dir or common dir
    that lies INSIDE the work tree (the part a sandboxed process can write, so it could plant a
    repository with its own config and hooks there); and, with ``expected_common_dir``, any
    other repository than that one.
    """
    root = Path(worktree).resolve()
    dotgit = root / ".git"
    target = read_pointer(root)
    if not target.is_dir():
        raise GitLinkError(f"{dotgit}: gitdir {target} is not a directory")
    commondir_file = target / "commondir"
    if commondir_file.is_file():
        common = _resolve_rel(target, commondir_file.read_text(errors="replace").strip())
        if target.parent != common / "worktrees":
            raise GitLinkError(f"{dotgit}: gitdir {target} is not a worktree of {common}")
        backlink_file = target / "gitdir"
        backlink = (
            _resolve_rel(target, backlink_file.read_text(errors="replace").strip())
            if backlink_file.is_file()
            else None
        )
        if backlink != dotgit:
            raise GitLinkError(f"{dotgit}: gitdir {target} does not link back to this work tree")
    else:  # a submodule-style separate git dir: a whole repository of its own
        common = target
    if not (common / "HEAD").is_file() or not (common / "objects").is_dir():
        raise GitLinkError(f"{dotgit}: {common} is not a git repository")
    for path in (target, common):
        if path.is_relative_to(root):
            raise GitLinkError(f"{dotgit}: gitdir {path} lies inside the work tree")
    if expected_common_dir is not None and common != Path(expected_common_dir).resolve():
        raise GitLinkError(
            f"{dotgit}: points at repository {common}, expected {Path(expected_common_dir).resolve()}"
        )
    return GitLink(git_dir=target, common_dir=common)


def check(worktree: str | Path, *, expected_common_dir: str | Path | None = None) -> None:
    """Validate ``<worktree>/.git`` when it is a file or a symlink; a directory (or none) passes.

    A symlinked ``.git`` is refused outright: nothing LHA creates is one, and following it is
    exactly the redirection this guard exists to stop. With ``expected_common_dir`` a ``.git``
    DIRECTORY must also be that repository.
    """
    dotgit = Path(worktree) / ".git"
    if dotgit.is_symlink():
        raise GitLinkError(f"{dotgit}: is a symlink")
    if dotgit.is_file():
        validate(worktree, expected_common_dir=expected_common_dir)
    elif (
        expected_common_dir is not None
        and dotgit.is_dir()
        and dotgit.resolve() != Path(expected_common_dir).resolve()
    ):
        raise GitLinkError(f"{dotgit}: expected repository {Path(expected_common_dir).resolve()}")


def git_dirs_in_tree(worktree: str | Path) -> list[Path]:
    """The git dir / common dir a ``.git`` FILE points at, when they lie inside ``worktree``.

    Best effort for the sandbox (never raises): those directories are bind-mounted read-only on
    top of the writable work tree. The host refuses such a layout anyway (``validate``).
    """
    root = Path(worktree).resolve()
    dotgit = root / ".git"
    if dotgit.is_symlink() or not dotgit.is_file():
        return []
    try:
        target = read_pointer(root)
        found = [target]
        commondir_file = target / "commondir"
        if commondir_file.is_file():
            found.append(_resolve_rel(target, commondir_file.read_text(errors="replace").strip()))
    except (GitLinkError, OSError):
        return []
    inside: list[Path] = []
    for path in found:
        if path != root and path.is_relative_to(root) and path.is_dir() and path not in inside:
            inside.append(path)
    return inside
