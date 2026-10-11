"""The optional plan (spec) review: an advisory design review of the Planner's checklist.

The Planner runs once, at intake, and its checklist drives the whole mission. A plan whose items
are not independently verifiable, are too coarse to finish in one focused attempt, leave out work
the task needs, or declare wrong dependencies wastes the mission before it starts. When
``LHA_PLAN_REVIEW`` is on (off by default), a small ``PlanReviewer`` asks a design reviewer model
to list the ways the plan is inadequate, and the gate re-plans (bounded by ``LHA_PLAN_REVIEW_MAX``)
so the Planner can fix them, then reviews again.

It is ADVISORY: an unparseable reply means "no findings" (never blocks the mission), and the
per-item deterministic verifier still gates every item, so a plan the review approves can never
make an item pass. Nothing else about the plan path changes; when the setting is off the Planner
is called exactly once and the reviewer is never built.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from lha.agents.planner import MissionPlan, Planner
from lha.contracts.model import ModelMessage, ModelProvider

#: The reviewer's system prompt (mirrors the Replanner's own ``_SYSTEM``, not a ``ROLES`` entry).
_SYSTEM = (
    "You are a design reviewer. You are given a mission and the checklist a planner produced for "
    "it. List only the ways the plan is inadequate: items that are not independently verifiable, "
    "items too coarse to finish in one focused attempt, work the task needs that is missing, or "
    "dependencies that are wrong. Be concrete and brief. If the plan is sound, return an empty "
    "array."
)
_INSTRUCTIONS = (
    "Reply with ONLY a JSON array of short findings (strings). An empty array [] means the plan is "
    "sound. No prose, no code fences."
)

#: At most this many findings are kept (a plan that is wrong everywhere needs no more).
MAX_FINDINGS = 20


def parse_findings(text: str) -> list[str]:
    """Extract short findings from model output; unparseable output means no findings.

    Mirrors ``planner.parse_plan``'s bracket extraction: the outermost ``[...]`` decoded as a JSON
    array of strings. Advisory: anything else (prose, a bad array, a non-list) is ``[]``.
    """
    start = text.find("[")
    end = text.rfind("]")
    if start == -1 or end == -1 or end <= start:
        return []
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return []
    if not isinstance(parsed, list):
        return []
    out: list[str] = []
    for value in parsed:
        finding = str(value).strip()
        if finding and finding not in out:
            out.append(finding)
    return out[:MAX_FINDINGS]


@dataclass(frozen=True)
class PlanReview:
    """One review of a plan: what it saw and whether it forced a re-plan."""

    item_count: int
    findings: list[str] = field(default_factory=list)
    replanned: bool = False

    def event_data(self) -> dict[str, object]:
        """The ``plan_review`` trace event's payload (``findings`` is the count)."""
        return {
            "item_count": self.item_count,
            "findings": len(self.findings),
            "replanned": int(self.replanned),
        }


def plan_text(*, title: str, description: str, acceptance: str, plan: MissionPlan) -> str:
    """The plan as the reviewer sees it (the mission, the definition of done, the items)."""
    done = acceptance.strip() or "(use your judgment)"
    lines = [
        f"Mission: {title}",
        "",
        description,
        "",
        f"Definition of done: {done}",
        "",
        "Plan:",
    ]
    for item in plan.checklist.items:
        files = plan.files.get(item.id, [])
        suffix = f" (files: {', '.join(files)})" if files else ""
        lines.append(f"[{item.id}] {item.description}{suffix}")
    return "\n".join(lines)


def reviewer_messages(*, title: str, description: str, acceptance: str, plan: MissionPlan):
    """The messages ``PlanReviewer.review`` sends (exported for parity tests)."""
    return [
        ModelMessage(role="system", content=_SYSTEM),
        ModelMessage(
            role="user",
            content=f"{plan_text(title=title, description=description, acceptance=acceptance, plan=plan)}\n\n{_INSTRUCTIONS}",
        ),
    ]


def acceptance_with_findings(acceptance: str, findings: list[str]) -> str:
    """The Planner's ``acceptance`` with the review's findings appended, so a re-plan can fix them.

    The Planner is given the findings through the same ``acceptance`` channel the Replanner is
    given the failure report, so no new Planner parameter is needed.
    """
    base = acceptance.strip() or "(use your judgment)"
    listed = "\n".join(f"- {finding}" for finding in findings)
    return (
        f"{base}\n\nA design review of the previous plan found:\n{listed}\n"
        "Fix these in the new plan."
    )


class PlanReviewer:
    """One model call that lists the ways a plan is inadequate (``[]`` when it is sound)."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def review(
        self, *, title: str, description: str, acceptance: str, plan: MissionPlan
    ) -> list[str]:
        messages = reviewer_messages(
            title=title, description=description, acceptance=acceptance, plan=plan
        )
        result = await self._model.complete(messages)
        return parse_findings(result.text)


async def review_plan(
    planner: Planner,
    reviewer: PlanReviewer,
    *,
    title: str,
    description: str,
    acceptance: str = "",
    max_replans: int = 1,
) -> tuple[MissionPlan, list[PlanReview]]:
    """Plan, review, and re-plan up to ``max_replans`` times; return the plan and every review.

    Each review is recorded (one ``PlanReview`` per call). A review with findings triggers a
    re-plan only while re-plans remain; after the bound the last plan starts anyway. The caller
    decides whether to review at all (``LHA_PLAN_REVIEW``).
    """
    plan = await planner.plan_mission(title=title, description=description, acceptance=acceptance)
    reviews: list[PlanReview] = []
    replans = 0
    while True:
        findings = await reviewer.review(
            title=title, description=description, acceptance=acceptance, plan=plan
        )
        replanned = bool(findings) and replans < max_replans
        reviews.append(
            PlanReview(item_count=len(plan.checklist.items), findings=findings, replanned=replanned)
        )
        if not replanned:
            return plan, reviews
        replans += 1
        plan = await planner.plan_mission(
            title=title,
            description=description,
            acceptance=acceptance_with_findings(acceptance, findings),
        )


async def plan_with_optional_review(
    *,
    planner: Planner,
    model: ModelProvider,
    settings,
    title: str,
    description: str,
    acceptance: str = "",
) -> tuple[MissionPlan, list[PlanReview]]:
    """The Planner, reviewed and re-planned when ``settings.plan_review`` is on.

    Off (the default): the Planner is called exactly once and no reviewer is built, so planning is
    byte-identical to before.
    """
    if not settings.plan_review:
        plan = await planner.plan_mission(
            title=title, description=description, acceptance=acceptance
        )
        return plan, []
    return await review_plan(
        planner,
        PlanReviewer(model),
        title=title,
        description=description,
        acceptance=acceptance,
        max_replans=settings.plan_review_max,
    )
