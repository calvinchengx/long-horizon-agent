"""Activities of the durable multi-agent organization (research runs as child workflows).

``MissionWorkflow`` uses these when a mission opts in (``MissionInput.research_per_item`` /
``review`` / ``max_parallel``; see ``lha.durable.org_round``):

* ``plan_round`` — read-only: the committed checklist and ownership map decide whether the next
  round is a parallel wave (items that each own a disjoint write-set) or a serial Lead cycle. It
  also removes worktrees, branches and cached results a previous, interrupted wave left behind.
* ``run_implementer`` — one parallel implementer (``lha.agents.waves.implement_in_worktree``)
  in its own git worktree on its own branch ``lha/implementer-<item>/<cycle>``. Heartbeats while
  it runs. Retry-safe: an exclusive per-cycle lock, a fresh worktree from the round's base on
  every attempt, and a result cached under ``.git/lha/implementers/`` once the branch is
  committed, so a retry after a crash returns the same branch instead of redoing the work.
  Leases (``request_lease``) are decided and committed through ``LeaseBroker``.
* ``integrate_branch`` — the ``BranchIntegrator`` merges one implementer branch (verified,
  owned per the committed map including leases, conflict-free, re-verified on the merged
  checkout) and commits the checkpoint: the integration commit IS the checkpoint. Exactly once
  per cycle id (a retry finds the committed ``cycle`` event). A failed implementer or a refused
  branch is recorded as a failed attempt (blocked after 3, then split by the replanner).
* ``review_cycle`` — an independent Reviewer (fresh context, read-only tools) reviews one
  verified item's diff and commits its verdict as a ``review`` event; a blocking verdict reopens
  the item (``todo``), and the third blocking review in a row blocks it instead, so a human
  decides at the deadlock gate. Exactly once per reviewed cycle.

Every activity that changes the mission checkout takes the checkout's lock (``workdir_lock``)
and starts from a clean ``HEAD``. Spend is metered against the mission budget (seeded with the
mission's recorded spend) and appended to the spend journal, like a cycle's.
"""

from __future__ import annotations

import asyncio
import dataclasses
import json
import re
from collections.abc import Awaitable, Callable
from pathlib import Path

from temporalio import activity
from temporalio.exceptions import ApplicationError

from lha.agent.assembly import lead_verifier, open_lead_sandbox
from lha.agent.prompt import render_decisions
from lha.agents.integrator import BranchIntegrator, prune_worktrees, remove_worktree
from lha.agents.reflection import reflect_on_failure
from lha.agents.reviewer import REVIEW_EVENT as _REVIEW_EVENT
from lha.agents.reviewer import Reviewer
from lha.agents.router import model_for_role
from lha.agents.waves import (
    MAX_CONSECUTIVE_FAILURES,
    ImplementerRun,
    board_context,
    board_event,
    diff_since,
    implement_in_worktree,
    implementer_objective,
    integrate_run,
    item_checks,
    maybe_split,
    new_implementer_run,
    parallel_batch,
    reflection_event,
    reflection_for,
    reopen_for_review,
)
from lha.config import Settings, get_settings
from lha.contracts.model import ModelProvider
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, DecisionRecord, EventRecord
from lha.contracts.tools import ToolContext
from lha.contracts.verify import VerificationResult
from lha.coordination.enforcement import changed_paths, effective_ownership
from lha.coordination.leases import LeaseBroker, LeaseDecision, finished_writers, lease_handler
from lha.coordination.ownership import writer_for_item
from lha.coordination.ticket import Ticket, TicketStatus
from lha.durable.activities import (
    ModelFactory,
    _config_error,
    _default_model_factory,
    _reset_workdir,
    _result_from_snapshot,
    _with_heartbeat,
    build_cycle_meter,
    committed_cycle_event,
    record_spend,
    resolve_checks,
    workdir_lock,
)
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    CycleInput,
    CycleResult,
    ImplementerInput,
    ImplementerOutput,
    IntegrateInput,
    PendingApproval,
    ReviewInput,
    RoundInput,
    RoundItem,
    RoundPlan,
)
from lha.execution import UnsafeSandboxError
from lha.execution.tools.toolset import build_run_dispatcher
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.hitl.approvals import DeferredApprovalGate
from lha.ids import idempotency_key
from lha.persistence.store import MissionStore, StoreUnavailableError, open_store
from lha.persistence.tracking import LedgerSink
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

