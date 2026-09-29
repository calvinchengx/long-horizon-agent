"""S2: workspace path containment for the local sandbox and the fs tools."""

from __future__ import annotations

import errno
import os
import threading
from collections.abc import Callable
from pathlib import Path

import pytest

from lha.contracts.model import ToolCall
from lha.contracts.tools import ToolContext
from lha.execution import paths
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.paths import (
    PathEscapeError,
    contained_posix,
    is_protected,
    is_protected_resolved,
    normalize_relpath,
    read_text_within,
    resolve_within,
    write_text_within,
)
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools

symlinks = pytest.mark.skipif(os.name == "nt", reason="symlinks need privileges on Windows")


@pytest.mark.parametrize(
    "bad", ["../x", "a/../../x", "/etc/passwd", "C:\\x", "\\\\srv\\x", "a\x00b"]
)
def test_normalize_rejects_escapes(bad: str) -> None:
    with pytest.raises(PathEscapeError):
        normalize_relpath(bad)


def test_normalize_accepts_inner_paths() -> None:
    assert str(normalize_relpath("a/./b/../c.txt")) == "a/c.txt"
    assert str(normalize_relpath("")) == "."
    assert contained_posix("/workspace", "src/x.py") == "/workspace/src/x.py"
    with pytest.raises(PathEscapeError):
        contained_posix("/workspace", "../etc/passwd")


def test_is_protected() -> None:
    assert is_protected(".lha/checklist.json")
    assert is_protected("./.git/hooks/pre-commit")
    assert is_protected(".GIT/config")
    assert not is_protected("src/.git_notes")


@pytest.mark.asyncio
async def test_local_session_rejects_traversal(tmp_path: Path) -> None:
    work = tmp_path / "work"
    session = await LocalSandbox().open(workdir=str(work))
    (tmp_path / "secret.txt").write_text("s3cret", encoding="utf-8")
    with pytest.raises(PathEscapeError):
        await session.read_file("../secret.txt")
    with pytest.raises(PathEscapeError):
        await session.read_file(str(tmp_path / "secret.txt"))
    with pytest.raises(PathEscapeError):
        await session.write_file("../pwned.txt", "x")
    assert not (tmp_path / "pwned.txt").exists()
    with pytest.raises(PathEscapeError):
        await session.exec(["true"], cwd="..")


@symlinks
@pytest.mark.asyncio
async def test_local_session_rejects_symlink_escape(tmp_path: Path) -> None:
    work = tmp_path / "work"
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "secret.txt").write_text("s3cret", encoding="utf-8")
    session = await LocalSandbox().open(workdir=str(work))
    (work / "link").symlink_to(outside, target_is_directory=True)
    (work / "file_link").symlink_to(outside / "secret.txt")

    with pytest.raises(PathEscapeError):
        await session.read_file("link/secret.txt")
    with pytest.raises(PathEscapeError):
        await session.read_file("file_link")
    with pytest.raises(PathEscapeError):
        await session.write_file("link/new.txt", "x")
    assert not (outside / "new.txt").exists()


@symlinks
def test_resolve_within_allows_inner_symlink(tmp_path: Path) -> None:
    (tmp_path / "real").mkdir()
    (tmp_path / "alias").symlink_to(tmp_path / "real", target_is_directory=True)
    assert resolve_within(tmp_path, "alias/x") == (tmp_path / "real" / "x").resolve()


@symlinks
@pytest.mark.asyncio
async def test_fs_tools_do_not_leak_outside_via_symlinks(tmp_path: Path) -> None:
    work = tmp_path / "work"
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "secret.txt").write_text("TOKEN=abc", encoding="utf-8")
    session = await LocalSandbox().open(workdir=str(work))
    (work / "ok.txt").write_text("TOKEN=inside", encoding="utf-8")
    (work / "leak").symlink_to(outside, target_is_directory=True)
    (work / "leak_file").symlink_to(outside / "secret.txt")
    ctx = ToolContext(mission_id="m", session=session)
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)

    listing = await dispatcher.dispatch(ToolCall(id="1", name="list_files", arguments={}), ctx)
    assert listing.ok and "secret" not in listing.content and "leak_file" not in listing.content

    grep = await dispatcher.dispatch(
        ToolCall(id="2", name="grep", arguments={"pattern": "TOKEN"}), ctx
    )
    assert grep.ok and "abc" not in grep.content and "inside" in grep.content

    sub = await dispatcher.dispatch(
        ToolCall(id="3", name="grep", arguments={"pattern": "TOKEN", "subdir": "leak"}), ctx
    )
    assert not sub.ok


