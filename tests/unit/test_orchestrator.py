"""Integration tests for the multi-agent orchestrator (full org flow, offline stubs)."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from lha.agents.orchestrator import Orchestrator
from lha.config import Settings
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.model.stub import StubModel
from lha.state import git_ops

_DONE = TurnResult(text='{"done": true, "summary": "did it"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
_BLOCK = TurnResult(
    text='{"done": true, "verdict": "block", "blocking_issues": ["no error handling"]}'
)
_PASSING_CHECK = Check(name="always_green", command=[sys.executable, "-c", "pass"])


def _checklist() -> Checklist:
    return Checklist(
        items=[
            ChecklistItem(id="01", description="do thing one"),
            ChecklistItem(id="02", description="do thing two"),
        ]
    )


def _stub(*script: TurnResult) -> StubModel:
    return StubModel(script=list(script))


@pytest.mark.asyncio
async def test_orchestrator_completes_mission_with_org(tmp_path: Path) -> None:
    settings = Settings(
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=100.0,
        max_cycles=20,
    )
    orchestrator = Orchestrator(
        settings,
        research_per_item=1,
        do_review=True,
        models={"lead": _stub(_DONE), "researcher": _stub(_DONE), "reviewer": _stub(_APPROVE)},
    )

    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Org test",
        description="exercise the org",
        checklist=_checklist(),
        checks=[_PASSING_CHECK],
    )

    assert summary.items_total == 2
    assert summary.items_done == 2
    assert summary.completed
    assert summary.stopped_reason == "complete"
    completed = sum(1 for line in git_ops.log_oneline(tmp_path, 100) if "lha: complete" in line)
    assert completed == 2


@pytest.mark.asyncio
async def test_blocking_review_reopens_the_item(tmp_path: Path) -> None:
    settings = Settings(
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=100.0,
        max_cycles=20,
    )
    orchestrator = Orchestrator(
        settings,
        research_per_item=0,
        do_review=True,
        models={
            "lead": _stub(_DONE),
            "researcher": _stub(_DONE),
            "reviewer": _stub(_BLOCK, _APPROVE),
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Review test",
        description="review gates items",
        checklist=_checklist(),
        checks=[_PASSING_CHECK],
    )

    assert summary.completed
    assert summary.cycles == 3  # 01, 01 again after the block, 02
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines() if line.strip()]
    reopened = [e for e in events if e["kind"] == "review_reopened"]
    assert len(reopened) == 1
    log = git_ops.log_oneline(tmp_path, 100)
    assert any("lha: review reopened 01" in line for line in log)


class _Pricey:
    """A fake provider that costs $1 per call (worst case == actual) and always says done."""

    name = "fake:pricey"
    default_max_tokens = 10

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        return TurnResult(
            text='{"done": true, "verdict": "approve", "blocking_issues": []}',
            usage=Usage(input_tokens=1, output_tokens=1, model="pricey"),
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 1.0


@pytest.mark.asyncio
async def test_every_role_is_metered_and_budget_hard_stops(tmp_path: Path) -> None:
    settings = Settings(
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=2.5,
        max_cycles=20,
    )
    pricey = _Pricey()
    orchestrator = Orchestrator(
        settings,
        research_per_item=1,
        do_review=True,
        models={"lead": pricey, "researcher": pricey, "reviewer": pricey},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Budget test",
        description="spend",
        checklist=_checklist(),
        checks=[_PASSING_CHECK],
    )
    # research ($1) + lead ($1) = $2 recorded; the reviewer's call ($2 + $1 worst case > $2.5)
    # is refused BEFORE it runs, so spend never exceeds the ceiling.
    assert summary.stopped_reason.startswith("governor:")
    assert summary.total_usd == pytest.approx(2.0)