REVIEW_EVENT = _REVIEW_EVENT
_CACHE_DIR = "lha/implementers"
_REVIEW_DIFF_CAP = 20_000
_SAFE = re.compile(r"[^A-Za-z0-9_.-]")


def _role_factory(role: str) -> ModelFactory:
    def factory(settings: Settings, _snapshot: object) -> ModelProvider:
        return model_for_role(role, settings)

    return factory


def _attempt() -> int:
    return activity.info().attempt if activity.in_activity() else 1


def _budget_error(exc: BudgetExceeded) -> ApplicationError:
    return ApplicationError(str(exc), type=ERROR_BUDGET_EXCEEDED, non_retryable=True)


async def _attach_ledger(
    settings: Settings, workdir: str, mission_id: str, meter: CostMeter, prefix: str
) -> MissionStore:
    """Hook the meter to the persistent cost ledger; returns the store (close it)."""
    try:
        store = await open_store(settings, workdir=workdir)
    except StoreUnavailableError as exc:
        raise _config_error(f"cannot open the mission store: {exc}", exc) from exc
    if activity.in_activity():
        info = activity.info()
        prefix = f"{prefix}:{info.workflow_id}:{info.activity_id}@{info.attempt}"
    LedgerSink(store, mission_id, key_prefix=prefix).attach(meter)
    return store


async def _close_store(store: MissionStore, meter: CostMeter) -> None:
    meter.on_record = None
    await store.close()


# --- plan_round -----------------------------------------------------------------------------
def _cache_dir(workdir: str) -> Path:
    return git_ops.git_dir(workdir) / _CACHE_DIR


def _clean_leftovers(workdir: str) -> None:
    """Worktrees, implementer branches and cached results of an interrupted wave."""
    prune_worktrees(workdir)
    cache = _cache_dir(workdir)
    if cache.exists():
        for child in cache.iterdir():
            child.unlink(missing_ok=True)


async def _plan_round(inp: RoundInput) -> RoundPlan:
    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(_clean_leftovers, inp.workdir)
    anchor = GitMissionAnchor(inp.workdir)
    snapshot = await anchor.read_situational_awareness()
    checklist = await anchor.read_checklist()
    ownership = effective_ownership(await anchor.read_ownership(), finished_writers(checklist))
    batch = parallel_batch(checklist, ownership, inp.max_parallel)
    items = batch or [i for i in [checklist.next_actionable()] if i is not None]
    return RoundPlan(
        head_sha=snapshot.head_sha,
        items=[RoundItem(item_id=i.id, description=i.description) for i in items],
        parallel=bool(batch),
        is_complete=snapshot.is_complete,
        is_deadlocked=snapshot.is_deadlocked,
    )


@activity.defn
async def plan_round(inp: RoundInput) -> RoundPlan:
    """Decide the next round (a parallel wave or a serial Lead cycle) from the committed truth."""
    return await _plan_round(inp)


# --- run_implementer ------------------------------------------------------------------------
def _cache_path(workdir: str, cycle_id: str) -> Path:
    return _cache_dir(workdir) / f"{_SAFE.sub('_', cycle_id)}.json"


def _load_cached(inp: ImplementerInput) -> ImplementerOutput | None:
    """A previous attempt's result, if its branch still points at the recorded head."""
    path = _cache_path(inp.workdir, inp.cycle_id)
    if not path.exists():
        return None
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
        raw["pending_approvals"] = [PendingApproval(**p) for p in raw["pending_approvals"]]
        out = ImplementerOutput(**raw)
    except (ValueError, TypeError, KeyError):
        return None
    tip = git_ops.run_git(
        inp.workdir, "rev-parse", "--verify", "-q", f"refs/heads/{out.branch}", check=False
    )
    return out if out.branch and tip.strip() == out.head else None


