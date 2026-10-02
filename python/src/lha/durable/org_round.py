"""One round of the multi-agent organization inside ``MissionWorkflow`` (workflow code).

Used instead of the single ``run_agent_cycle`` call when a mission opts in
(``MissionInput.research_per_item`` > 0, ``review``, or ``max_parallel`` >= 2); with all three
off the workflow runs exactly as before. Deterministic scheduling only; the work happens in
activities (``lha.durable.org_activities``) and in researcher child workflows
(``SubAgentWorkflow``). A round is:

1. ``plan_round`` reads the committed checklist and ownership map: a **wave** when two or more
   actionable items each own a disjoint write-set (at most ``max_parallel``, and never more than
   the cycles left), otherwise the next actionable item (**serial**).
2. **Research** (``research_per_item`` > 0): that many read-only researcher child workflows per
   item, concurrently (``fan_out_children``). Every failure is kept: it is written to the gate
   log (``lha mission-status``) and committed as a ``research`` event with the round's
   checkpoint; the round goes on with the briefs that did arrive.
3. **Serial**: one ``run_agent_cycle`` with the briefs in the Lead's prompt. **Wave**: one
   ``run_implementer`` activity per item, concurrently, each in its own worktree; then one
   ``integrate_branch`` activity per item, in checklist order: each integration commit is a
   checkpoint and counts as a cycle.
4. **Review** (``review``): after every verified item (a passed serial cycle, a merged branch),
   ``review_cycle``. A blocking review reopens the item; a review that cannot run (its activity
   failed) is written to the gate log and the item stays done.

Failures: an implementer whose activity fails for good (a non-retryable error) is recorded as a
failed attempt at integration. When implementers fail only with retryable errors (an outage),
the other branches are still integrated, then that error is re-raised so the mission parks as
after a failed cycle. A budget refusal anywhere ends the mission. ``state.cycles_done`` is
advanced here, as each cycle-consuming activity completes, so a round that raises has already
counted the checkpoints it committed.

Cancellation (``lha mission-abort``): every activity of a round is started with
``WAIT_CANCELLATION_COMPLETED``, so the workflow writes ABORTED only after the work in flight has
acknowledged the cancel (an implementer's worktree removed and its spend journaled, a cycle's
last row write landed), and ``raise_if_cancel_requested`` after each one keeps a cancel that an activity
finished through from being lost. The org path is new with ``PATCH_ORG``, so this changes no
recorded history (the cancellation type is not part of the scheduled command).
"""

from __future__ import annotations

import asyncio
import dataclasses
from collections.abc import Callable
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ActivityError, ApplicationError, is_cancelled_exception

with workflow.unsafe.imports_passed_through():
    from lha.durable.activities import run_agent_cycle
    from lha.durable.org_activities import (
        integrate_branch,
        plan_round,
        review_cycle,
        run_implementer,
    )

from lha.durable.subagent_workflow import fan_out_children
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    MAX_PARALLEL,
    MAX_RESEARCH_PER_ITEM,
    CycleInput,
    CycleResult,
    ImplementerInput,
    ImplementerOutput,
    IntegrateInput,
    MissionInput,
    MissionState,
    PendingApproval,
    ReviewInput,
    RoundInput,
    RoundItem,
    RoundPlan,
    SubAgentInput,
)

# ``workflow.patched`` id of the organization path (only reached when a mission opts in).
PATCH_ORG = "lha-durable-org-v1"

_WORK_RETRY = RetryPolicy(
    initial_interval=timedelta(seconds=1),
    maximum_interval=timedelta(seconds=30),
    backoff_coefficient=2.0,
    maximum_attempts=5,
    non_retryable_error_types=[ERROR_BUDGET_EXCEEDED, ERROR_CONFIG],
)
_SHORT_RETRY = RetryPolicy(maximum_attempts=3)
_WORK_TIMEOUT = timedelta(hours=1)
_HEARTBEAT = timedelta(minutes=2)
_PLAN_TIMEOUT = timedelta(minutes=5)
_WAIT_CANCEL = workflow.ActivityCancellationType.WAIT_CANCELLATION_COMPLETED

RESEARCH_TEMPLATES = (
    "Find context relevant to: {task}",
    "Find existing files/code related to: {task}",
    "Find the tests and checks that cover: {task}",
    "Find the conventions and interfaces to respect for: {task}",
)
assert len(RESEARCH_TEMPLATES) >= MAX_RESEARCH_PER_ITEM

Log = Callable[[str], None]


def org_enabled(inp: MissionInput) -> bool:
    """Whether the mission opted into any part of the organization."""
    return inp.research_per_item > 0 or inp.review or inp.max_parallel >= 2


def org_config_error(inp: MissionInput) -> str:
    """Why the organization options are invalid ('' when they are fine)."""
    if not 0 <= inp.research_per_item <= MAX_RESEARCH_PER_ITEM:
        return f"research_per_item must be 0..{MAX_RESEARCH_PER_ITEM} (got {inp.research_per_item})"
    if not 0 <= inp.max_parallel <= MAX_PARALLEL:
        return f"max_parallel must be 0..{MAX_PARALLEL} (got {inp.max_parallel})"
    return ""


