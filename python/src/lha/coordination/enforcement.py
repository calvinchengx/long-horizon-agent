"""Enforcing the ``FileOwnershipMap``: at the tool call, and again at the git layer.

Two layers, because neither alone is enough:

* ``OwnershipGuard`` wraps a ``ToolDispatcher``. A mutating tool that names workspace paths
  (``ToolSpec.path_args``, e.g. ``write_file``) is refused — before it runs — when the path is not
  writable by the agent's writer identities, with a message telling the model whose file it is and
  what to do instead. The shell tool names no paths, so it cannot be checked here.
* ``changed_paths`` + ``FileOwnershipMap.violations_any`` check what a writer actually changed on
  its branch (``git diff``), which catches writes made through the shell. The orchestrator refuses
  to merge a branch with violations.
"""

from __future__ import annotations

from collections.abc import Iterable
from pathlib import Path

from lha.contracts.model import ToolCall
from lha.contracts.state import EventRecord
from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult, ToolSpec
from lha.coordination.ownership import (
    LEAD,
    FileOwnershipMap,
    InvalidPathError,
    _normalize,
    is_shared,
)
from lha.state import git_ops

# Harness-owned paths are never part of a writer's change set (and never merged).
_EXCLUDED = (".lha",)


class OwnershipGuard:
    """A ``ToolDispatcher`` that refuses mutating path writes outside the writer's files.

    ``writers`` are the identities the agent acts as: an implementer is just its own id; the
    serial lead working an item is ``{"lead", <that item's implementer id>}`` (unassigned space
    plus the item's own leased files, but never another open item's files). Both the map and the
    identities can be swapped between cycles (``update``).
    """

    def __init__(
        self,
        inner: ToolDispatcher,
        ownership: FileOwnershipMap,
        writers: Iterable[str],
        *,
        lease_tool: bool = False,
    ) -> None:
        self._inner = inner
        self._ownership = ownership
        self._writers = tuple(writers)
        # The agent has the ``request_lease`` tool: say so in refusals.
        self._lease_tool = lease_tool

    @property
    def writers(self) -> tuple[str, ...]:
        return self._writers

    def update(self, *, ownership: FileOwnershipMap, writers: Iterable[str]) -> None:
        self._ownership = ownership
        self._writers = tuple(writers)

    def specs(self) -> list[ToolSpec]:
        return self._inner.specs()

    def drain_events(self) -> list[EventRecord]:
        """The wrapped dispatcher's gate events (``tool_approval`` etc.), so wrapping never hides
        the approval audit trail from the agent loop."""
        drain = getattr(self._inner, "drain_events", None)
        return list(drain()) if callable(drain) else []

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        spec = next((s for s in self._inner.specs() if s.name == call.name), None)
        if spec is not None and spec.mutating:
            for name in spec.path_args:
                value = call.arguments.get(name)
                if isinstance(value, str) and value:
                    refusal = self.refusal(value)
                    if refusal is not None:
                        return ToolResult.failure(refusal)
        return await self._inner.dispatch(call, ctx)

    def refusal(self, path: str) -> str | None:
        """Why the current writers may not write ``path`` (``None`` if they may)."""
        if self._ownership.permits_any(writers=self._writers, path=path):
            return None
        who = " / ".join(self._writers)
        try:
            norm = _normalize(path)
        except InvalidPathError as exc:
            return f"ownership: {who} may not write {path!r}: {exc}"
        if is_shared(norm):
            reason = "it is a shared file (build manifest, lockfile, package entry point, ...) "
            reason += "that only the lead writes"
        else:
            owner = self._ownership.owner_of(norm)
            if owner is None:
                reason = "it is outside your write-set (unassigned files belong to the lead)"
            else:
                reason = f"it is owned by {owner!r}"
        if self._lease_tool:
            advice = (
                "if you really need this file, call request_lease with the path and why; write "
                "it only if the lease is granted"
            )
        else:
            advice = (
                "if you really need this file, stop and say so in your summary (a lease request) "
                "instead of writing it"
            )
        return f"ownership: {who} may not write {norm!r}: {reason}. Write only the files you own; {advice}."


def changed_paths(workdir: str | Path, base: str, head: str) -> list[str]:
    """Repo-relative paths that differ between ``base`` and ``head`` (``.lha/`` excluded)."""
    if not base or not head or base == head:
        return []
    out = git_ops.run_git(
        workdir,
        "diff",
        "--name-only",
        "--no-renames",
        f"{base}..{head}",
        "--",
        ".",
        *(f":(exclude){p}" for p in _EXCLUDED),
    )
    return [line for line in out.splitlines() if line.strip()]


def effective_ownership(
    ownership: FileOwnershipMap, done_writers: Iterable[str]
) -> FileOwnershipMap:
    """``ownership`` with every finished writer's files released to the lead."""
    effective = ownership.model_copy(deep=True)
    for writer in done_writers:
        if writer != LEAD:
            effective.release(writer)
    return effective