def _save_cached(workdir: str, out: ImplementerOutput) -> None:
    path = _cache_path(workdir, out.cycle_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(dataclasses.asdict(out)), encoding="utf-8")


def _context_notes(inp: ImplementerInput) -> str:
    parts: list[str] = []
    if inp.steer_notes:
        notes = "\n".join(f"- {note}" for note in inp.steer_notes)
        parts.append(f"Operator steering (most recent last):\n{notes}")
    if inp.approved_actions:
        approved = "\n".join(f"- {a.summary}" for a in inp.approved_actions)
        parts.append(
            "An operator APPROVED these previously queued actions; each is allowed once, "
            f"exactly as requested:\n{approved}"
        )
    return "\n\n".join(parts)


def _output(
    run: ImplementerRun, inp: ImplementerInput, gate: DeferredApprovalGate, spent: float
) -> ImplementerOutput:
    return ImplementerOutput(
        item_id=run.item.id,
        cycle_id=inp.cycle_id,
        branch=run.branch,
        head=run.head,
        brief=run.brief[:8_000],
        tool_calls=run.tool_calls,
        error=run.error,
        verification_json=run.verification.model_dump_json() if run.verification else "",
        decisions_json=[d.model_dump_json() for d in run.decisions],
        ticket_json=run.ticket.model_dump_json(),
        ticket_history=[{k: str(v) for k, v in h.items()} for h in run.tickets],
        leases=[d.model_dump_json() for d in run.leases],
        spent_usd=spent,
        pending_approvals=[PendingApproval(**p) for p in gate.pending],
        used_approvals=list(gate.used),
    )