@pytest.mark.asyncio
async def test_fs_tools_reject_parent_paths(tmp_path: Path) -> None:
    session = await LocalSandbox().open(workdir=str(tmp_path / "work"))
    ctx = ToolContext(mission_id="m", session=session)
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    for call in (
        ToolCall(id="1", name="read_file", arguments={"path": "../x"}),
        ToolCall(id="2", name="write_file", arguments={"path": "/tmp/x", "content": "y"}),
        ToolCall(id="3", name="list_files", arguments={"subdir": "../"}),
        ToolCall(id="4", name="grep", arguments={"pattern": "a", "subdir": ".."}),
    ):
        result = await dispatcher.dispatch(call, ctx)
        assert not result.ok, call


@symlinks
@pytest.mark.asyncio
async def test_symlink_loop_is_a_tool_failure_not_a_crash(tmp_path: Path) -> None:
    work = tmp_path / "work"
    session = await LocalSandbox().open(workdir=str(work))
    (work / "ok.txt").write_text("TOKEN=inside", encoding="utf-8")
    (work / "loop1").symlink_to("loop2")
    (work / "loop2").symlink_to("loop1")
    ctx = ToolContext(mission_id="m", session=session)
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)

    with pytest.raises(PathEscapeError, match=r"^Symlink loop from "):
        resolve_within(work, "loop1")

    for call in (
        ToolCall(id="1", name="read_file", arguments={"path": "loop1"}),
        ToolCall(id="2", name="list_files", arguments={"subdir": "loop1"}),
        ToolCall(id="3", name="grep", arguments={"pattern": "TOKEN", "subdir": "loop1"}),
    ):
        result = await dispatcher.dispatch(call, ctx)
        assert not result.ok, call
        assert (result.error or "").startswith(
            f"path not allowed for {call.name!r}: Symlink loop from "
        ), result.error

    # A loop elsewhere in the tree is skipped, not fatal, when walking the whole workspace.
    listing = await dispatcher.dispatch(ToolCall(id="4", name="list_files", arguments={}), ctx)
    assert listing.ok and "ok.txt" in listing.content and "loop1" not in listing.content
    grep = await dispatcher.dispatch(
        ToolCall(id="5", name="grep", arguments={"pattern": "TOKEN"}), ctx
    )
    assert grep.ok and "inside" in grep.content


# --- Rules pinned by the mutation audit (docs/20-testing.md#mutation-audit) ---------------------


def _reason(exc: pytest.ExceptionInfo[BaseException]) -> str:
    return str(exc.value)


def test_non_string_path_is_rejected_with_its_type() -> None:
    with pytest.raises(PathEscapeError) as exc:
        normalize_relpath(42)  # type: ignore[arg-type]
    assert _reason(exc) == "path must be a string, got int"


@symlinks
def test_symlink_escape_reason(tmp_path: Path) -> None:
    root = tmp_path / "ws"
    root.mkdir()
    (root / "out").symlink_to(tmp_path)
    with pytest.raises(PathEscapeError) as exc:
        resolve_within(root, "out/x")
    assert _reason(exc) == "path escapes the workspace: 'out/x'"


# Harness-owned dirs stay protected lexically and through symlinks, and only at the top level.
@symlinks
def test_is_protected_resolved(tmp_path: Path) -> None:
    (tmp_path / ".git").mkdir()
    (tmp_path / "src").mkdir()
    (tmp_path / "meta").symlink_to(tmp_path / ".git")
    assert is_protected_resolved(tmp_path, ".git/config")
    assert is_protected_resolved(tmp_path, "meta")
    assert is_protected_resolved(tmp_path, "meta/config")
    assert not is_protected_resolved(tmp_path, "src/a.py")
    assert not is_protected_resolved(tmp_path, "src/.git")
    assert not is_protected_resolved(tmp_path, ".")


fifos = pytest.mark.skipif(not hasattr(os, "mkfifo"), reason="needs POSIX FIFOs")


def _fifo_error(pipe: Path, op: Callable[[], object], peer_flags: int) -> BaseException:
    """Run ``op`` on a FIFO; fail (instead of hanging) if its open blocks waiting for a peer."""
    raised: list[BaseException] = []

    def run() -> None:
        try:
            op()
        except BaseException as exc:
            raised.append(exc)

    worker = threading.Thread(target=run, daemon=True)
    worker.start()
    worker.join(2)
    if worker.is_alive():  # blocked in open(): connect a peer to release it, then fail
        os.close(os.open(pipe, peer_flags | os.O_NONBLOCK))
        worker.join(2)
        pytest.fail("opening a FIFO blocked")
    assert len(raised) == 1
    return raised[0]


