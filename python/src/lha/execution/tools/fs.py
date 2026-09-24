"""Filesystem tools: read / write / list / grep, scoped to the sandbox workspace.

All paths are confined to the workspace (``lha.execution.paths``): absolute paths, ``..`` and
symlinks pointing outside are rejected; walks skip symlinked entries that resolve outside.

``grep`` treats ``pattern`` as a LITERAL substring by default. ``regex: true`` opts into Python
regular expressions, with ReDoS guards: patterns are length-capped and rejected if they contain
backreferences or a quantified group that itself contains a quantifier (``(a+)+``, ``(a|a)*``
-style nesting, the classic catastrophic-backtracking shapes); lines are matched only up to
``_MAX_LINE`` characters and files larger than ``_MAX_FILE_BYTES`` are skipped.
"""

from __future__ import annotations

import asyncio
import re
from pathlib import Path

from lha.contracts.sandbox import host_root
from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.execution.paths import resolve_within
from lha.execution.tools.limits import MAX_TOOL_OUTPUT

_IGNORE_PARTS = {".git", "__pycache__", ".venv", "node_modules", ".mypy_cache", ".ruff_cache"}
_MAX_PATTERN = 256
_MAX_LINE = 2_000
_MAX_FILE_BYTES = 2_000_000
_MAX_FILES = 2_000
_MAX_HITS = 500
# A group containing a quantifier, itself followed by a quantifier: (x+)+, (x*)*, (x+){2,}, ...
_NESTED_QUANTIFIER = re.compile(r"\((?:[^()\\]|\\.)*[+*}](?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d)")
_ALTERNATION_QUANTIFIED = re.compile(r"\((?:[^()\\]|\\.)*\|(?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d)")
_BACKREFERENCE = re.compile(r"\\[1-9]|\(\?P=")


def _ignored(path: Path) -> bool:
    return any(part in _IGNORE_PARTS for part in path.parts)


def _clip(text: str) -> str:
    return text if len(text) <= MAX_TOOL_OUTPUT else text[:MAX_TOOL_OUTPUT] + "\n…[truncated]"


def _link_inside(link: Path, root: Path) -> bool:
    """True if symlink ``link`` resolves inside ``root``; a symlink loop counts as outside."""
    try:
        return link.resolve().is_relative_to(root)
    except RuntimeError:
        return False


def _contained_files(root: Path, start: Path) -> list[Path]:
    """Regular files under ``start`` whose real path stays inside ``root`` (sorted, capped)."""
    found: list[Path] = []
    for path in sorted(start.rglob("*")):
        rel = path.relative_to(root)
        if _ignored(rel) or (path.is_symlink() and not _link_inside(path, root)):
            continue
        if path.is_file():
            found.append(path)
            if len(found) >= _MAX_FILES:
                break
    return found


def compile_search_pattern(pattern: str, *, regex: bool) -> re.Pattern[str]:
    """Compile a grep pattern: literal by default, guarded regex when ``regex`` is True.

    Raises ``ValueError`` for over-long or backtracking-prone patterns (and bad regex syntax).
    """
    if not pattern:
        raise ValueError("pattern must not be empty")
    if len(pattern) > _MAX_PATTERN:
        raise ValueError(f"pattern longer than {_MAX_PATTERN} characters")
    if not regex:
        return re.compile(re.escape(pattern))
    if _BACKREFERENCE.search(pattern):
        raise ValueError("backreferences are not allowed")
    if _NESTED_QUANTIFIER.search(pattern) or _ALTERNATION_QUANTIFIED.search(pattern):
        raise ValueError("nested/alternated quantified groups are not allowed (ReDoS risk)")
    try:
        return re.compile(pattern)
    except re.error as exc:
        raise ValueError(f"bad regex: {exc}") from exc


class ReadFileTool:
    spec = ToolSpec(
        name="read_file",
        description="Read a UTF-8 text file at a path relative to the workspace root.",
        parameters={
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
        },
        path_args=["path"],
    )

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        path = str(arguments.get("path", ""))
        try:
            content = await ctx.session.read_file(path)
        except (OSError, UnicodeDecodeError) as exc:
            return ToolResult.failure(f"cannot read {path!r}: {exc}")
        return ToolResult.success(_clip(content))


class WriteFileTool:
    spec = ToolSpec(
        name="write_file",
        description="Create or overwrite a UTF-8 text file (path relative to the workspace root).",
        parameters={
            "type": "object",
            "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
            "required": ["path", "content"],
        },
        mutating=True,
        path_args=["path"],
    )

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        path = str(arguments.get("path", ""))
        content = arguments.get("content", "")
        if not isinstance(content, str):
            return ToolResult.failure("'content' must be a string")
        try:
            await ctx.session.write_file(path, content)
        except OSError as exc:
            return ToolResult.failure(f"cannot write {path!r}: {exc}")
        return ToolResult.success(f"wrote {len(content)} bytes to {path}")


class ListFilesTool:
    spec = ToolSpec(
        name="list_files",
        description="List files under the workspace (optionally a subdirectory), ignoring noise.",
        parameters={
            "type": "object",
            "properties": {"subdir": {"type": "string"}},
        },
        path_args=["subdir"],
    )

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        subdir = str(arguments.get("subdir", "") or "")

        def _walk() -> list[str]:
            root = Path(host_root(ctx.session)).resolve()
            start = resolve_within(root, subdir)
            return [str(p.relative_to(root)) for p in _contained_files(root, start)]

        try:
            files = await asyncio.to_thread(_walk)
        except OSError as exc:
            return ToolResult.failure(str(exc))
        return ToolResult.success(_clip("\n".join(files)) if files else "(no files)")


class GrepTool:
    spec = ToolSpec(
        name="grep",
        description=(
            "Search file contents; returns path:line: text matches. 'pattern' is a literal "
            "string unless 'regex' is true (simple regular expressions only)."
        ),
        parameters={
            "type": "object",
            "properties": {
                "pattern": {"type": "string"},
                "subdir": {"type": "string"},
                "regex": {"type": "boolean"},
            },
            "required": ["pattern"],
        },
        path_args=["subdir"],
    )

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        pattern = str(arguments.get("pattern", ""))
        subdir = str(arguments.get("subdir", "") or "")
        try:
            matcher = compile_search_pattern(pattern, regex=arguments.get("regex") is True)
        except ValueError as exc:
            return ToolResult.failure(str(exc))

        def _search() -> list[str]:
            root = Path(host_root(ctx.session)).resolve()
            start = resolve_within(root, subdir)
            hits: list[str] = []
            for path in _contained_files(root, start):
                try:
                    if path.stat().st_size > _MAX_FILE_BYTES:
                        continue
                    text = path.read_text(encoding="utf-8")
                except (OSError, UnicodeDecodeError):
                    continue
                for lineno, line in enumerate(text.splitlines(), start=1):
                    if matcher.search(line[:_MAX_LINE]):
                        rel = path.relative_to(root)
                        hits.append(f"{rel}:{lineno}: {line.strip()[:200]}")
                        if len(hits) >= _MAX_HITS:
                            return hits
            return hits

        try:
            hits = await asyncio.to_thread(_search)
        except OSError as exc:
            return ToolResult.failure(str(exc))
        return ToolResult.success(_clip("\n".join(hits)) if hits else "(no matches)")
