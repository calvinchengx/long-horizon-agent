"""The optional plan (spec) review: advisory findings, a bounded re-plan, and its event.

The gate is off by default; when on, a design reviewer model reads the Planner's checklist and,
on findings, the Planner re-plans up to ``LHA_PLAN_REVIEW_MAX`` times. It is advisory: the
per-item verifier still gates every item, an unparseable reply means no findings, and the
``plan_review`` trace event (before the first cycle) records what each review saw.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lha.agent.runner import run_mission_local
from lha.agents.plan_review import (
    PlanReview,
    PlanReviewer,
    acceptance_with_findings,
    parse_findings,
    plan_with_optional_review,
    review_plan,
)
from lha.agents.planner import Planner
from lha.config import Settings
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.model.stub import StubModel

_PLAN = '[{"description": "step one"}, {"description": "step two"}]'
_PLAN_FIXED = (
    '[{"description": "step one"}, {"description": "step two"}, {"description": "step three"}]'
)
_FINDINGS = '["item 02 is not independently verifiable", "missing the migration step"]'


class _Counting:
    """A scripted stub that counts its calls (so a test can prove one was never made)."""

    name = "stub:counting"

    def __init__(self, script: list[TurnResult]) -> None:
        self._model = StubModel(script=script)
        self.calls = 0

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls += 1
        return await self._model.complete(messages, tools=tools, max_tokens=max_tokens)

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


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


# --- parsing ------------------------------------------------------------------------------------


def test_parse_findings_is_defensive() -> None:
    assert parse_findings(_FINDINGS) == [
        "item 02 is not independently verifiable",
        "missing the migration step",
    ]
    assert parse_findings("[]") == []
    assert parse_findings('Sure:\n["too coarse"]\nHope that helps.') == ["too coarse"]
    assert parse_findings("no json here") == []
    assert parse_findings('{"findings": ["x"]}') == ["x"]  # the outermost [...] wins
    assert parse_findings('["a", " a ", "b", "a", ""]') == [
        "a",
        "b",
    ]  # trimmed, deduped, blank dropped


def test_acceptance_with_findings_appends_them() -> None:
    text = acceptance_with_findings("all tests pass", ["fix deps"])
    assert text.startswith("all tests pass") and "- fix deps" in text
    assert acceptance_with_findings("", []).startswith("(use your judgment)")


# --- the gate -----------------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_findings_then_none_replans_once() -> None:
    planner = _Counting([TurnResult(text=_PLAN), TurnResult(text=_PLAN_FIXED)])
    reviewer = _Counting([TurnResult(text=_FINDINGS), TurnResult(text="[]")])
    plan, reviews = await review_plan(
        Planner(planner), PlanReviewer(reviewer), title="T", description="D", max_replans=1
    )
    assert planner.calls == 2  # the Planner re-planned once
    assert [r.findings for r in reviews] == [
        ["item 02 is not independently verifiable", "missing the migration step"],
        [],
    ]
    assert [r.replanned for r in reviews] == [True, False]
    assert [r.item_count for r in reviews] == [2, 3]  # the re-plan's checklist was reviewed
    assert len(plan.checklist.items) == 3


@pytest.mark.asyncio
async def test_no_findings_does_not_replan() -> None:
    planner = _Counting([TurnResult(text=_PLAN)])
    reviewer = _Counting([TurnResult(text="[]")])
    plan, reviews = await review_plan(
        Planner(planner), PlanReviewer(reviewer), title="T", description="D", max_replans=3
    )
    assert planner.calls == 1 and reviewer.calls == 1
    assert len(reviews) == 1 and reviews[0].findings == [] and reviews[0].replanned is False
    assert len(plan.checklist.items) == 2


@pytest.mark.asyncio
async def test_findings_persisting_to_the_bound_proceed_after_max_replans() -> None:
    planner = _Counting([TurnResult(text=_PLAN), TurnResult(text=_PLAN_FIXED)])
    reviewer = _Counting([TurnResult(text=_FINDINGS)])  # always finds something
    _, reviews = await review_plan(
        Planner(planner), PlanReviewer(reviewer), title="T", description="D", max_replans=1
    )
    assert planner.calls == 2  # exactly MAX re-plans, then it starts anyway
    assert [r.replanned for r in reviews] == [True, False]
    assert all(r.findings for r in reviews)


@pytest.mark.asyncio
async def test_unparseable_reply_means_no_findings() -> None:
    planner = _Counting([TurnResult(text=_PLAN)])
    reviewer = _Counting([TurnResult(text="I could not review this plan.")])
    _, reviews = await review_plan(
        Planner(planner), PlanReviewer(reviewer), title="T", description="D", max_replans=1
    )
    assert planner.calls == 1 and len(reviews) == 1 and reviews[0].replanned is False


@pytest.mark.asyncio
async def test_review_off_makes_no_reviewer_call() -> None:
    planner = _Counting([TurnResult(text=_PLAN)])
    reviewer_model = _Counting([TurnResult(text=_FINDINGS)])
    plan, reviews = await plan_with_optional_review(
        planner=Planner(planner),
        model=reviewer_model,
        settings=_settings(plan_review=False),
        title="T",
        description="D",
    )
    assert reviewer_model.calls == 0 and reviews == []
    assert planner.calls == 1 and len(plan.checklist.items) == 2


@pytest.mark.asyncio
async def test_review_on_uses_the_bound_from_settings() -> None:
    planner = _Counting([TurnResult(text=_PLAN), TurnResult(text=_PLAN_FIXED)])
    reviewer_model = _Counting([TurnResult(text=_FINDINGS)])  # always finds something
    _, reviews = await plan_with_optional_review(
        planner=Planner(planner),
        model=reviewer_model,
        settings=_settings(plan_review=True, plan_review_max=1),
        title="T",
        description="D",
    )
    assert planner.calls == 2 and len(reviews) == 2
    assert reviews[0].event_data() == {"item_count": 2, "findings": 2, "replanned": 1}
    assert reviews[1].event_data() == {"item_count": 3, "findings": 2, "replanned": 0}


# --- the recorded event -------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_run_records_the_plan_review_event_before_the_first_cycle(tmp_path: Path) -> None:
    reviews = [
        PlanReview(item_count=2, findings=["a", "b"], replanned=True),
        PlanReview(item_count=3, findings=[], replanned=False),
    ]
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[Check(name="green", command=["python", "-c", "pass"])],
        settings=_settings(),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "ok"}')]),
        plan_reviews=reviews,
    )
    assert summary.completed
    trace = [json.loads(line) for line in summary.trace_jsonl.splitlines() if line.strip()]
    plan_events = [t for t in trace if t["kind"] == "plan_review"]
    assert [e["data"] for e in plan_events] == [
        {"item_count": 2, "findings": 2, "replanned": 1},
        {"item_count": 3, "findings": 0, "replanned": 0},
    ]
    assert all(e["cycle_id"] == "" for e in plan_events)  # before any cycle
    # They precede the first cycle's event.
    first_plan = next(i for i, t in enumerate(trace) if t["kind"] == "plan_review")
    first_cycle = next(i for i, t in enumerate(trace) if t["kind"] == "cycle_started")
    assert first_plan < first_cycle
