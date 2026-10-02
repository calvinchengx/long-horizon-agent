"""The durable control plane: ``MissionWorkflow``.

A deterministic scheduler — NO LLM/tool/IO in this body. It repeatedly dispatches one
``run_agent_cycle`` activity and ends with an explicit outcome:

  * ``completed``         — every checklist item verified done;
  * ``deadlocked``        — items remain but none is actionable (blocked / unsatisfiable deps)
                            and no deadlock gate is configured (or "retry" unblocked nothing);
  * ``aborted``           — "abort" at the deadlock gate (by a human or its default);
  * ``impossible``        — "impossible" at the deadlock gate: a final checkpoint records it;
  * ``budget_exhausted``  — the per-call budget governor refused a model call;
  * ``max_cycles``        — the iteration ceiling was hit.

Temporal journals each finished activity's result, so after a worker crash replay returns
completed cycles from history without re-running them; an attempt that was in flight is retried
(its model calls are re-spent — the activity makes that retry safe, see ``activities``). When a
cycle keeps failing with retryable errors (an outage), the mission does not fail: it PARKS
(status ``DEGRADED_PARK``), durably sleeping with capped exponential backoff and probing
dependency health until they recover, then resumes. Non-retryable errors (budget, configuration)
end or fail the mission immediately. Continue-As-New keeps the history bounded; everything the
workflow needs rides in ``MissionInput.state``.

Human gates (status ``WAITING_ON_HUMAN``): an irreversible tool call a cycle queued (approve /
reject, default reject) and, when ``deadlock_gate_seconds > 0``, a deadlock (retry / abort /
impossible, default ``deadlock_gate_default``). Every gate has a timeout, a default action and an
escalation ladder: reminders at ``gate_escalation_seconds`` after it opens, each recorded in the
anchor and the ``hitl_gates`` table and sent to the optional webhook by the ``notify_gate``
activity (never from workflow code), then the default. ``ops.lifecycle.should_declare_impossible`` drives the deadlock gate's
"impossible" recommendation.

``SLEEPING`` is the status while the workflow waits on a durable timer by design: a scheduled
start (``MissionInput.resume_at``), the pause between cycles (``cycle_pause_seconds``) or an
operator ``snooze``. It is distinct from ``DEGRADED_PARK`` (a dependency is down) and
``WAITING_ON_HUMAN`` (a gate is open).

The ``missions`` row (``lha missions``): the cycle activity writes what a cycle observes (RUNNING,
also when deadlocked, DONE, WAITING_ON_HUMAN for a queued approval); the workflow writes the
statuses only it knows through the ``record_mission_status`` activity — SLEEPING,
DEGRADED_PARK, WAITING_ON_HUMAN when a gate opens, and the final status of every ending
(including a failure and a cancellation by ``lha mission-abort``). Those writes are best effort:
a short timeout and retry, then the mission goes on. On a cancellation the workflow waits for the
cycle in flight to acknowledge it (``WAIT_CANCELLATION_COMPLETED``) before it writes ABORTED, and
the store never moves a terminal status back, so an abort mid-cycle ends ABORTED. An org round
(below) waits the same way for every activity it has in flight.

The multi-agent organization (opt-in per mission: ``MissionInput.research_per_item``,
``review``, ``max_parallel``) replaces the single cycle activity with one round of
``lha.durable.org_round``: researcher child workflows before the round, a serial Lead cycle or a
parallel implementer wave (one activity per implementer, in its own git worktree, then one
integration activity per branch), and an independent review after every verified item. With
all three off (the default) the workflow issues exactly the commands it always did.

Determinism: behaviour added after histories were recorded is guarded by ``workflow.patched``
(``PATCH_*`` below), so a history recorded by an older build replays down its old code path.
"""

from __future__ import annotations

import asyncio
import dataclasses
from collections.abc import Callable
from datetime import UTC, datetime, timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import (
    ActivityError,
    ApplicationError,
    FailureError,
    is_cancelled_exception,
)

# The activity module pulls in git/model/etc.; pass it through the workflow sandbox unchanged
# rather than letting the sandbox re-import (and reject) its non-deterministic dependencies.
with workflow.unsafe.imports_passed_through():
    from lha.durable.activities import (
        check_mission_health,
        declare_impossible,
        notify_gate,
        read_mission_snapshot,
        record_mission_status,
        run_agent_cycle,
        unblock_items,
    )
    from lha.hitl.escalation import escalation_schedule, next_rung
    from lha.ops.lifecycle import should_declare_impossible