# A FIFO is not a regular file: reading or writing one is refused instead of blocking.
@fifos
def test_reading_and_writing_a_fifo_is_refused(tmp_path: Path) -> None:
    pipe = tmp_path / "pipe"
    os.mkfifo(pipe)
    ops: list[tuple[Callable[[], object], int]] = [
        (lambda: read_text_within(tmp_path, "pipe"), os.O_WRONLY),
        (lambda: write_text_within(tmp_path, "pipe", "x"), os.O_RDONLY),
    ]
    for op, peer in ops:
        exc = _fifo_error(pipe, op, peer)
        assert isinstance(exc, IsADirectoryError)
        assert str(exc) == "not a regular file: 'pipe'"


def test_writing_the_root_is_refused(tmp_path: Path) -> None:
    with pytest.raises(IsADirectoryError) as exc:
        write_text_within(tmp_path, ".", "x")
    assert _reason(exc) == "cannot write to the workspace root: '.'"


# Writes create missing parents, reuse existing ones, truncate, and use mode 0o644 (less umask).
def test_write_creates_parents_truncates_and_sets_mode(tmp_path: Path) -> None:
    old = os.umask(0o022)
    try:
        write_text_within(tmp_path, "a/b/c.txt", "long content")
        write_text_within(tmp_path, "a/b/c.txt", "é")
        write_text_within(tmp_path, "a/b/d.txt", "d")
    finally:
        os.umask(old)
    assert read_text_within(tmp_path, "a/b/c.txt") == "é"
    assert (tmp_path / "a/b/c.txt").read_bytes() == "é".encode()
    assert (tmp_path / "a/b/d.txt").stat().st_mode & 0o777 == 0o644


# A symlink swapped in after resolution (the resolve->open race) is not followed: O_NOFOLLOW.
@symlinks
def test_symlink_swapped_in_after_resolve_is_not_followed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    root = tmp_path / "ws"
    root.mkdir()
    outside = tmp_path / "secret.txt"
    outside.write_text("secret")
    link = root / "f.txt"
    link.symlink_to(outside)
    monkeypatch.setattr(paths, "resolve_within", lambda _root, _rel: link)
    with pytest.raises(OSError) as read_exc:
        read_text_within(root, "f.txt")
    assert read_exc.value.errno == errno.ELOOP
    with pytest.raises(OSError) as write_exc:
        write_text_within(root, "f.txt", "pwned")
    assert write_exc.value.errno == errno.ELOOP
    assert outside.read_text() == "secret"


# After open the fd must still be the resolved path: a vanished or swapped inode is refused.
def test_file_replaced_during_open_is_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    (tmp_path / "f.txt").write_text("f")
    (tmp_path / "other.txt").write_text("o")
    real_stat = os.stat
    target = resolve_within(tmp_path, "f.txt")

    def swapped(path: object, *args: object, **kwargs: object) -> os.stat_result:
        if path == target:
            return real_stat(tmp_path / "other.txt")
        return real_stat(path, *args, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(paths.os, "stat", swapped)
    with pytest.raises(PathEscapeError) as exc:
        read_text_within(tmp_path, "f.txt")
    assert _reason(exc) == "path changed during open: 'f.txt'"

    def vanished(path: object, *args: object, **kwargs: object) -> os.stat_result:
        if path == target:
            raise FileNotFoundError(path)
        return real_stat(path, *args, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(paths.os, "stat", vanished)
    with pytest.raises(PathEscapeError) as exc:
        read_text_within(tmp_path, "f.txt")
    assert _reason(exc) == "path changed during open: 'f.txt'"


# The fd is opened O_NONBLOCK (so FIFOs cannot hang the open) and set back to blocking once
# verified as a regular file, before any real IO.
def test_verified_fd_is_back_in_blocking_mode(tmp_path: Path) -> None:
    (tmp_path / "f.txt").write_text("f")
    target = resolve_within(tmp_path, "f.txt")
    fd = os.open(target, os.O_RDONLY | os.O_NONBLOCK)
    try:
        assert not os.get_blocking(fd)
        paths._verify_fd(tmp_path.resolve(), "f.txt", fd, target)
        assert os.get_blocking(fd)
    finally:
        os.close(fd)