def raise_if_cancel_requested() -> None:
    """Re-raise a workflow cancellation that a waited-for activity swallowed.

    With ``WAIT_CANCELLATION_COMPLETED``, an activity that finishes successfully although it was
    asked to cancel hands its result back and the SDK drops the ``CancelledError``; the pending
    request is still counted on the workflow task (``Task.cancelling()``), so an abort is never
    lost that way.
    """
    task = asyncio.current_task()
    if task is not None and task.cancelling():
        raise asyncio.CancelledError("mission cancelled while its work was finishing")


def _app(err: ActivityError) -> ApplicationError | None:
    return err.cause if isinstance(err.cause, ApplicationError) else None


def _is_budget(err: BaseException) -> bool:
    if not isinstance(err, ActivityError):
        return False
    app = _app(err)
    return app is not None and app.type == ERROR_BUDGET_EXCEEDED


async def _research(
    inp: MissionInput, items: list[RoundItem], cycle_id: str, log: Log
) -> dict[str, tuple[list[str], list[str]]]:
    """``research_per_item`` researcher children per item: ``{item: (briefs, failures)}``."""
    per_item = min(inp.research_per_item, MAX_RESEARCH_PER_ITEM)
    if per_item <= 0 or not items:
        return {}
    inputs: list[SubAgentInput] = []
    owners: list[str] = []
    for item in items:
        for template in RESEARCH_TEMPLATES[:per_item]:
            inputs.append(
                SubAgentInput(
                    role_name="researcher",
                    objective=template.format(task=item.description),
                    workdir=inp.workdir,
                    mission_id=inp.mission_id,
                    allow_egress=True,
                    budget_usd=inp.budget_usd,
                    cycle_id=f"{cycle_id}-research",
                )
            )
            owners.append(item.item_id)
    results = await fan_out_children(inputs)
    out: dict[str, tuple[list[str], list[str]]] = {item.item_id: ([], []) for item in items}
    for owner, res in zip(owners, results, strict=True):
        briefs, failures = out[owner]
        if isinstance(res, str):  # a failure description
            failures.append(res)
            log(f"research for {owner} failed: {res[:300]}")
        elif res.brief:
            briefs.append(res.brief)
    return out


async def _review(
    inp: MissionInput, state: MissionState, result: CycleResult, cycle_id: str, log: Log
) -> CycleResult:
    """Review a verified item; fold the verdict into the cycle's result."""
    if not (inp.review and result.advanced and result.verdict == "passed" and result.item_id):
        return result
    try:
        rev = await workflow.execute_activity(
            review_cycle,
            ReviewInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=cycle_id,
                item_id=result.item_id,
                head_sha=result.head_sha,
                base_sha=result.base_sha,
                budget_usd=inp.budget_usd,
                max_cycles=inp.max_cycles,
            ),
            start_to_close_timeout=_WORK_TIMEOUT,
            heartbeat_timeout=_HEARTBEAT,
            retry_policy=_WORK_RETRY,
            cancellation_type=_WAIT_CANCEL,
        )
        raise_if_cancel_requested()
    except ActivityError as err:
        if is_cancelled_exception(err) or _is_budget(err):
            raise
        log(f"review of {result.item_id} ({cycle_id}) failed: {err.cause or err}; left unreviewed")
        return result
    if not rev.advanced:
        return result
    if rev.verdict != "passed":
        how = "blocked it" if rev.item_blocked else "reopened it"
        log(f"review of {result.item_id} ({cycle_id}) {how}")
    return dataclasses.replace(
        result,
        head_sha=rev.head_sha or result.head_sha,
        is_complete=rev.is_complete,
        is_deadlocked=rev.is_deadlocked,
        items_done=rev.items_done,
        items_total=rev.items_total,
        reason=rev.reason if rev.is_deadlocked else result.reason,
        verdict=result.verdict if rev.verdict == "passed" else rev.verdict,
        item_blocked=rev.item_blocked,
        spent_usd=result.spent_usd + rev.spent_usd,
        note=f"{result.note}; review: {rev.verdict}",
    )


