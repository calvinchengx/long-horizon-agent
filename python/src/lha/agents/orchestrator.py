"""The multi-agent orchestrator — drives the full org through a mission (local flow).

This is the asymmetric org in action: for each checklist item it (1) fans out **read-only
Researchers** to gather context, (2) hands the briefs to the single **Lead Engineer** loop which
implements + verifies + checkpoints, then (3) on failure runs **reflection** (fed into the next
attempt) or (4) on success runs an independent **Reviewer** whose structured verdict can REOPEN
the item. Per-role model tiers, a per-call budget governor, loop detection, and OTel spans wrap
the whole thing.

Every model call of every role goes through ONE ``CostMeter`` (``MeteredModel``): it is
budget-checked before it runs (hard stop, ``BudgetExceeded``) and recorded in one ledger after.
Pass the same meter to the Planner's provider (``meter.wrap(...)``) so intake is counted too.

It deliberately keeps coupled writes single-threaded (only the Lead writes) and parallelizes only
reads — the evidence-backed split. The durable Temporal spine runs the same composition; this is
the $0/local driver for it.
"""

from __future__ import annotations

import asyncio
import contextlib

import httpx

from lha.agent.assembly import build_lead_loop, open_lead_sandbox
from lha.agent.runner import MissionSummary, build_meter
from lha.agents.reflection import reflect_on_failure
from lha.agents.reviewer import Reviewer, ReviewResult
from lha.agents.router import model_for_role
from lha.agents.team import research_fanout
from lha.config import Settings, get_settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.state import Checklist, Checkpoint
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.tools.toolset import build_run_dispatcher, preflight_run_tools
from lha.governor.governor import LoopDetector
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.ids import new_id
from lha.obs.events import TraceRecorder, configure_logging
from lha.obs.otel import agent_span
from lha.persistence.services import open_run_services
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.verifier import default_python_checks

_ROLES = ("lead", "researcher", "reviewer")
_REVIEW_DIFF_CAP = 20_000


def _diff_since(workdir: str, base: str, head: str) -> str:
    if not base or not head or base == head:
        return "(no new commits)"
    diff = git_ops.run_git(
        workdir, "diff", f"{base}..{head}", "--", ".", ":(exclude).lha", check=False
    )
    return diff or "(empty diff)"