async def _run_implementer(
    inp: ImplementerInput,
    *,
    settings: Settings | None = None,
    model_factory: ModelFactory | None = None,
) -> ImplementerOutput:
    """One implementer attempt (see the module docstring); safe to retry."""
    settings = settings or get_settings()
    factory = model_factory or _role_factory("implementer")
    checks = resolve_checks(inp.check_commands)
    async with workdir_lock(inp.workdir, name=f"lha-impl-{_SAFE.sub('_', inp.cycle_id)}.lock"):
        cached = await asyncio.to_thread(_load_cached, inp)
        if cached is not None:  # a previous attempt committed the branch, then crashed
            return cached
        anchor = GitMissionAnchor(inp.workdir)
        snapshot = await anchor.read_situational_awareness()
        checklist = await anchor.read_checklist()
        item = checklist.get(inp.item_id)
        if item is None or not item.is_actionable_status:
            status = item.status if item is not None else "missing"
            return ImplementerOutput(
                item_id=inp.item_id,
                cycle_id=inp.cycle_id,
                error=f"item {inp.item_id} is no longer actionable ({status})",
            )
        ownership = effective_ownership(await anchor.read_ownership(), finished_writers(checklist))
        meter = await asyncio.to_thread(
            build_cycle_meter,
            settings,
            CycleInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=inp.cycle_id,
                budget_usd=inp.budget_usd,
                max_cycles=inp.max_cycles,
            ),
        )
        focused = snapshot.model_copy(update={"active_item": item})
        try:
            model = meter.wrap(factory(settings, focused), role="implementer")
        except ValueError as exc:
            raise _config_error(f"cannot build the model: {exc}", exc) from exc
        try:
            store = await _attach_ledger(settings, inp.workdir, inp.mission_id, meter, "impl")
        except BaseException:
            await model.aclose()
            raise
        gate = DeferredApprovalGate(approved=[a.fingerprint for a in inp.approved_actions])
        run = new_implementer_run(
            item,
            inp.cycle_id,
            ownership,
            tool_budget=settings.max_turns_per_cycle,
            acceptance=[c.name for c in checks],
        )
        # The board and this item's reflection come from the committed log, as a resumed
        # ``lha orchestrate`` rebuilds them; the operator's notes follow the board.
        events = await anchor.read_events()
        objective, extra = implementer_objective(
            run,
            mission_text=snapshot.mission.render_anchor() if snapshot.mission else "",
            reflection=reflection_for(events, item.id),
            decisions_text=render_decisions(snapshot.last_decisions),
            briefs=list(inp.research_briefs),
            board="\n\n".join(p for p in (board_context(events), _context_notes(inp)) if p),
        )
        leases: list[LeaseDecision] = []
        handler = lease_handler(
            LeaseBroker(GitMissionAnchor(inp.workdir)),
            writer=run.writer,
            cycle_id=inp.cycle_id,
            ownership=ownership,
            log=leases,
        )
        try:
            await _with_heartbeat(
                implement_in_worktree(
                    run,
                    settings=settings,
                    workdir=inp.workdir,
                    base=inp.base_sha,
                    ownership=ownership,
                    model=model,
                    gate=gate,
                    allow_egress=None,
                    mission_id=inp.mission_id,
                    mission_checks=checks,
                    objective=objective,
                    extra=extra,
                    lease=handler,
                ),
                f"implementer:{inp.item_id}",
            )
        except BudgetExceeded as exc:
            raise _budget_error(exc) from exc
        except UnsafeSandboxError as exc:
            raise _config_error(f"cannot open the sandbox: {exc}", exc) from exc
        finally:
            try:
                await model.aclose()
                if run.worktree is not None:  # the branch stays for the integrator
                    await asyncio.to_thread(remove_worktree, inp.workdir, path=run.worktree)
                await asyncio.to_thread(
                    record_spend,
                    inp.workdir,
                    key=idempotency_key(inp.mission_id, inp.cycle_id, "impl", _attempt()),
                    cycle_id=inp.cycle_id,
                    ledger=meter.ledger,
                )
            finally:
                await _close_store(store, meter)
        run.leases = leases
        spent = sum(e.usd for e in meter.ledger.entries if e.cycle_id == inp.cycle_id)
        out = _output(run, inp, gate, spent)
        await asyncio.to_thread(_save_cached, inp.workdir, out)
        return out


def make_implementer_activity(
    *, settings: Settings | None = None, model_factory: ModelFactory | None = None
) -> Callable[[ImplementerInput], Awaitable[ImplementerOutput]]:
    """A ``run_implementer`` activity bound to explicit settings / model factory (tests)."""

    @activity.defn(name="run_implementer")
    async def run_implementer_bound(inp: ImplementerInput) -> ImplementerOutput:
        return await _run_implementer(inp, settings=settings, model_factory=model_factory)

    return run_implementer_bound


@activity.defn
async def run_implementer(inp: ImplementerInput) -> ImplementerOutput:
    """One parallel implementer in its own worktree (worker settings, the implementer's model)."""
    return await _run_implementer(inp)


# --- integrate_branch -----------------------------------------------------------------------
def _abort_merge(workdir: str) -> None:
    if (git_ops.git_dir(workdir) / "MERGE_HEAD").exists():
        git_ops.run_git(workdir, "merge", "--abort", check=False)


def _delete_branch(workdir: str, branch: str) -> None:
    if branch:
        git_ops.run_git(workdir, "branch", "-D", branch, check=False)


def _run_from_output(
    inp: IntegrateInput, item: ChecklistItem, fallback: ImplementerRun
) -> ImplementerRun:
    out = inp.output
    if out is None:
        fallback.error = inp.error or "the implementer produced no result"
        return fallback
    run = ImplementerRun(
        item=item,
        cycle_id=inp.cycle_id,
        writer=writer_for_item(item.id),
        ticket=Ticket.model_validate_json(out.ticket_json) if out.ticket_json else fallback.ticket,
        branch=out.branch,
        head=out.head,
        brief=out.brief,
        tool_calls=out.tool_calls,
        verification=(
            VerificationResult.model_validate_json(out.verification_json)
            if out.verification_json
            else None
        ),
        decisions=[DecisionRecord.model_validate_json(d) for d in out.decisions_json],
        error=out.error,
        tickets=[dict(h) for h in out.ticket_history] or fallback.tickets,
        leases=[LeaseDecision.model_validate_json(d) for d in out.leases],
    )
    if run.ticket.status in (TicketStatus.DONE, TicketStatus.FAILED):
        run.ticket = fallback.ticket  # never re-advance a terminal ticket
    return run


