"""Dispatcher wrappers must pass the gate's approval events through to the agent loop."""

from __future__ import annotations

from lha.contracts.model import ToolCall
from lha.contracts.state import EventRecord
from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.coordination.enforcement import OwnershipGuard
from lha.coordination.ownership import LEAD, FileOwnershipMap
from lha.execution.tools.decisions import DecisionToolDispatcher


class _Inner:
    def __init__(self) -> None:
        self.events = [EventRecord(kind="tool_approval", cycle_id="c1", payload={"x": 1})]

    def specs(self) -> list[ToolSpec]:
        return []

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        return ToolResult.success("ok")

    def drain_events(self) -> list[EventRecord]:
        out, self.events = self.events, []
        return out


class _Sink:
    def record_decision(self, record: object) -> int:  # pragma: no cover - not called
        return 0


def test_wrappers_forward_gate_events() -> None:
    inner = _Inner()
    guard = OwnershipGuard(inner, FileOwnershipMap(), writers=(LEAD,))
    wrapped = DecisionToolDispatcher(guard, _Sink())  # type: ignore[arg-type]
    assert [e.kind for e in wrapped.drain_events()] == ["tool_approval"]
    assert wrapped.drain_events() == []


def test_wrappers_without_inner_events() -> None:
    class Bare(_Inner):
        drain_events = None  # type: ignore[assignment]

    assert OwnershipGuard(Bare(), FileOwnershipMap(), writers=(LEAD,)).drain_events() == []


async def test_tampered_decision_chain_is_non_retryable() -> None:
    import pytest
    from temporalio.exceptions import ApplicationError

    from lha.coordination.decision_log import DecisionChainError
    from lha.durable.activities import _refuse_tampered_chain
    from lha.durable.types import ERROR_CONFIG

    async def cycle() -> None:
        raise DecisionChainError("line 3: hash mismatch")

    with pytest.raises(ApplicationError) as info:
        await _refuse_tampered_chain(cycle())  # type: ignore[arg-type]
    assert info.value.type == ERROR_CONFIG and info.value.non_retryable
    assert "decision log failed verification" in str(info.value)
