"""A local (non-Temporal) mission runner.

Drives the ``AgentLoop`` cycle-by-cycle until the mission completes, deadlocks (nothing actionable
but items still open — never reported as complete), the budget governor refuses the next step or a
model call (``BudgetExceeded``), or a loop is detected. Every model call (planner + lead) goes
through ONE ``CostMeter`` so a single ledger/governor sees all spend. The sandbox comes from
``settings.sandbox`` via ``open_sandbox`` (``local`` requires ``settings.allow_unsafe_local``). This is the simplest way to run a real mission end-to-end at
$0 (stub/Ollama) on one machine — no Temporal server required. The durable spine runs the same
loop inside an activity; this runner is for dev, demos, and CI.
"""

from __future__ import annotations

from dataclasses import dataclass

from lha.agent.assembly import build_lead_loop, open_lead_sandbox
from lha.agents.planner import Planner
from lha.config import Settings, get_settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.state import Checklist
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.tools import default_local_tools
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor, LoopDetector
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.ids import new_id
from lha.model import build_provider
from lha.obs.events import TraceRecorder, configure_logging
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.verifier import default_python_checks


@dataclass
class MissionSummary:
    """Terminal summary of a locally-run mission.

    ``stopped_reason`` is ``"complete"`` ONLY when every item is verified done; otherwise e.g.
    ``"deadlocked: <reason>"``, ``"governor: <reason>"`` (budget / max cycles),
    ``"loop on item <id>"`` or ``"max_cycles"``.
    """

    mission_id: str
    completed: bool
    cycles: int
    items_done: int
    items_total: int
    total_usd: float
    head_sha: str
    stopped_reason: str
    trace_jsonl: str


def build_meter(settings: Settings) -> CostMeter:
    """A fresh ledger + governor from settings (unknown/unpriced cost denied unless opted in)."""
    governor = BudgetGovernor(
        ceiling_usd=settings.budget_usd_ceiling,
        max_cycles=settings.max_cycles,
        allow_unknown_cost=settings.allow_unpriced_models,
    )
    return CostMeter(ledger=CostLedger(), governor=governor)


async def aclose_provider(provider: object | None) -> None:
    """Close a provider that owns resources (e.g. an HTTP client); no-op for ``None``/stubs."""
    close = getattr(provider, "aclose", None)
    if close is not None:
        await close()


def full_access_dispatcher(*, allow_egress: bool = False) -> AllowListDispatcher:
    """The Lead's dispatcher: every default tool, mutating allowed (explicit — the default is
    fail-closed)."""
    return AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, allow_egress=allow_egress
    )


async def run_mission_local(
    *,
    workdir: str,
    title: str,
    description: str,
    checklist: Checklist,
    checks: list[Check] | None = None,
    settings: Settings | None = None,
    anchor_text: str | None = None,
    allow_egress: bool = False,
    meter: CostMeter | None = None,
    model: ModelProvider | None = None,
    gate: HITLGate | None = None,
    references: list[str] | None = None,
) -> MissionSummary:
    """Initialize the anchor and run cycles until done / deadlocked / over-budget / looping.

    ``checks``: the gating verification checks (``None`` => ``default_python_checks()``; an item
    is never marked done without at least one gating check). ``meter``: share one budget/ledger
    with other callers (e.g. the Planner). ``model``: override the lead provider (it is wrapped
    with the meter either way). ``gate``: where irreversible commands go for a human decision
    (``None`` denies them). ``references``: vendored reference paths recited every cycle.
    """
    settings = settings or get_settings()
    configure_logging()
    recorder = TraceRecorder()
    meter = meter or build_meter(settings)
    loop_detector = LoopDetector(threshold=settings.stall_limit)
    mission_checks = default_python_checks() if checks is None else checks

    session = await open_lead_sandbox(settings, workdir)
    mission_id = new_id("mission")
    cycles = 0
    last_head = ""
    stopped = "max_cycles"
    # A provider built here owns its HTTP client and is closed here; a caller's ``model`` is not.
    owned_model: ModelProvider | None = None
    try:
        if model is None:
            model = owned_model = build_provider(settings)
        anchor = GitMissionAnchor(workdir)
        await anchor.initialize(
            title=title, description=description, items=checklist, references=references
        )
        if allow_egress and not settings.web_hosts():
            raise ValueError(
                "allow_egress needs LHA_WEB_ALLOW_HOSTS (the hosts fetch_url may read)"
            )
        loop = build_lead_loop(
            settings,
            model=meter.wrap(model, role="lead"),
            anchor=anchor,
            workdir=workdir,
            gate=gate,
            recorder=recorder,
        )
        ctx = ToolContext(mission_id=mission_id, session=session)

        while cycles < settings.max_cycles:
            decision = meter.governor.authorize_next(meter.ledger, cycles_done=cycles)
            if not decision.allow:
                recorder.record("governor_block", mission_id=mission_id, reason=decision.reason)
                stopped = f"governor: {decision.reason}"
                break

            meter.cycle_id = f"c{cycles + 1}"
            try:
                outcome = await loop.run_cycle(
                    ctx=ctx,
                    mission_id=mission_id,
                    cycle_id=meter.cycle_id,
                    anchor_text=anchor_text,
                    checks=mission_checks,
                )
            except BudgetExceeded as exc:
                recorder.record("governor_block", mission_id=mission_id, reason=str(exc))
                stopped = f"governor: {exc.decision.reason}"
                break
            if outcome.advanced:
                cycles += 1
            last_head = outcome.head_sha or last_head

            if outcome.is_complete:
                stopped = "complete"
                break
            if outcome.is_deadlocked or not outcome.advanced:
                reason = outcome.reason or "no actionable item"
                recorder.record("deadlocked", mission_id=mission_id, reason=reason)
                stopped = f"deadlocked: {reason}"
                break
            # Stop if the same item keeps failing verification (consecutive; reset on success).
            if loop_detector.observe(f"{outcome.item_id}:failed", failed=not outcome.verified):
                recorder.record(
                    "loop_detected", mission_id=mission_id, item_id=outcome.item_id or ""
                )
                stopped = f"loop on item {outcome.item_id}"
                break

        final = await anchor.read_checklist()
    finally:
        try:
            await session.close()
        finally:
            await aclose_provider(owned_model)
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


async def plan_and_run_local(
    *,
    workdir: str,
    title: str,
    task: str,
    checks: list[Check] | None = None,
    settings: Settings | None = None,
    allow_egress: bool = False,
    meter: CostMeter | None = None,
    gate: HITLGate | None = None,
    references: list[str] | None = None,
) -> MissionSummary:
    """Mission intake → execution: the Planner decomposes ``task`` into a checklist, then run it.

    Planner and lead share one ``CostMeter``; ``BudgetExceeded`` from the planning call
    propagates (nothing has run yet).
    """
    settings = settings or get_settings()
    meter = meter or build_meter(settings)
    planner_model = build_provider(settings)
    try:
        checklist = await Planner(meter.wrap(planner_model, role="planner")).plan(
            title=title, description=task
        )
    finally:
        await aclose_provider(planner_model)
    return await run_mission_local(
        workdir=workdir,
        title=title,
        description=task,
        checklist=checklist,
        checks=checks,
        settings=settings,
        allow_egress=allow_egress,
        meter=meter,
        gate=gate,
        references=references,
    )