from lha.durable.org_round import (
    PATCH_ORG,
    org_config_error,
    org_enabled,
    raise_if_cancel_requested,
    run_org_round,
)
from lha.durable.signals import (
    DEADLOCK_DEFAULTS,
    GATE_DEADLOCK,
    GATE_TOOL_CALL,
    MAX_STEER_CHARS,
    MAX_STEER_NOTES,
    QUERY_GATE,
    QUERY_GATE_LOG,
    QUERY_STATUS,
    QUERY_STEER_NOTES,
    SIGNAL_HUMAN_DECISION,
    SIGNAL_HUMAN_DECISION_V2,
    SIGNAL_SNOOZE,
    SIGNAL_STEER,
    STATUS_ABORTED,
    STATUS_DEGRADED_PARK,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_RUNNING,
    STATUS_SLEEPING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    OUTCOME_ABORTED,
    OUTCOME_BUDGET_EXHAUSTED,
    OUTCOME_COMPLETED,
    OUTCOME_DEADLOCKED,
    OUTCOME_IMPOSSIBLE,
    OUTCOME_MAX_CYCLES,
    ApprovedAction,
    CycleInput,
    CycleResult,
    FinalizeInput,
    GateNotice,
    GateView,
    HealthInput,
    MissionInput,
    MissionResult,
    MissionState,
    MissionStatusInput,
    UnblockInput,
)

_CYCLE_RETRY = RetryPolicy(
    initial_interval=timedelta(seconds=1),
    maximum_interval=timedelta(seconds=30),
    backoff_coefficient=2.0,
    maximum_attempts=5,
    non_retryable_error_types=[ERROR_BUDGET_EXCEEDED, ERROR_CONFIG],
)
CYCLE_START_TO_CLOSE = timedelta(hours=1)
CYCLE_HEARTBEAT_TIMEOUT = timedelta(minutes=2)
_ONE_SHOT = RetryPolicy(maximum_attempts=1)
_SHORT_RETRY = RetryPolicy(maximum_attempts=3)

# The pre-ladder deadlock gate's options (kept so histories recorded before it replay).
DEADLOCK_OPTIONS = ("retry", "abort")
LADDER_DEADLOCK_OPTIONS = ("retry", "abort", "impossible")
APPROVAL_OPTIONS = ("approve", "reject")
MAX_GATE_LOG = 50
_NOTIFY_TIMEOUT = timedelta(minutes=3)

# ``workflow.patched`` ids for behaviour added after histories were recorded.
PATCH_GATE_LADDER = "lha-gate-escalation-v1"
PATCH_SLEEPING = "lha-sleeping-v1"
PATCH_MISSION_ROW = "lha-mission-row-v1"
PATCH_CYCLE_CANCEL = "lha-cycle-wait-cancel-v1"
# A cycle that completes the mission opens no approval gate: no later cycle could use it.
PATCH_COMPLETE_SKIPS_APPROVALS = "lha-complete-skips-approvals-v1"
# PATCH_ORG ("lha-durable-org-v1", ``lha.durable.org_round``): the multi-agent round, reached
# only by missions that opt in, so every history recorded without it replays unchanged. The org
# path always waits for a cancelled activity (see ``org_round``), so it needs no cancel patch.

# ``record_mission_status`` is best effort: a short timeout and a few quick retries, then the
# mission goes on without the row update (logged).
_ROW_TIMEOUT = timedelta(seconds=30)
_ROW_DEADLINE = timedelta(minutes=2)
_ROW_RETRY = RetryPolicy(
    initial_interval=timedelta(seconds=1),
    maximum_interval=timedelta(seconds=5),
    maximum_attempts=3,
    non_retryable_error_types=[ERROR_CONFIG],
)

_TERMINAL_STATUS = {
    OUTCOME_COMPLETED: STATUS_DONE,
    OUTCOME_DEADLOCKED: STATUS_IMPOSSIBLE,
    OUTCOME_IMPOSSIBLE: STATUS_IMPOSSIBLE,
    OUTCOME_BUDGET_EXHAUSTED: STATUS_ABORTED,
    OUTCOME_MAX_CYCLES: STATUS_ABORTED,
    OUTCOME_ABORTED: STATUS_ABORTED,
}


