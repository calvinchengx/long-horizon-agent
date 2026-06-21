"""Workspace path containment — the one place every sandbox and fs tool resolves agent paths.

Agent-supplied paths are untrusted. Every path the model names must stay inside the session's
workspace root, so this module:

- rejects empty-anchor tricks: absolute paths, drive letters, NUL bytes;
- rejects lexical escapes (``../x``, ``a/../../x``) before touching the filesystem;
- resolves symlinks on the host (``Path.resolve``) and re-checks containment, so a symlink planted
  in the workspace cannot point reads/writes outside it;
- re-checks the opened file descriptor against the path after open (``O_NOFOLLOW`` + inode
  comparison), narrowing the resolve→open race window where the OS allows it.

``normalize_relpath`` is purely lexical (usable for container/VM paths the host cannot resolve);
``resolve_within`` / ``read_text_within`` / ``write_text_within`` are for host-local workspaces.
"""

from __future__ import annotations

import errno
import os
import posixpath
import stat
from pathlib import Path, PurePosixPath, PureWindowsPath

# Top-level workspace dirs owned by the harness: the agent must never mutate them directly.
PROTECTED_DIRS = frozenset({".lha", ".git"})

_O_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
_O_BINARY = getattr(os, "O_BINARY", 0)
# Opening a FIFO blocks until a peer appears; O_NONBLOCK makes the open return immediately so the
# regular-file check in ``_verify_fd`` can reject it (blocking mode is restored before real IO).
_O_NONBLOCK = getattr(os, "O_NONBLOCK", 0)


class PathEscapeError(PermissionError):
    """Raised when an agent-supplied path would leave the workspace root.

    Subclasses ``PermissionError`` (hence ``OSError``) so existing ``except OSError`` handlers in
    tools turn it into a normal tool failure.
    """


def normalize_relpath(relpath: str) -> PurePosixPath:
    """Lexically validate ``relpath`` and return it normalized (``.`` for the root itself).

    Raises ``PathEscapeError`` for absolute paths, drive-qualified paths, NUL bytes, or any path
    whose normalized form climbs above the root.
    """
    if not isinstance(relpath, str):
        raise PathEscapeError(f"path must be a string, got {type(relpath).__name__}")
    if "\x00" in relpath:
        raise PathEscapeError("path contains a NUL byte")
    unified = relpath.replace("\\", "/")
    win = PureWindowsPath(relpath)
    if unified.startswith("/") or win.drive or win.anchor:
        raise PathEscapeError(f"absolute paths are not allowed: {relpath!r}")
    normalized = posixpath.normpath(unified) if unified else "."
    if normalized == ".." or normalized.startswith("../"):
        raise PathEscapeError(f"path escapes the workspace: {relpath!r}")
    return PurePosixPath(normalized)


def is_protected(relpath: str) -> bool:
    """True if ``relpath`` (lexically) targets a harness-owned dir such as ``.lha/`` or ``.git/``.

    Case-insensitive, because the default macOS/Windows filesystems are.
    """
    parts = normalize_relpath(relpath).parts
    return bool(parts) and parts[0].casefold() in PROTECTED_DIRS


def contained_posix(workdir: str, relpath: str) -> str:
    """Join a validated ``relpath`` onto a container/VM ``workdir`` (lexical containment only)."""
    rel = normalize_relpath(relpath)
    return str(PurePosixPath(workdir) / rel) if str(rel) != "." else workdir


def resolve_within(root: str | os.PathLike[str], relpath: str) -> Path:
    """Resolve ``relpath`` under ``root`` following symlinks; raise if the result escapes."""
    rel = normalize_relpath(relpath)
    root_resolved = Path(root).resolve()
    candidate = (root_resolved / rel).resolve()
    if not candidate.is_relative_to(root_resolved):
        raise PathEscapeError(f"path escapes the workspace: {relpath!r}")
    return candidate


def is_protected_resolved(root: str | os.PathLike[str], relpath: str) -> bool:
    """Like ``is_protected`` but after symlink resolution (``link -> .git`` is also protected)."""
    if is_protected(relpath):
        return True
    root_resolved = Path(root).resolve()
    parts = resolve_within(root, relpath).relative_to(root_resolved).parts
    return bool(parts) and parts[0].casefold() in PROTECTED_DIRS


def _verify_fd(root: Path, relpath: str, fd: int, expected: Path) -> None:
    """After open: the fd must be a regular file that is still the contained path we resolved."""
    st = os.fstat(fd)
    if not stat.S_ISREG(st.st_mode):
        raise IsADirectoryError(f"not a regular file: {relpath!r}")
    again = resolve_within(root, relpath)
    try:
        current = os.stat(again)
    except OSError as exc:
        raise PathEscapeError(f"path changed during open: {relpath!r}") from exc
    if again != expected or (current.st_dev, current.st_ino) != (st.st_dev, st.st_ino):
        raise PathEscapeError(f"path changed during open: {relpath!r}")
    if _O_NONBLOCK:
        os.set_blocking(fd, True)


def read_text_within(root: str | os.PathLike[str], relpath: str) -> str:
    """Read a UTF-8 file at ``relpath`` inside ``root`` (containment checked before and after)."""
    target = resolve_within(root, relpath)
    fd = os.open(target, os.O_RDONLY | _O_NOFOLLOW | _O_BINARY | _O_NONBLOCK)
    with os.fdopen(fd, "rb") as handle:
        _verify_fd(Path(root).resolve(), relpath, handle.fileno(), target)
        return handle.read().decode("utf-8")


def write_text_within(root: str | os.PathLike[str], relpath: str, content: str) -> Path:
    """Write UTF-8 ``content`` to ``relpath`` inside ``root``, creating contained parents."""
    root_resolved = Path(root).resolve()
    target = resolve_within(root, relpath)
    if target == root_resolved:
        raise IsADirectoryError(f"cannot write to the workspace root: {relpath!r}")
    target.parent.mkdir(parents=True, exist_ok=True)
    # Re-resolve after mkdir: a racing symlink swap of a parent must not redirect the write.
    target = resolve_within(root, relpath)
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | _O_NOFOLLOW | _O_BINARY | _O_NONBLOCK
    try:
        fd = os.open(target, flags, 0o644)
    except OSError as exc:
        if exc.errno == errno.ENXIO:  # a FIFO with no reader (O_NONBLOCK): not a regular file
            raise IsADirectoryError(f"not a regular file: {relpath!r}") from exc
        raise
    with os.fdopen(fd, "wb") as handle:
        _verify_fd(root_resolved, relpath, handle.fileno(), target)
        handle.write(content.encode("utf-8"))
    return target