async def run_org_round(inp: MissionInput, state: MissionState, log: Log) -> CycleResult:
    """One round (see the module docstring); advances ``state.cycles_done`` itself."""
    remaining = max(1, inp.max_cycles - state.cycles_done)
    plan: RoundPlan = await workflow.execute_activity(
        plan_round,
        RoundInput(
            mission_id=inp.mission_id,
            workdir=inp.workdir,
            max_parallel=min(inp.max_parallel, remaining) if inp.max_parallel >= 2 else 0,
        ),
        start_to_close_timeout=_PLAN_TIMEOUT,
        retry_policy=_SHORT_RETRY,
        cancellation_type=_WAIT_CANCEL,
    )
    raise_if_cancel_requested()
    first = f"c{state.cycles_done + 1}"
    research = await _research(inp, plan.items, first, log)
    if plan.parallel:
        return await _wave(inp, state, plan, research, log)

    item = plan.items[0].item_id if plan.items else None
    briefs, failures = research.get(item, ([], [])) if item is not None else ([], [])
    result = await workflow.execute_activity(
        run_agent_cycle,
        CycleInput(
            mission_id=inp.mission_id,
            workdir=inp.workdir,
            cycle_id=first,
            check_commands=inp.check_commands,
            budget_usd=inp.budget_usd,
            max_cycles=inp.max_cycles,
            steer_notes=list(state.steer_notes),
            approved_actions=list(state.approved_actions),
            research_item=item if item in research else None,
            research_briefs=briefs,
            research_failures=failures,
        ),
        start_to_close_timeout=_WORK_TIMEOUT,
        heartbeat_timeout=_HEARTBEAT,
        retry_policy=_WORK_RETRY,
        cancellation_type=_WAIT_CANCEL,
    )
    state.cycles_done += 1
    raise_if_cancel_requested()
    return await _review(inp, state, result, first, log)


async def _wave(
    inp: MissionInput,
    state: MissionState,
    plan: RoundPlan,
    research: dict[str, tuple[list[str], list[str]]],
    log: Log,
) -> CycleResult:
    start = state.cycles_done + 1
    cycle_ids = [f"c{start + n}" for n in range(len(plan.items))]
    log(f"parallel wave {', '.join(i.item_id for i in plan.items)} at {plan.head_sha[:12]}")
    tasks = [
        asyncio.ensure_future(
            workflow.execute_activity(
                run_implementer,
                ImplementerInput(
                    mission_id=inp.mission_id,
                    workdir=inp.workdir,
                    cycle_id=cycle_id,
                    item_id=item.item_id,
                    base_sha=plan.head_sha,
                    check_commands=inp.check_commands,
                    budget_usd=inp.budget_usd,
                    max_cycles=inp.max_cycles,
                    steer_notes=list(state.steer_notes),
                    approved_actions=list(state.approved_actions),
                    research_briefs=research.get(item.item_id, ([], []))[0],
                    research_failures=research.get(item.item_id, ([], []))[1],
                    wave_size=len(plan.items),
                ),
                start_to_close_timeout=_WORK_TIMEOUT,
                heartbeat_timeout=_HEARTBEAT,
                retry_policy=_WORK_RETRY,
                cancellation_type=_WAIT_CANCEL,
            )
        )
        for item, cycle_id in zip(plan.items, cycle_ids, strict=True)
    ]
    try:
        outcomes = await asyncio.gather(*tasks, return_exceptions=True)
    except asyncio.CancelledError:
        for task in tasks:
            task.cancel()
        raise
    raise_if_cancel_requested()

    deferred: ActivityError | None = None
    last: CycleResult | None = None
    pending: list[PendingApproval] = []
    used: list[str] = []
    for item, cycle_id, outcome in zip(plan.items, cycle_ids, outcomes, strict=True):
        output: ImplementerOutput | None = None
        error = ""
        # (No isinstance on the output: activity results are built with the activity module's
        # own classes, which the workflow sandbox does not share.)
        if not isinstance(outcome, BaseException):
            output = outcome
            pending.extend(outcome.pending_approvals)
            used.extend(outcome.used_approvals)
        elif isinstance(outcome, ActivityError) and not is_cancelled_exception(outcome):
            if _is_budget(outcome):
                raise outcome
            app = _app(outcome)
            if app is None or not app.non_retryable:  # transient: retry in a later round
                deferred = deferred or outcome
                log(f"implementer for {item.item_id} failed (retryable): {outcome.cause}")
                continue
            error = f"{app.type or 'error'}: {app.message}"[:500]
        else:
            raise outcome
        briefs, failures = research.get(item.item_id, ([], []))
        result = await workflow.execute_activity(
            integrate_branch,
            IntegrateInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=cycle_id,
                item_id=item.item_id,
                base_sha=plan.head_sha,
                output=output,
                error=error,
                check_commands=inp.check_commands,
                budget_usd=inp.budget_usd,
                max_cycles=inp.max_cycles,
                research_briefs=len(briefs),
                research_failures=failures,
                research_brief_texts=briefs,
            ),
            start_to_close_timeout=_WORK_TIMEOUT,
            heartbeat_timeout=_HEARTBEAT,
            retry_policy=_WORK_RETRY,
            cancellation_type=_WAIT_CANCEL,
        )
        if result.advanced:
            state.cycles_done += 1
        raise_if_cancel_requested()
        last = await _review(inp, state, result, cycle_id, log)
    if deferred is not None:
        raise deferred  # an outage: the mission parks, then a later round redoes those items
    assert last is not None  # every item was integrated or deferred
    return dataclasses.replace(last, pending_approvals=pending, used_approvals=used)