class Orchestrator:
    """Runs a mission with the full org (research fan-out + Lead + review + reflection)."""

    def __init__(
        self,
        settings: Settings | None = None,
        *,
        research_per_item: int = 2,
        do_review: bool = True,
        meter: CostMeter | None = None,
        models: dict[str, ModelProvider] | None = None,
    ) -> None:
        """``meter``: share one budget/ledger with other callers (e.g. the Planner).
        ``models``: per-role provider overrides (``lead``/``researcher``/``reviewer``); unmetered
        providers are wrapped with the meter here either way.
        """
        self._settings = settings or get_settings()
        self._research_per_item = research_per_item
        self._do_review = do_review
        self._meter = meter
        self._models = dict(models or {})

    async def run_mission(
        self,
        *,
        workdir: str,
        title: str,
        description: str,
        checklist: Checklist,
        checks: list[Check] | None = None,
        allow_egress: bool | None = None,
        gate: HITLGate | None = None,
        references: list[str] | None = None,
    ) -> MissionSummary:
        """Run the org until complete / deadlocked / over-budget / looping.

        ``checks``: gating verification checks (``None`` => ``default_python_checks()``).
        The sandbox comes from ``settings.sandbox`` (``local`` needs ``allow_unsafe_local``).
        ``allow_egress``: ``None`` => the Lead and the Researchers get the web tools iff
        ``LHA_WEB_ALLOW_HOSTS`` is set (the Reviewer's role hides them); ``False`` drops them. A
        lethal-trifecta run raises ``RuleOfTwoViolation`` before it starts.
        """
        settings = self._settings
        preflight_run_tools(settings)
        configure_logging()
        recorder = TraceRecorder()
        meter = self._meter or build_meter(settings)
        loop_detector = LoopDetector(threshold=settings.stall_limit)
        mission_checks = default_python_checks() if checks is None else checks

        # The sandbox session and one shared HTTP pool for every role's provider are both closed
        # when the mission ends, however it ends.
        async with contextlib.AsyncExitStack() as stack:
            session = await open_lead_sandbox(settings, workdir)
            stack.push_async_callback(session.close)
            http = await stack.enter_async_context(httpx.AsyncClient(timeout=300.0))
            anchor = GitMissionAnchor(workdir)
            await anchor.initialize(
                title=title, description=description, items=checklist, references=references
            )
            raw = {
                role: self._models.get(role) or model_for_role(role, settings, client=http)
                for role in _ROLES
            }
            lead_model = meter.wrap(raw["lead"], role="lead")
            research_model = meter.wrap(raw["researcher"], role="researcher")
            review_model = meter.wrap(raw["reviewer"], role="reviewer")
            reflection_model = meter.wrap(raw["lead"], role="reflection")

            # Researchers get the web tools with the lead (the Reviewer's role hides them).
            read_tools = build_run_dispatcher(
                settings, allow_mutating=False, allow_egress=allow_egress
            )
            mission_id = new_id("mission")
            # Persistence + memory (mission row, persistent cost ledger, tiered memory).
            services = await open_run_services(
                settings,
                mission_id=mission_id,
                workdir=workdir,
                meter=meter,
                title=title,
                description=description,
                model=meter.wrap(raw["lead"], role="librarian"),
                recorder=recorder,
            )
            stack.push_async_callback(services.close)
            await services.tracker.running()
            lead = build_lead_loop(
                settings,
                model=lead_model,
                anchor=anchor,
                workdir=workdir,
                gate=gate,
                recorder=recorder,
                memory=services.memory,
                allow_egress=allow_egress,
            )
            reviewer = Reviewer(review_model, read_tools)

            ctx = ToolContext(mission_id=mission_id, session=session)
            reflections: dict[str, str] = {}
            cycles = 0
            last_head = ""
            stopped = "max_cycles"

            try:
                while cycles < settings.max_cycles:
                    decision = meter.governor.authorize_next(meter.ledger, cycles_done=cycles)
                    if not decision.allow:
                        recorder.record(
                            "governor_block", mission_id=mission_id, reason=decision.reason
                        )
                        stopped = f"governor: {decision.reason}"
                        break

                    snapshot = await anchor.read_situational_awareness()
                    if snapshot.is_complete:
                        stopped = "complete"
                        break
                    if snapshot.is_deadlocked or snapshot.active_item is None:
                        stopped = f"deadlocked: {snapshot.deadlock_reason or 'no actionable item'}"
                        break
                    item = snapshot.active_item
                    cycle_id = f"c{cycles + 1}"
                    meter.cycle_id = cycle_id

                    briefs: list[str] = []
                    if self._research_per_item > 0:
                        queries = [
                            f"Find context relevant to: {item.description}",
                            f"Find existing files/code related to: {item.description}",
                        ][: self._research_per_item]
                        results = await research_fanout(
                            model=research_model, dispatcher=read_tools, ctx=ctx, queries=queries
                        )
                        briefs = [r.brief for r in results if r.error is None and r.brief]
                        failures = [r.error for r in results if r.error is not None]
                        recorder.record(
                            "research",
                            mission_id=mission_id,
                            item=item.id,
                            n=len(briefs),
                            failed=len(failures),
                        )
                        for error in failures:
                            recorder.record(
                                "research_failed", mission_id=mission_id, item=item.id, error=error
                            )

                    # The anchor comes from the immutable mission spec, never from progress.md.
                    mission_text = (
                        snapshot.mission.render_anchor()
                        if snapshot.mission is not None
                        else f"Mission: {title}\n\n{description}"
                    )
                    anchor_text = (
                        f"{mission_text}\n"
                        f"{reflections.get(item.id, '')}\n\n"
                        f"Research briefs:\n" + ("\n---\n".join(briefs) or "(none)")
                    )

                    with agent_span("cycle", mission_id=mission_id, item=item.id):
                        outcome = await lead.run_cycle(
                            ctx=ctx,
                            mission_id=mission_id,
                            cycle_id=cycle_id,
                            anchor_text=anchor_text,
                            checks=mission_checks,
                        )
                    if not outcome.advanced:  # nothing actionable after all
                        stopped = f"deadlocked: {outcome.reason or 'no actionable item'}"
                        break
                    cycles += 1
                    last_head = outcome.head_sha or last_head

                    if not outcome.verified:
                        if outcome.is_deadlocked:
                            recorder.record(
                                "deadlocked", mission_id=mission_id, reason=outcome.reason
                            )
                            stopped = f"deadlocked: {outcome.reason or 'no actionable item'}"
                            break
                        if loop_detector.observe(f"{item.id}:failed"):
                            stopped = f"loop on item {item.id}"
                            break
                        if outcome.item_blocked:  # not re-picked; no point reflecting on it
                            continue
                        reflection = await reflect_on_failure(
                            model=reflection_model,
                            item_description=item.description,
                            failure_summary=(
                                f"{item.id} failed verification ({outcome.verdict}) after "
                                f"{outcome.turns} turns: {outcome.reason}"
                            ),
                        )
                        reflections[item.id] = f"\nReflection on {item.id}: {reflection}\n"
                        recorder.record("reflection", mission_id=mission_id, item=item.id)
                        continue
                    loop_detector.observe(f"{item.id}:failed", failed=False)

                    if self._do_review:
                        diff = await asyncio.to_thread(
                            _diff_since, workdir, snapshot.head_sha, outcome.head_sha
                        )
                        review = await reviewer.review(
                            diff=diff[:_REVIEW_DIFF_CAP], criteria=item.description, ctx=ctx
                        )
                        recorder.record(
                            "review",
                            mission_id=mission_id,
                            item=item.id,
                            blocking=review.blocking,
                            verdict=review.verdict,
                        )
                        if review.blocking:
                            last_head = (
                                await self._reopen(anchor, item.id, cycle_id, review) or last_head
                            )
                            reflections[item.id] = f"\n{review.notes()}\n"
                            recorder.record("review_reopened", mission_id=mission_id, item=item.id)
                            if loop_detector.observe(f"{item.id}:review_blocked"):
                                stopped = f"loop on item {item.id} (review keeps blocking)"
                                break
                            continue
                        loop_detector.observe(f"{item.id}:review_blocked", failed=False)

                    if outcome.is_complete:
                        stopped = "complete"
                        break
            except BudgetExceeded as exc:
                recorder.record("governor_block", mission_id=mission_id, reason=str(exc))
                stopped = f"governor: {exc.decision.reason}"
            except BaseException as exc:
                await services.finish(f"error: {type(exc).__name__}", head_sha=last_head)
                raise

            final = await anchor.read_checklist()
            await services.finish(stopped, head_sha=last_head)
        return MissionSummary(
            mission_id=mission_id,
            completed=final.is_complete,
            cycles=cycles,
            items_done=final.items_done,
            items_total=final.items_total,
            total_usd=meter.ledger.total_usd,
            head_sha=last_head,
            stopped_reason=stopped,
            trace_jsonl=recorder.to_jsonl(),
        )

    @staticmethod
    async def _reopen(
        anchor: GitMissionAnchor, item_id: str, cycle_id: str, review: ReviewResult
    ) -> str | None:
        """Put a verified-but-review-blocked item back to ``todo`` with the review notes."""
        checklist = await anchor.read_checklist()
        item = checklist.get(item_id)
        if item is None:
            return None
        item.status = "todo"
        item.verified_by = []
        item.notes = review.notes()
        item.last_failure = "reviewer blocked: " + "; ".join(review.blocking_issues or ["unparsed"])
        return await anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=cycle_id,
                progress_summary=f"{cycle_id}: review reopened {item_id} ({review.verdict})",
                checklist=checklist,
                commit_message=f"lha: review reopened {item_id}",
            )
        )