async def _integrate_branch(
    inp: IntegrateInput,
    *,
    settings: Settings | None = None,
    model_factory: ModelFactory | None = None,
) -> CycleResult:
    """Integrate one implementer's branch and commit the checkpoint (exactly once per cycle)."""
    settings = settings or get_settings()
    factory = model_factory or _default_model_factory
    checks = resolve_checks(inp.check_commands)
    branch = inp.output.branch if inp.output is not None else ""
    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(_abort_merge, inp.workdir)
        await asyncio.to_thread(_reset_workdir, inp.workdir, settings)
        anchor = GitMissionAnchor(inp.workdir)
        payload = await asyncio.to_thread(committed_cycle_event, inp.workdir, inp.cycle_id)
        if payload is not None:  # a previous attempt committed this integration, then crashed
            await asyncio.to_thread(_delete_branch, inp.workdir, branch)
            snapshot = await anchor.read_situational_awareness()
            status = str(payload.get("status", ""))
            return _result_from_snapshot(
                snapshot,
                item_id=inp.item_id,
                advanced=True,
                verdict=str(payload.get("verdict", "")),
                item_blocked=status == "blocked",
                item_split=status == "split",
                note="already integrated by a previous attempt",
            )
        checklist = await anchor.read_checklist()
        item = checklist.get(inp.item_id)
        if item is None or not item.is_actionable_status:
            await asyncio.to_thread(_delete_branch, inp.workdir, branch)
            snapshot = await anchor.read_situational_awareness()
            return _result_from_snapshot(
                snapshot, item_id=inp.item_id, advanced=False, note="item no longer actionable"
            )
        finished = finished_writers(checklist)
        ownership = effective_ownership(await anchor.read_ownership(), finished)
        fallback = new_implementer_run(
            item,
            inp.cycle_id,
            ownership,
            tool_budget=settings.max_turns_per_cycle,
            acceptance=[c.name for c in checks],
        )
        run = _run_from_output(inp, item, fallback)
        if run.head:  # the git-layer check, against the committed map (leases included)
            paths = await asyncio.to_thread(changed_paths, inp.workdir, inp.base_sha, run.head)
            run.violations = ownership.violations(writer=run.writer, paths=paths)
        snapshot = await anchor.read_situational_awareness()
        spend = CycleInput(
            mission_id=inp.mission_id,
            workdir=inp.workdir,
            cycle_id=inp.cycle_id,
            budget_usd=inp.budget_usd,
            max_cycles=inp.max_cycles,
        )

        async def split(current_list: Checklist, current: ChecklistItem) -> list[str]:
            meter = await asyncio.to_thread(build_cycle_meter, settings, spend)
            try:
                model = meter.wrap(factory(settings, snapshot), role="replanner")
            except ValueError as exc:
                raise _config_error(f"cannot build the model: {exc}", exc) from exc
            try:
                store = await _attach_ledger(settings, inp.workdir, inp.mission_id, meter, "split")
            except BaseException:
                await model.aclose()
                raise
            try:
                return await maybe_split(
                    current_list,
                    current,
                    settings=settings,
                    model=model,
                    mission_text=snapshot.mission.render_anchor() if snapshot.mission else "",
                )
            except BudgetExceeded as exc:
                raise _budget_error(exc) from exc
            finally:
                try:
                    await model.aclose()
                    await asyncio.to_thread(
                        record_spend,
                        inp.workdir,
                        key=idempotency_key(inp.mission_id, inp.cycle_id, "split", _attempt()),
                        cycle_id=inp.cycle_id,
                        ledger=meter.ledger,
                    )
                finally:
                    await _close_store(store, meter)

        # What ``lha orchestrate`` posts to its board during a round is committed here with the
        # integration checkpoint: each research brief and the implementer's summary.
        events: list[EventRecord] = [
            board_event(f"researcher:{inp.item_id}", brief, inp.cycle_id)
            for brief in inp.research_brief_texts
        ]
        if inp.output is not None and inp.output.brief:
            events.append(
                board_event(run.writer, f"[{inp.item_id}] {inp.output.brief}", inp.cycle_id)
            )
        if inp.research_briefs or inp.research_failures:
            events.append(
                EventRecord(
                    kind="research",
                    cycle_id=inp.cycle_id,
                    payload={
                        "item": inp.item_id,
                        "n": inp.research_briefs,
                        "failed": len(inp.research_failures),
                        "failures": [f[:500] for f in inp.research_failures],
                    },
                )
            )
        try:
            session = await open_lead_sandbox(settings, inp.workdir)
        except (UnsafeSandboxError, ValueError) as exc:
            raise _config_error(f"cannot open the sandbox: {exc}", exc) from exc
        integrator = BranchIntegrator(
            workdir=inp.workdir,
            session=session,
            verifier=lead_verifier(inp.workdir, settings),
            checks=checks,
        )
        try:
            report = await _with_heartbeat(
                integrate_run(
                    run,
                    anchor=anchor,
                    integrator=integrator,
                    workdir=inp.workdir,
                    base=inp.base_sha,
                    checks=item_checks(item, checks, settings.trusted_check_commands())[0],
                    split=split,
                    events=events,
                ),
                f"integrate:{inp.item_id}",
            )
        finally:
            await session.close()
            await asyncio.to_thread(_abort_merge, inp.workdir)
        await asyncio.to_thread(_delete_branch, inp.workdir, branch)
        if not report.merged and report.status not in ("blocked", "split"):
            await _reflect(inp, item, report.reason, settings=settings, factory=factory)
        after = await anchor.read_situational_awareness()
        note = "verified + integrated" if report.merged else f"not integrated: {report.reason}"
        return _result_from_snapshot(
            after,
            item_id=inp.item_id,
            advanced=True,
            verdict=report.verdict,
            item_blocked=report.status == "blocked",
            item_split=report.status == "split",
            note=note[:2_000],
            spent_usd=inp.output.spent_usd if inp.output is not None else 0.0,
            base_sha=report.before,
        )


