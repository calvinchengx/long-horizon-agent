"""Tests for failover, context compaction, and doc-schema migrators."""

from __future__ import annotations

import httpx
import pytest

from lha.agent.compaction import compact_messages
from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.model.failover import FailoverModel
from lha.model.stub import StubModel
from lha.state.migrations import migrate, register


class _AlwaysFails(ModelProvider):
    name = "fails"

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        # A transient outage (503) — the only kind of error failover should fall through on.
        request = httpx.Request("POST", "http://fails/")
        raise httpx.HTTPStatusError(
            "503", request=request, response=httpx.Response(503, request=request)
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


async def _no_sleep(_: float) -> None:
    return None


@pytest.mark.asyncio
async def test_failover_uses_backup() -> None:
    fm = FailoverModel(
        [_AlwaysFails(), StubModel(script=[TurnResult(text="backup ok")])], sleep=_no_sleep
    )
    result = await fm.complete([ModelMessage(role="user", content="hi")])
    assert result.text == "backup ok"


@pytest.mark.asyncio
async def test_failover_raises_when_all_fail() -> None:
    fm = FailoverModel([_AlwaysFails()], sleep=_no_sleep)
    with pytest.raises(httpx.HTTPStatusError):
        await fm.complete([ModelMessage(role="user", content="hi")])


@pytest.mark.asyncio
async def test_compaction_summarizes_old_turns() -> None:
    messages = [ModelMessage(role="system", content="sys")] + [
        ModelMessage(role="user", content=f"turn {i}") for i in range(10)
    ]
    model = StubModel(script=[TurnResult(text="SUMMARY")])
    out = await compact_messages(model=model, messages=messages, keep_last=2)
    assert out[0].role == "system"
    assert out[1].content == "turn 0"  # the original task message is kept verbatim
    assert "compacted summary" in out[2].content
    assert "SUMMARY" in out[2].content
    assert out[-1].content == "turn 9"
    assert len(out) == 1 + 1 + 1 + 2


@pytest.mark.asyncio
async def test_compaction_noop_when_short() -> None:
    messages = [ModelMessage(role="system", content="sys"), ModelMessage(role="user", content="a")]
    out = await compact_messages(model=StubModel(), messages=messages, keep_last=4)
    assert out == messages


def test_doc_schema_migrators() -> None:
    @register("test_artifact", 1)
    def _v1_to_v2(data: dict[str, object]) -> dict[str, object]:
        updated = dict(data)
        updated["schema_version"] = 2
        updated["new_field"] = True
        return updated

    out = migrate("test_artifact", {"schema_version": 1}, target_version=2)
    assert out["schema_version"] == 2
    assert out["new_field"] is True


@pytest.mark.asyncio
async def test_compaction_summarizes_newest_older_turns_and_keeps_pairs() -> None:
    class _Capture(StubModel):
        seen: str = ""

        async def complete(
            self,
            messages: list[ModelMessage],
            *,
            tools: list[dict[str, object]] | None = None,
            max_tokens: int | None = None,
        ) -> TurnResult:
            _Capture.seen = messages[-1].content
            return TurnResult(text="S")

    body: list[ModelMessage] = [ModelMessage(role="user", content="TASK")]
    for i in range(40):
        body.append(ModelMessage(role="assistant", content=f"action {i} " + "x" * 600))
        body.append(ModelMessage(role="user", content=f"OBSERVATION (t): result {i}"))
    out = await compact_messages(model=_Capture(), messages=body, keep_last=3)
    assert "result 36" in _Capture.seen  # the NEWEST summarized turns reach the summarizer
    assert out[0].content == "TASK"
    assert out[2].role == "assistant"  # kept tail starts with an action, not an orphan result