def _application_cause(err: ActivityError) -> ApplicationError | None:
    cause = err.cause
    return cause if isinstance(cause, ApplicationError) else None


def _config_failure(message: str) -> ApplicationError:
    return ApplicationError(message, type=ERROR_CONFIG, non_retryable=True)


def _iso(moment: datetime) -> str:
    return moment.astimezone(UTC).isoformat(timespec="seconds")


@workflow.defn
class MissionWorkflow:
    """Long-lived orchestrator for one mission."""

    def __init__(self) -> None:
        # Signals can arrive before ``run`` starts; they land here and are merged into the
        # carried state at the top of ``run``.
        self._state = MissionState()
        self._park_reason = ""
        self._rejected_decisions: list[str] = []
        self._decision_by = ""
        self._open_question = ""
        self._gate: GateView | None = None

    @workflow.run
    async def run(self, inp: MissionInput) -> MissionResult:
        """Run the mission; the ``missions`` row gets the final status on every ending — also a
        failure or a cancellation (``lha mission-abort``), from these handlers."""
        try:
            return await self._run_mission(inp)
        except (asyncio.CancelledError, Exception) as err:  # never ContinueAsNewError
            if is_cancelled_exception(err):
                reason = "mission cancelled"
            elif isinstance(err, FailureError):  # fails the workflow (not just a task)
                reason = f"mission failed: {err.message}"
            else:
                raise
            self._state.status = STATUS_ABORTED
            await self._record_row(inp, STATUS_ABORTED, reason)
            raise

    async def _run_mission(self, inp: MissionInput) -> MissionResult:
        early = self._state
        state = dataclasses.replace(inp.state) if inp.state is not None else MissionState()
        if early.pending_decision is not None:
            state.pending_decision = early.pending_decision
            state.pending_decision_by = early.pending_decision_by
        state.steer_notes = [*state.steer_notes, *early.steer_notes][-MAX_STEER_NOTES:]
        if inp.state is None:
            state.resume_at = max(inp.resume_at, early.resume_at)
        elif early.resume_at:
            state.resume_at = early.resume_at
        self._state = state

        if inp.check_commands is not None and not any(inp.check_commands):
            raise _config_failure(
                "check_commands is empty: at least one gating check is required; omit it to use "
                "the default Python checks"
            )
        if inp.cycles_before_can < 1:
            raise _config_failure(f"cycles_before_can must be >= 1 (got {inp.cycles_before_can})")
        if inp.max_cycles < 0:
            raise _config_failure(f"max_cycles must be >= 0 (got {inp.max_cycles})")
        org_error = org_config_error(inp)
        if org_error:
            raise _config_failure(org_error)

        while True:
            if state.cycles_done >= inp.max_cycles:
                return await self._terminal(inp, OUTCOME_MAX_CYCLES, "iteration ceiling reached")

            await self._sleep_until_resume(inp)
            state.status = STATUS_RUNNING
            org = org_enabled(inp) and workflow.patched(PATCH_ORG)
            cycles_before = state.cycles_done
            try:
                if org:  # research / review / parallel waves (``lha.durable.org_round``)
                    result = await run_org_round(inp, state, self._log)
                    raise_if_cancel_requested()
                else:
                    # A cancelled cycle is waited for (its last row write lands before ABORTED).
                    wait_cancel = workflow.patched(PATCH_CYCLE_CANCEL)
                    result = await workflow.execute_activity(
                        run_agent_cycle,
                        CycleInput(
                            mission_id=inp.mission_id,
                            workdir=inp.workdir,
                            cycle_id=f"c{state.cycles_done + 1}",
                            check_commands=inp.check_commands,
                            budget_usd=inp.budget_usd,
                            max_cycles=inp.max_cycles,
                            steer_notes=list(state.steer_notes),
                            approved_actions=list(state.approved_actions),
                        ),
                        start_to_close_timeout=CYCLE_START_TO_CLOSE,
                        heartbeat_timeout=CYCLE_HEARTBEAT_TIMEOUT,
                        retry_policy=_CYCLE_RETRY,
                        cancellation_type=(
                            workflow.ActivityCancellationType.WAIT_CANCELLATION_COMPLETED
                            if wait_cancel
                            else workflow.ActivityCancellationType.TRY_CANCEL
                        ),
                    )
                    if wait_cancel:
                        raise_if_cancel_requested()
            except ActivityError as err:
                if is_cancelled_exception(err):
                    raise  # the mission was cancelled: never park on it
                app = _application_cause(err)
                if app is not None and app.type == ERROR_BUDGET_EXCEEDED:
                    return await self._terminal(inp, OUTCOME_BUDGET_EXHAUSTED, app.message)
                if app is not None and app.non_retryable:
                    state.status = STATUS_ABORTED
                    raise ApplicationError(
                        f"mission {inp.mission_id} failed: {app.message}",
                        type=app.type,
                        non_retryable=True,
                    ) from err
                # Retries exhausted on a transient failure: park instead of failing the mission.
                await self._park(inp, reason=str(err.cause or err))
                self._maybe_continue_as_new(inp)
                continue

            if not org:
                state.cycles_done += 1  # an org round counts its own cycles as it goes
            self._absorb(result)
            self._track_failures(result)
            if not (result.is_complete and workflow.patched(PATCH_COMPLETE_SKIPS_APPROVALS)):
                await self._resolve_approvals(inp, result)

            if result.is_complete:
                return await self._terminal(inp, OUTCOME_COMPLETED)
            if result.is_deadlocked:
                ending = await self._on_deadlock(inp, result)
                if ending is None:
                    continue
                return await self._terminal(inp, *ending)

            if inp.cycle_pause_seconds > 0 and workflow.patched(PATCH_SLEEPING):
                wake = workflow.now().timestamp() + inp.cycle_pause_seconds
                state.resume_at = max(state.resume_at, wake)
            if org:  # a wave can advance several cycles: CAN when a multiple is crossed
                cbc = inp.cycles_before_can
                if state.cycles_done // cbc > cycles_before // cbc:
                    workflow.continue_as_new(dataclasses.replace(inp, state=state))
            elif state.cycles_done % inp.cycles_before_can == 0:
                workflow.continue_as_new(dataclasses.replace(inp, state=state))
            self._maybe_continue_as_new(inp)

    # --- internals -----------------------------------------------------------------------
    def _absorb(self, result: CycleResult) -> None:
        state = self._state
        if result.head_sha:
            state.head_sha = result.head_sha
        state.items_done = result.items_done
        state.items_total = result.items_total
        if result.item_id is not None:
            state.last_item = result.item_id

    async def _resolve_approvals(self, inp: MissionInput, result: CycleResult) -> None:
        """Ask a human about each irreversible action the cycle attempted (durably, one by one).

        Approved actions are passed to the following cycles and allowed ONCE (by fingerprint);
        rejected ones are remembered so the same request is not asked again.
        """
        state = self._state
        used = set(result.used_approvals)
        state.approved_actions = [a for a in state.approved_actions if a.fingerprint not in used]
        known = {a.fingerprint for a in state.approved_actions} | set(state.rejected_actions)
        for req in result.pending_approvals:
            if req.fingerprint in known:
                continue
            known.add(req.fingerprint)
            summary = f"{req.tool} {req.arguments}".strip()
            question = (
                f"Mission {inp.mission_id} wants to run an irreversible action: {summary} "
                f"({req.reason}). Approve or reject?"
            )
            if workflow.patched(PATCH_GATE_LADDER):
                decision, _defaulted = await self._run_gate(
                    inp,
                    GateView(
                        gate_id=f"approval-{req.fingerprint[:12]}",
                        kind=GATE_TOOL_CALL,
                        question=question,
                        options=list(APPROVAL_OPTIONS),
                        default_action="reject",
                        request=req,
                    ),
                    timeout_seconds=inp.approval_timeout_seconds,
                )
            else:
                decision = await self.await_human_gate(
                    question=question,
                    options=list(APPROVAL_OPTIONS),
                    default_action="reject",
                    timeout_seconds=inp.approval_timeout_seconds,
                )
            if decision == "approve":
                state.approved_actions.append(ApprovedAction(req.fingerprint, summary[:500]))
            else:
                state.rejected_actions.append(req.fingerprint)

    def _maybe_continue_as_new(self, inp: MissionInput) -> None:
        if workflow.info().is_continue_as_new_suggested():
            workflow.continue_as_new(dataclasses.replace(inp, state=self._state))

    async def _park(self, inp: MissionInput, *, reason: str) -> None:
        """Durably sleep with capped exponential backoff until dependencies are healthy again."""
        state = self._state
        state.status = STATUS_DEGRADED_PARK
        state.parks += 1
        self._park_reason = reason
        await self._record_row(inp, STATUS_DEGRADED_PARK, reason)
        cap = max(1, inp.park_max_seconds)
        delay = min(max(1, inp.park_initial_seconds), cap)
        while True:
            await workflow.sleep(timedelta(seconds=delay))
            try:
                report = await workflow.execute_activity(
                    check_mission_health,
                    HealthInput(mission_id=inp.mission_id, workdir=inp.workdir),
                    start_to_close_timeout=timedelta(minutes=2),
                    retry_policy=_ONE_SHOT,
                )
                if report.healthy:
                    break
                self._park_reason = report.reason
            except ActivityError as err:
                if is_cancelled_exception(err):
                    raise
                self._park_reason = f"health check failed: {err.cause or err}"
            delay = min(delay * 2, cap)
            self._maybe_continue_as_new(inp)
        state.status = STATUS_RUNNING
        self._park_reason = ""

    def _track_failures(self, result: CycleResult) -> None:
        """Consecutive non-passing verified attempts on the same item (pure state, no commands)."""
        state = self._state
        if not result.advanced or result.item_id is None:
            return
        if result.verdict == "passed":
            state.fail_item, state.fail_streak = None, 0
        elif result.item_id == state.fail_item:
            state.fail_streak += 1
        else:
            state.fail_item, state.fail_streak = result.item_id, 1

    def _log(self, line: str) -> None:
        state = self._state
        state.gate_log = [*state.gate_log, f"{_iso(workflow.now())} {line}"][-MAX_GATE_LOG:]

    async def _sleep_until_resume(self, inp: MissionInput) -> None:
        """SLEEPING: a durable timer until ``resume_at`` (a ``snooze`` can move or end it)."""
        state = self._state
        if state.resume_at <= workflow.now().timestamp() or not workflow.patched(PATCH_SLEEPING):
            return
        state.status = STATUS_SLEEPING
        until = _iso(datetime.fromtimestamp(state.resume_at, UTC))
        self._log(f"sleeping until {until}")
        await self._record_row(inp, STATUS_SLEEPING, f"sleeping until {until}")
        while True:
            target = state.resume_at
            remaining = target - workflow.now().timestamp()
            if remaining <= 0:
                break
            try:
                await workflow.wait_condition(
                    lambda target=target: state.resume_at != target,
                    timeout=timedelta(seconds=remaining),
                )
            except TimeoutError:
                break
        state.resume_at = 0.0
        state.status = STATUS_RUNNING
        self._log("woke up")

    async def _notify(
        self,
        inp: MissionInput,
        view: GateView,
        event: str,
        *,
        decision: str = "",
        step: int = 0,
        by: str = "",
    ) -> None:
        """Record a gate event in the anchor + webhook (an activity); never fails the gate."""
        try:
            await workflow.execute_activity(
                notify_gate,
                GateNotice(
                    mission_id=inp.mission_id,
                    workdir=inp.workdir,
                    gate_id=view.gate_id,
                    kind=view.kind,
                    event=event,
                    question=view.question,
                    options=list(view.options),
                    default_action=view.default_action,
                    decision=decision,
                    step=step,
                    deadline=view.deadline,
                    request=view.request,
                    by=by,
                    at=_iso(workflow.now()),
                ),
                start_to_close_timeout=_NOTIFY_TIMEOUT,
                retry_policy=_SHORT_RETRY,
            )
        except ActivityError as err:
            if is_cancelled_exception(err):
                raise
            self._log(f"gate {view.gate_id}: notification failed: {err.cause or err}")

    def _take_decision(self, options: list[str]) -> str | None:
        """Consume the held ``human_decision``; invalid ones are recorded and discarded."""
        allowed = {opt.strip().lower(): opt for opt in options}
        state = self._state
        while state.pending_decision is not None:
            pending = state.pending_decision
            self._decision_by = state.pending_decision_by
            state.pending_decision = None  # consumed
            state.pending_decision_by = ""
            choice = allowed.get(pending.strip().lower())
            if choice is not None:
                return choice
            self._rejected_decisions.append(pending)
        return None

    async def _run_gate(
        self, inp: MissionInput, view: GateView, *, timeout_seconds: int
    ) -> tuple[str, bool]:
        """Open ``view`` (WAITING_ON_HUMAN) and walk the escalation ladder until a decision.

        Returns ``(decision, defaulted)``: a valid ``human_decision`` (held decisions count), or
        the default after reminders at each ``gate_escalation_seconds`` offset inside the timeout.
        """
        state = self._state
        previous = state.status
        state.status = STATUS_WAITING_ON_HUMAN
        timeout = max(1, timeout_seconds)
        opened = workflow.now()
        schedule = escalation_schedule(timeout, inp.gate_escalation_seconds)
        view.opened_at = _iso(opened)
        view.deadline = _iso(opened + timedelta(seconds=timeout))
        view.next_escalation_at = _iso(opened + timedelta(seconds=schedule[0])) if schedule else ""
        self._gate = view
        self._open_question = f"{view.question} [{' / '.join(view.options)}]"
        ready: Callable[[], bool] = lambda: state.pending_decision is not None  # noqa: E731
        self._log(f"{view.kind} gate {view.gate_id} opened (default {view.default_action})")
        await self._record_row(inp, STATUS_WAITING_ON_HUMAN, f"{view.kind} gate {view.gate_id}")
        await self._notify(inp, view, "opened")
        sent = 0
        try:
            while True:
                choice = self._take_decision(view.options)
                if choice is not None:
                    by = self._decision_by
                    suffix = f" by {by}" if by else ""
                    self._log(f"{view.kind} gate {view.gate_id} resolved: {choice}{suffix}")
                    await self._notify(inp, view, "resolved", decision=choice, by=by)
                    return choice, False
                elapsed = (workflow.now() - opened).total_seconds()
                rung = next_rung(elapsed, timeout, schedule, sent)
                if rung.wait_seconds > 0:
                    try:
                        await workflow.wait_condition(
                            ready, timeout=timedelta(seconds=rung.wait_seconds)
                        )
                        continue  # a decision arrived: validate it at the top
                    except TimeoutError:
                        pass
                if rung.step == 0:
                    default = view.default_action
                    self._log(f"{view.kind} gate {view.gate_id} timed out: default {default}")
                    await self._notify(inp, view, "defaulted", decision=default)
                    return default, True
                sent = rung.step
                state.escalations += 1
                view.escalations_sent = sent
                view.next_escalation_at = (
                    _iso(opened + timedelta(seconds=schedule[sent])) if sent < len(schedule) else ""
                )
                self._log(f"{view.kind} gate {view.gate_id}: reminder {sent} (escalation)")
                await self._notify(inp, view, "reminder", step=sent)
        finally:
            state.status = previous
            self._gate = None
            self._open_question = ""

    async def _on_deadlock(self, inp: MissionInput, result: CycleResult) -> tuple[str, str] | None:
        """``None`` to keep going (blocked items were retried), else ``(outcome, reason)``."""
        reason = result.reason or "no actionable item"
        if inp.deadlock_gate_seconds <= 0:
            return OUTCOME_DEADLOCKED, reason
        if not workflow.patched(PATCH_GATE_LADDER):
            if await self._resolve_deadlock(inp, result):
                return None
            return OUTCOME_DEADLOCKED, reason

        state = self._state
        recommended = ""
        if should_declare_impossible(
            consecutive_failures=state.fail_streak, threshold=inp.impossible_after_failures
        ):
            recommended = "impossible"
        default = inp.deadlock_gate_default.strip().lower()
        if default not in DEADLOCK_DEFAULTS:
            default = "abort"
        advice = (
            f" Recommended: impossible (item {state.fail_item} failed {state.fail_streak} times "
            "in a row)."
            if recommended
            else ""
        )
        decision, defaulted = await self._run_gate(
            inp,
            GateView(
                gate_id=f"deadlock-{state.cycles_done}",
                kind=GATE_DEADLOCK,
                question=(
                    f"Mission {inp.mission_id} is deadlocked ({reason}). Retry the blocked items, "
                    f"abort, or declare the mission impossible?{advice}"
                ),
                options=list(LADDER_DEADLOCK_OPTIONS),
                default_action=default,
                recommended=recommended,
            ),
            timeout_seconds=inp.deadlock_gate_seconds,
        )
        how = "by default (no human answered)" if defaulted else "by a human"
        if decision == "retry":
            if await self._unblock(inp):
                return None
            return OUTCOME_DEADLOCKED, reason
        if decision == "impossible":
            final = await workflow.execute_activity(
                declare_impossible,
                FinalizeInput(
                    mission_id=inp.mission_id,
                    workdir=inp.workdir,
                    cycle_id=f"impossible-{state.cycles_done}",
                    reason=f"declared impossible {how}: {reason}",
                ),
                start_to_close_timeout=timedelta(minutes=5),
                retry_policy=_SHORT_RETRY,
            )
            self._absorb(final)
            return OUTCOME_IMPOSSIBLE, f"declared impossible {how}: {reason}"
        return OUTCOME_ABORTED, f"aborted at the deadlock gate {how}: {reason}"

    async def _unblock(self, inp: MissionInput) -> bool:
        state = self._state
        state.deadlock_retries += 1
        unblocked = await workflow.execute_activity(
            unblock_items,
            UnblockInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=f"u{state.deadlock_retries}",
            ),
            start_to_close_timeout=timedelta(minutes=5),
            retry_policy=_SHORT_RETRY,
        )
        self._absorb(unblocked)
        state.fail_item, state.fail_streak = None, 0
        return unblocked.advanced

    async def _resolve_deadlock(self, inp: MissionInput, result: CycleResult) -> bool:
        """The pre-ladder deadlock gate (replays of older histories take this path)."""
        decision = await self.await_human_gate(
            question=f"Mission {inp.mission_id} is deadlocked ({result.reason}). Retry or abort?",
            options=list(DEADLOCK_OPTIONS),
            default_action="abort",
            timeout_seconds=inp.deadlock_gate_seconds,
        )
        if decision != "retry":
            return False
        return await self._unblock(inp)

    async def _record_row(self, inp: MissionInput, status: str, reason: str = "") -> None:
        """Write a status the workflow owns to the ``missions`` row (an activity; best effort).

        Never fails or blocks the mission beyond ``_ROW_DEADLINE``: an error is only logged.
        Guarded by ``PATCH_MISSION_ROW`` so histories recorded before it still replay.
        """
        if not workflow.patched(PATCH_MISSION_ROW):
            return
        try:
            await workflow.execute_activity(
                record_mission_status,
                MissionStatusInput(
                    mission_id=inp.mission_id,
                    workdir=inp.workdir,
                    status=status,
                    head_sha=self._state.head_sha or None,
                    reason=reason[:500],
                ),
                start_to_close_timeout=_ROW_TIMEOUT,
                schedule_to_close_timeout=_ROW_DEADLINE,
                retry_policy=_ROW_RETRY,
            )
        except ActivityError as err:
            if is_cancelled_exception(err):
                raise
            workflow.logger.warning("mission row not updated to %s: %s", status, err.cause or err)

    async def _terminal(self, inp: MissionInput, outcome: str, reason: str = "") -> MissionResult:
        state = self._state
        if not state.head_sha and not state.items_total:
            # No cycle reported yet in this mission: read the committed truth once.
            snap = await workflow.execute_activity(
                read_mission_snapshot,
                HealthInput(mission_id=inp.mission_id, workdir=inp.workdir),
                start_to_close_timeout=timedelta(minutes=2),
                retry_policy=_SHORT_RETRY,
            )
            self._absorb(snap)
        state.status = _TERMINAL_STATUS[outcome]
        await self._record_row(inp, state.status, f"{outcome}: {reason}" if reason else outcome)
        return MissionResult(
            mission_id=inp.mission_id,
            completed=outcome == OUTCOME_COMPLETED,
            cycles=state.cycles_done,
            head_sha=state.head_sha,
            items_done=state.items_done,
            items_total=state.items_total,
            outcome=outcome,
            reason=reason,
            status=state.status,
        )

    # --- queries ---------------------------------------------------------------------------
    @workflow.query
    def cycles_done(self) -> int:
        """Live count of completed cycles (works mid-run and after completion)."""
        return self._state.cycles_done

    @workflow.query
    def last_item(self) -> str | None:
        """The id of the most recently worked checklist item."""
        return self._state.last_item

    @workflow.query
    def park_reason(self) -> str:
        """Why the mission is parked ('' when not parked)."""
        return self._park_reason

    @workflow.query(name=QUERY_GATE)
    def gate(self) -> GateView | None:
        """The open gate: question, options, default, deadline, escalations, pending request."""
        return self._gate

    @workflow.query(name=QUERY_GATE_LOG)
    def gate_log(self) -> list[str]:
        """Recent gate / sleep events (opened, reminders, resolutions, defaults), oldest first."""
        return list(self._state.gate_log)

    @workflow.query
    def resume_at(self) -> float:
        """Epoch time the mission sleeps until (0 when not sleeping)."""
        return self._state.resume_at

    @workflow.query
    def open_question(self) -> str:
        """The question an open human gate is waiting on ('' when none is open)."""
        return self._open_question

    @workflow.query(name=QUERY_STEER_NOTES)
    def steer_notes(self) -> list[str]:
        """The operator's steering notes, oldest first (``lha mission-steer``)."""
        return list(self._state.steer_notes)

    @workflow.query
    def rejected_decisions(self) -> list[str]:
        """Human decisions that were discarded because they matched no offered option."""
        return list(self._rejected_decisions)

    # --- durable human-in-the-loop -------------------------------------------------------
    @workflow.query(name=QUERY_STATUS)
    def status(self) -> str:
        """Current status — lets a human distinguish DEGRADED_PARK from WAITING_ON_HUMAN."""
        return self._state.status

    @workflow.signal(name=SIGNAL_HUMAN_DECISION)
    def human_decision(self, decision: str) -> None:
        """Deliver a human decision. It is held until a gate consumes it (early signals are kept,
        and survive Continue-As-New); a gate only accepts one of the options it offered."""
        self._state.pending_decision = decision.strip()
        self._state.pending_decision_by = ""

    @workflow.signal(name=SIGNAL_HUMAN_DECISION_V2)
    def human_decision_v2(self, payload: dict[str, str]) -> None:
        """``human_decision`` with who made it (``{"decision", "by"}``; ``lha mission-approve
        --as``): the gate's ``resolved_by`` names them."""
        self._state.pending_decision = str(payload.get("decision", "")).strip()
        self._state.pending_decision_by = str(payload.get("by", "")).strip()[:200]

    @workflow.signal(name=SIGNAL_SNOOZE)
    def snooze(self, seconds: int) -> None:
        """Sleep (SLEEPING) before the next cycle for ``seconds``; ``0`` wakes a sleeping mission."""
        self._state.resume_at = workflow.now().timestamp() + seconds if seconds > 0 else 0.0

    @workflow.signal(name=SIGNAL_STEER)
    def steer(self, note: str) -> None:
        """Append an operator steering note; every following cycle's prompt includes it."""
        note = note.strip()[:MAX_STEER_CHARS]
        if note:
            self._state.steer_notes = [*self._state.steer_notes, note][-MAX_STEER_NOTES:]

    async def await_human_gate(
        self,
        *,
        question: str,
        options: list[str],
        default_action: str,
        timeout_seconds: int,
    ) -> str:
        """Park on a human decision (visibly WAITING_ON_HUMAN); apply the default on timeout.

        A decision that arrived BEFORE the gate opened is honored (it is only cleared when
        consumed). Decisions that match none of ``options`` (case-insensitive) are discarded —
        recorded in ``rejected_decisions`` — and the gate keeps waiting. Durable: survives
        crashes/restarts, and a queryable status means a parked-on-human gate is never mistaken
        for a healthy durable sleep.
        """
        allowed = {opt.strip().lower(): opt for opt in options}
        if default_action.strip().lower() not in allowed:
            raise ValueError(f"default_action {default_action!r} is not one of {options}")
        state = self._state
        previous = state.status
        state.status = STATUS_WAITING_ON_HUMAN
        self._open_question = f"{question} [{' / '.join(options)}]"
        deadline = workflow.now() + timedelta(seconds=timeout_seconds)
        try:
            while True:
                pending = state.pending_decision
                if pending is not None:
                    state.pending_decision = None  # consumed
                    state.pending_decision_by = ""
                    choice = allowed.get(pending.lower())
                    if choice is not None:
                        return choice
                    self._rejected_decisions.append(pending)
                    continue
                remaining = (deadline - workflow.now()).total_seconds()
                if remaining <= 0:
                    return allowed[default_action.strip().lower()]
                try:
                    await workflow.wait_condition(
                        lambda: state.pending_decision is not None,
                        timeout=timedelta(seconds=remaining),
                    )
                except TimeoutError:
                    return allowed[default_action.strip().lower()]
        finally:
            state.status = previous
            self._open_question = ""