async def _reflect(
    inp: IntegrateInput,
    item: ChecklistItem,
    reason: str,
    *,
    settings: Settings,
    factory: ModelFactory,
) -> None:
    """Reflect on a failed attempt and commit the lesson as a ``reflection`` event.

    ``lha orchestrate`` does the same after a failed integration; here the event gets an
    anchor-only commit of its own, since the integration checkpoint is already made. Best
    effort: a model failure is logged and the attempt stays recorded as failed.
    """
    anchor = GitMissionAnchor(inp.workdir)
    snapshot = await anchor.read_situational_awareness()
    meter = await asyncio.to_thread(
        build_cycle_meter,
        settings,
        CycleInput(
            mission_id=inp.mission_id,
            workdir=inp.workdir,
            cycle_id=inp.cycle_id,
            budget_usd=inp.budget_usd,
            max_cycles=inp.max_cycles,
        ),
    )
    try:
        model = meter.wrap(factory(settings, snapshot), role="reflection")
    except ValueError as exc:
        activity.logger.warning("reflection skipped: cannot build the model: %s", exc)
        return
    try:
        store = await _attach_ledger(settings, inp.workdir, inp.mission_id, meter, "reflect")
    except BaseException:
        await model.aclose()
        raise
    try:
        text = await reflect_on_failure(
            model=model,
            item_description=item.description,
            failure_summary=f"{item.id} was not integrated: {reason}",
        )
    except BudgetExceeded as exc:
        raise _budget_error(exc) from exc
    except Exception as exc:  # the lesson is help, not a gate
        activity.logger.warning("reflection on %s failed: %s", item.id, exc)
        return
    finally:
        try:
            await model.aclose()
            await asyncio.to_thread(
                record_spend,
                inp.workdir,
                key=idempotency_key(inp.mission_id, inp.cycle_id, "reflect", _attempt()),
                cycle_id=inp.cycle_id,
                ledger=meter.ledger,
            )
        finally:
            await _close_store(store, meter)
    await anchor.commit_anchor_update(
        Checkpoint(
            cycle_id=inp.cycle_id,
            progress_summary="",
            checklist=await anchor.read_checklist(),
            events=[
                reflection_event(item.id, f"\nReflection on {item.id}: {text}\n", inp.cycle_id)
            ],
            commit_message=f"lha: reflection on {item.id}",
        )
    )


