"""S2: workspace path containment for the local sandbox and the fs tools."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from lha.contracts.model import ToolCall
from lha.contracts.tools import ToolContext
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.paths import (
    PathEscapeError,
    contained_posix,
    is_protected,
    normalize_relpath,
    resolve_within,
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
