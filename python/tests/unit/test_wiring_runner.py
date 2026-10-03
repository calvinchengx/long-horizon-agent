"""Integration wiring of the local runner: sandbox factory, one CostMeter, deadlock vs complete."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from lha.agent.runner import build_meter, plan_and_run_local, run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.execution.factory import UnsafeSandboxError
from lha.governor.governor import LoopDetector
from lha.model.stub import StubModel

_PASS = Check(name="green", command=[sys.executable, "-c", "pass"])
_FAIL = Check(name="red", command=[sys.executable, "-c", "raise SystemExit(1)"])
_DONE = TurnResult(text='{"done": true, "summary": "ok"}')


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 20,
        "max_turns_per_cycle": 3,
        "stall_limit": 50,
    }
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


def _one_item() -> Checklist:
    return Checklist(items=[ChecklistItem(id="01", description="do it")])


@pytest.mark.asyncio
async def test_local_sandbox_requires_explicit_opt_in(tmp_path: Path) -> None:
    workdir = tmp_path / "ws"
    with pytest.raises(UnsafeSandboxError):
        await run_mission_local(
            workdir=str(workdir),
            title="t",
            description="d",
            checklist=_one_item(),
            checks=[_PASS],
            settings=_settings(allow_unsafe_local=False),
        )
    assert not workdir.exists()  # refused before touching the workspace


@pytest.mark.asyncio
async def test_runner_completes_when_verified(tmp_path: Path) -> None:
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=_one_item(),
        checks=[_PASS],
        settings=_settings(),
        model=StubModel(script=[_DONE]),
    )
    assert summary.completed and summary.stopped_reason == "complete"


@pytest.mark.asyncio
async def test_runner_reports_deadlock_never_complete(tmp_path: Path) -> None:
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=_one_item(),
        checks=[_FAIL],
        settings=_settings(),
        model=StubModel(script=[_DONE]),
    )
    assert not summary.completed
    assert summary.stopped_reason.startswith("deadlocked:")
    assert summary.items_done == 0 and summary.items_total == 1
    assert summary.cycles == 3  # AgentLoop blocks the item after 3 consecutive failures


class _Pricey:
    """$1 per call (worst case == actual): writes a file, then says done."""

    name = "fake:pricey"
    default_max_tokens = 10

    def __init__(self) -> None:
        self.calls = 0

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls += 1
        text = (
            '{"tool": "write_file", "arguments": {"path": "a.txt", "content": "x"}}'
            if self.calls == 1
            else '{"done": true}'
        )
        return TurnResult(text=text, usage=Usage(input_tokens=1, output_tokens=1, model="p"))

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 1.0


@pytest.mark.asyncio
async def test_budget_exceeded_mid_cycle_stops_cleanly(tmp_path: Path) -> None:
    pricey = _Pricey()
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=_one_item(),
        checks=[_PASS],
        settings=_settings(budget_usd_ceiling=1.5),
        model=pricey,
    )
    assert summary.stopped_reason.startswith("governor:")
    assert "worst-case" in summary.stopped_reason
    assert summary.total_usd == pytest.approx(1.0)  # the 2nd call was refused before it ran
    assert pricey.calls == 1
    assert not summary.completed


@pytest.mark.asyncio
async def test_planner_and_lead_share_one_meter(tmp_path: Path) -> None:
    settings = _settings()
    meter = build_meter(settings)
    summary = await plan_and_run_local(
        workdir=str(tmp_path),
        title="t",
        task="do it",
        checks=[_PASS],
        settings=settings,
        meter=meter,
    )
    roles = {entry.role for entry in meter.ledger.entries}
    assert {"planner", "lead"} <= roles
    assert summary.completed


def test_loop_detector_counts_consecutive_failures_only() -> None:
    detector = LoopDetector(threshold=2)
    assert not detector.observe("01")
    assert not detector.observe("01", failed=False)  # progress resets the streak
    assert not detector.observe("01")
    assert detector.observe("01")
    assert not detector.observe("02")  # signatures are independent


async def test_the_loop_detector_stops_a_repeating_failure(tmp_path: Path) -> None:
    import json

    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=_one_item(),
        checks=[_FAIL],
        settings=_settings(stall_limit=2),
        model=StubModel(script=[_DONE]),
    )
    assert summary.stopped_reason == "loop on item 01" and summary.cycles == 2
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    assert [e["data"] for e in events if e["kind"] == "loop_detected"] == [{"item_id": "01"}]