def make_integrate_activity(
    *, settings: Settings | None = None, model_factory: ModelFactory | None = None
) -> Callable[[IntegrateInput], Awaitable[CycleResult]]:
    """An ``integrate_branch`` activity bound to explicit settings / model factory (tests)."""

    @activity.defn(name="integrate_branch")
    async def integrate_branch_bound(inp: IntegrateInput) -> CycleResult:
        return await _integrate_branch(inp, settings=settings, model_factory=model_factory)

    return integrate_branch_bound


@activity.defn
async def integrate_branch(inp: IntegrateInput) -> CycleResult:
    """Merge one implementer branch and commit the checkpoint (worker settings)."""
    return await _integrate_branch(inp)


# --- review_cycle ---------------------------------------------------------------------------
def _blocking_streak(events: list[EventRecord], item_id: str) -> int:
    """Blocking reviews of ``item_id`` since its last approval."""
    streak = 0
    for event in events:
        if event.kind == REVIEW_EVENT and event.payload.get("item_id") == item_id:
            streak = streak + 1 if event.payload.get("blocking") else 0
    return streak


async def _review_cycle(
    inp: ReviewInput,
    *,
    settings: Settings | None = None,
    model_factory: ModelFactory | None = None,
) -> CycleResult:
    """Review one verified item and commit the verdict (exactly once per reviewed cycle)."""
    settings = settings or get_settings()
    factory = model_factory or _role_factory("reviewer")
    review_id = f"{inp.cycle_id}-review"
    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(_reset_workdir, inp.workdir, settings)
        anchor = GitMissionAnchor(inp.workdir)
        payload = await asyncio.to_thread(
            committed_cycle_event, inp.workdir, review_id, kind=REVIEW_EVENT
        )
        if payload is not None:
            snapshot = await anchor.read_situational_awareness()
            return _result_from_snapshot(
                snapshot,
                item_id=inp.item_id,
                advanced=True,
                verdict="review_blocked" if payload.get("blocking") else "passed",
                item_blocked=bool(payload.get("blocked")),
                note="already reviewed by a previous attempt",
            )
        checklist = await anchor.read_checklist()
        snapshot = await anchor.read_situational_awareness()
        item = checklist.get(inp.item_id)
        if item is None or item.status != "done":
            return _result_from_snapshot(
                snapshot, item_id=inp.item_id, advanced=False, note="item not done: no review"
            )
        base = inp.base_sha or f"{inp.head_sha}^1"
        diff = await asyncio.to_thread(diff_since, inp.workdir, base, inp.head_sha)
        meter = await asyncio.to_thread(
            build_cycle_meter,
            settings,
            CycleInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=review_id,
                budget_usd=inp.budget_usd,
                max_cycles=inp.max_cycles,
            ),
        )
        try:
            model = meter.wrap(factory(settings, snapshot), role="reviewer")
            tools = build_run_dispatcher(settings, allow_mutating=False, allow_egress=False)
        except ValueError as exc:
            raise _config_error(f"cannot assemble the reviewer: {exc}", exc) from exc
        try:
            session = await open_lead_sandbox(settings, inp.workdir)
        except (UnsafeSandboxError, ValueError) as exc:
            await model.aclose()
            raise _config_error(f"cannot open the sandbox: {exc}", exc) from exc
        try:
            store = await _attach_ledger(settings, inp.workdir, inp.mission_id, meter, "review")
        except BaseException:
            await session.close()
            await model.aclose()
            raise
        try:
            review = await _with_heartbeat(
                Reviewer(model, tools).review(
                    diff=diff[:_REVIEW_DIFF_CAP],
                    criteria=item.description,
                    ctx=ToolContext(mission_id=inp.mission_id, session=session),
                ),
                review_id,
            )
        except BudgetExceeded as exc:
            raise _budget_error(exc) from exc
        finally:
            try:
                await session.close()
                await model.aclose()
                await asyncio.to_thread(
                    record_spend,
                    inp.workdir,
                    key=idempotency_key(inp.mission_id, review_id, _attempt()),
                    cycle_id=review_id,
                    ledger=meter.ledger,
                )
            finally:
                await _close_store(store, meter)
        blocked = False
        if review.blocking:
            streak = _blocking_streak(await anchor.read_events(), inp.item_id) + 1
            blocked = streak >= MAX_CONSECUTIVE_FAILURES
            reopen_for_review(checklist, inp.item_id, review, block=blocked)
        outcome = ("blocked" if blocked else "reopened") if review.blocking else "approved"
        await anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=review_id,
                progress_summary=f"- {review_id} review of [{inp.item_id}]: {outcome}",
                checklist=checklist,
                events=[
                    *(
                        [board_event(f"reviewer:{inp.item_id}", review.notes(), review_id)]
                        if review.blocking
                        else []
                    ),
                    EventRecord(
                        kind=REVIEW_EVENT,
                        cycle_id=review_id,
                        payload={
                            "item_id": inp.item_id,
                            "verdict": review.verdict,
                            "blocking": review.blocking,
                            "blocking_issues": review.blocking_issues[:20],
                            "advisory": review.advisory[:20],
                            "reopened": review.blocking and not blocked,
                            "blocked": blocked,
                            "base": base,
                            "head": inp.head_sha,
                        },
                    ),
                ],
                commit_message=f"lha: review {outcome} {inp.item_id}",
            )
        )
        after = await anchor.read_situational_awareness()
        return _result_from_snapshot(
            after,
            item_id=inp.item_id,
            advanced=True,
            verdict="review_blocked" if review.blocking else "passed",
            item_blocked=blocked,
            note=review.notes()[:2_000],
            spent_usd=sum(e.usd for e in meter.ledger.entries if e.cycle_id == review_id),
        )


def make_review_activity(
    *, settings: Settings | None = None, model_factory: ModelFactory | None = None
) -> Callable[[ReviewInput], Awaitable[CycleResult]]:
    """A ``review_cycle`` activity bound to explicit settings / model factory (tests)."""

    @activity.defn(name="review_cycle")
    async def review_cycle_bound(inp: ReviewInput) -> CycleResult:
        return await _review_cycle(inp, settings=settings, model_factory=model_factory)

    return review_cycle_bound


@activity.defn
async def review_cycle(inp: ReviewInput) -> CycleResult:
    """Independent review of one verified item (worker settings, the reviewer's model)."""
    return await _review_cycle(inp)


def make_org_activities(
    *,
    settings: Settings | None = None,
    implementer_factory: ModelFactory | None = None,
    reviewer_factory: ModelFactory | None = None,
    lead_factory: ModelFactory | None = None,
) -> list[Callable[..., Awaitable[object]]]:
    """``plan_round`` + the three bound org activities (for tests and embedding)."""
    return [
        plan_round,
        make_implementer_activity(settings=settings, model_factory=implementer_factory),
        make_integrate_activity(settings=settings, model_factory=lead_factory),
        make_review_activity(settings=settings, model_factory=reviewer_factory),
    ]
