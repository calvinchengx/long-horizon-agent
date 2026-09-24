"""``request_lease`` — lets a parallel implementer ask for a file outside its write-set.

The tool does not touch the workspace. It hands ``(path, reason)`` to a handler (in practice
``lha.coordination.leases.lease_handler``: the orchestrator's decision, committed to the mission
anchor) and returns the decision as the tool result. A granted file becomes writable through the
implementer's ownership guard at once; a refused one stays off limits.

``with_lease_tool`` wraps any ``ToolDispatcher`` so the tool is served next to the dispatcher's
own tools. Put it OUTSIDE the ``OwnershipGuard`` (the guard only checks the tools it wraps).
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from lha.contracts.model import ToolCall
from lha.contracts.state import EventRecord
from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult, ToolSpec
from lha.execution.dispatcher import validate_arguments

REQUEST_LEASE = "request_lease"

#: ``(path, reason) -> (granted, message)``.
LeaseHandler = Callable[[str, str], Awaitable[tuple[bool, str]]]

LEASE_SPEC = ToolSpec(
    name=REQUEST_LEASE,
    description=(
        "Ask for a lease on ONE file outside your write-set, when your item cannot be finished "
        "without changing it. The orchestrator grants it if nobody else is working on that file "
        "and refuses otherwise (another open item owns it, or it is a shared or harness file). "
        "Only write the file after a grant."
    ),
    parameters={
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "Repo-relative path of the file."},
            "reason": {"type": "string", "description": "Why your item needs this file."},
        },
        "required": ["path", "reason"],
        "additionalProperties": False,
    },
    # It changes durable mission state (the ownership map): read-only roles never get it.
    mutating=True,
)


class LeaseToolDispatcher:
    """A ``ToolDispatcher`` that serves ``request_lease`` itself and delegates the rest."""

    def __init__(self, inner: ToolDispatcher, handler: LeaseHandler) -> None:
        self._inner = inner
        self._handler = handler

    @property
    def inner(self) -> ToolDispatcher:
        return self._inner

    def specs(self) -> list[ToolSpec]:
        return [*self._inner.specs(), LEASE_SPEC]

    def drain_events(self) -> list[EventRecord]:
        """The wrapped dispatcher's gate events, so wrapping never hides the approval trail."""
        drain = getattr(self._inner, "drain_events", None)
        return list(drain()) if callable(drain) else []

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        if call.name != REQUEST_LEASE:
            return await self._inner.dispatch(call, ctx)
        error = validate_arguments(LEASE_SPEC.parameters, call.arguments)
        if error:
            return ToolResult.failure(f"invalid args for {REQUEST_LEASE!r}: {error}")
        path = str(call.arguments.get("path", "")).strip()
        reason = str(call.arguments.get("reason", "")).strip()
        if not path or not reason:
            return ToolResult.failure("request_lease needs a non-empty path and reason")
        try:
            granted, message = await self._handler(path, reason)
        except Exception as exc:  # the loop never crashes on a tool error
            return ToolResult.failure(f"lease request failed: {type(exc).__name__}: {exc}")
        return ToolResult.success(message) if granted else ToolResult.failure(message)


def with_lease_tool(dispatcher: ToolDispatcher, handler: LeaseHandler) -> ToolDispatcher:
    """``dispatcher`` plus ``request_lease`` bound to ``handler`` (unchanged if it has one)."""
    if any(spec.name == REQUEST_LEASE for spec in dispatcher.specs()):
        return dispatcher
    return LeaseToolDispatcher(dispatcher, handler)
