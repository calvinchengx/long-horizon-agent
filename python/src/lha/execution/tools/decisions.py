"""``record_decision`` — lets the agent write a design decision to the never-compacted log.

The tool does not touch the workspace. It hands a ``DecisionRecord`` to a ``DecisionSink``:
``GitMissionAnchor`` (queued in memory, chained onto ``.lha/decisions.ndjson`` by the cycle's
checkpoint commit) or a ``DecisionBuffer`` (an implementer's decisions, committed by the
integrator only if its work is merged — decisions travel with the code they describe).

``with_decision_tool`` wraps any ``ToolDispatcher`` so the tool is offered and served next to
the dispatcher's own tools. ``lha.agent.assembly.build_lead_loop`` applies it with the mission
anchor, and every run path (``run-local``/``mission``, ``orchestrate``, the Temporal cycle
activity) builds its lead there, so every lead has the tool.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from lha.contracts.model import ToolCall
from lha.contracts.state import DecisionRecord, EventRecord
from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult, ToolSpec
from lha.execution.dispatcher import validate_arguments

RECORD_DECISION = "record_decision"

_MAX_DECISION = 500
_MAX_TEXT = 2_000
_MAX_AFFECTED = 50
_MAX_PATH = 300


@runtime_checkable
class DecisionSink(Protocol):
    """Where recorded decisions go until they are committed."""

    def record_decision(self, record: DecisionRecord) -> int:
        """Queue ``record``; return how many decisions are now queued."""
        ...


class DecisionBuffer:
    """An in-memory ``DecisionSink`` (e.g. one parallel implementer's decisions)."""

    def __init__(self) -> None:
        self.records: list[DecisionRecord] = []

    def record_decision(self, record: DecisionRecord) -> int:
        self.records.append(record)
        return len(self.records)


class RecordDecisionTool:
    """Record one design decision (committed with the current cycle's checkpoint)."""

    spec = ToolSpec(
        name=RECORD_DECISION,
        description=(
            "Record a design decision that later work must stay consistent with (an interface, "
            "data format, library choice, naming rule...). It is committed with this cycle's "
            "checkpoint to the mission's append-only, hash-chained decision log and shown to "
            "every later cycle. Record the decision, why, what you rejected, and affected files."
        ),
        parameters={
            "type": "object",
            "properties": {
                "decision": {"type": "string"},
                "rationale": {"type": "string"},
                "alternatives_rejected": {"type": "string"},
                "affected": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["decision", "rationale"],
            "additionalProperties": False,
        },
        # It writes durable mission state (not the workspace): read-only roles never get it.
        mutating=True,
    )

    def __init__(self, sink: DecisionSink) -> None:
        self._sink = sink

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        decision = str(arguments.get("decision", "")).strip()
        rationale = str(arguments.get("rationale", "")).strip()
        if not decision or not rationale:
            return ToolResult.failure("record_decision needs a non-empty decision and rationale")
        if len(decision) > _MAX_DECISION:
            return ToolResult.failure(
                f"decision is too long ({len(decision)} > {_MAX_DECISION} chars); state it "
                "briefly and put the detail in the rationale"
            )
        raw_affected = arguments.get("affected", [])
        affected = (
            [str(a).strip()[:_MAX_PATH] for a in raw_affected if str(a).strip()]
            if isinstance(raw_affected, list)
            else []
        )
        record = DecisionRecord(
            decision=decision,
            rationale=rationale[:_MAX_TEXT],
            alternatives_rejected=str(arguments.get("alternatives_rejected", "")).strip()[
                :_MAX_TEXT
            ],
            affected=affected[:_MAX_AFFECTED],
        )
        queued = self._sink.record_decision(record)
        return ToolResult.success(
            f"recorded decision ({queued} this cycle); it is committed with this cycle's checkpoint"
        )


class DecisionToolDispatcher:
    """A ``ToolDispatcher`` that serves ``record_decision`` itself and delegates the rest."""

    def __init__(self, inner: ToolDispatcher, sink: DecisionSink) -> None:
        self._inner = inner
        self._tool = RecordDecisionTool(sink)

    @property
    def inner(self) -> ToolDispatcher:
        return self._inner

    def specs(self) -> list[ToolSpec]:
        return [*self._inner.specs(), self._tool.spec]

    def drain_events(self) -> list[EventRecord]:
        """The wrapped dispatcher's gate events (``tool_approval`` etc.), so wrapping never hides
        the approval audit trail from the agent loop."""
        drain = getattr(self._inner, "drain_events", None)
        return list(drain()) if callable(drain) else []

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        if call.name != RECORD_DECISION:
            return await self._inner.dispatch(call, ctx)
        error = validate_arguments(self._tool.spec.parameters, call.arguments)
        if error:
            return ToolResult.failure(f"invalid args for {RECORD_DECISION!r}: {error}")
        try:
            return await self._tool.run(call.arguments, ctx)
        except Exception as exc:  # the loop never crashes on a tool error
            return ToolResult.failure(f"{type(exc).__name__}: {exc}")


def with_decision_tool(dispatcher: ToolDispatcher, sink: DecisionSink) -> ToolDispatcher:
    """``dispatcher`` plus ``record_decision`` bound to ``sink`` (unchanged if it has one)."""
    if any(spec.name == RECORD_DECISION for spec in dispatcher.specs()):
        return dispatcher
    return DecisionToolDispatcher(dispatcher, sink)
