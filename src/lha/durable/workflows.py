"""The durable control plane: ``MissionWorkflow``.

A deterministic scheduler — NO LLM/tool/IO in this body. It repeatedly dispatches one
``run_agent_cycle`` activity and ends with an explicit outcome:

  * ``completed``         — every checklist item verified done;
  * ``deadlocked``        — items remain but none is actionable (blocked / unsatisfiable deps),
                            optionally after a human "retry"/"abort" gate;
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
"""

from __future__ import annotations

import dataclasses
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ActivityError, ApplicationError

# The activity module pulls in git/model/etc.; pass it through the workflow sandbox unchanged
# rather than letting the sandbox re-import (and reject) its non-deterministic dependencies.
with workflow.unsafe.imports_passed_through():
    from lha.durable.activities import (
        check_mission_health,
        read_mission_snapshot,
        run_agent_cycle,
        unblock_items,
    )

from lha.durable.signals import (
    QUERY_STATUS,
    SIGNAL_HUMAN_DECISION,
    SIGNAL_STEER,
    STATUS_ABORTED,
    STATUS_DEGRADED_PARK,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_RUNNING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    OUTCOME_ABORTED,
    OUTCOME_BUDGET_EXHAUSTED,
    OUTCOME_COMPLETED,
    OUTCOME_DEADLOCKED,
    OUTCOME_MAX_CYCLES,
    CycleInput,
    CycleResult,
    HealthInput,
    MissionInput,
    MissionResult,
    MissionState,
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

MAX_STEER_NOTES = 20
MAX_STEER_CHARS = 2000
DEADLOCK_OPTIONS = ("retry", "abort")

_TERMINAL_STATUS = {
    OUTCOME_COMPLETED: STATUS_DONE,
    OUTCOME_DEADLOCKED: STATUS_IMPOSSIBLE,
    OUTCOME_BUDGET_EXHAUSTED: STATUS_ABORTED,
    OUTCOME_MAX_CYCLES: STATUS_ABORTED,
    OUTCOME_ABORTED: STATUS_ABORTED,
}


def _application_cause(err: ActivityError) -> ApplicationError | None:
    cause = err.cause
    return cause if isinstance(cause, ApplicationError) else None


def _config_failure(message: str) -> ApplicationError:
    return ApplicationError(message, type=ERROR_CONFIG, non_retryable=True)


@workflow.defn
class MissionWorkflow:
    """Long-lived orchestrator for one mission."""

    def __init__(self) -> None:
        # Signals can arrive before ``run`` starts; they land here and are merged into the
        # carried state at the top of ``run``.
        self._state = MissionState()
        self._park_reason = ""
        self._rejected_decisions: list[str] = []

    @workflow.run
    async def run(self, inp: MissionInput) -> MissionResult:
        early = self._state
        state = dataclasses.replace(inp.state) if inp.state is not None else MissionState()
        if early.pending_decision is not None:
            state.pending_decision = early.pending_decision
        state.steer_notes = [*state.steer_notes, *early.steer_notes][-MAX_STEER_NOTES:]
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

        while True:
            if state.cycles_done >= inp.max_cycles:
                return await self._terminal(inp, OUTCOME_MAX_CYCLES, "iteration ceiling reached")

            state.status = STATUS_RUNNING
            try:
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
                    ),
                    start_to_close_timeout=CYCLE_START_TO_CLOSE,
                    heartbeat_timeout=CYCLE_HEARTBEAT_TIMEOUT,
                    retry_policy=_CYCLE_RETRY,
                )
            except ActivityError as err:
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

            state.cycles_done += 1
            self._absorb(result)

            if result.is_complete:
                return await self._terminal(inp, OUTCOME_COMPLETED)
            if result.is_deadlocked:
                if await self._resolve_deadlock(inp, result):
                    continue
                return await self._terminal(
                    inp, OUTCOME_DEADLOCKED, result.reason or "no actionable item"
                )

            if state.cycles_done % inp.cycles_before_can == 0:
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

    def _maybe_continue_as_new(self, inp: MissionInput) -> None:
        if workflow.info().is_continue_as_new_suggested():
            workflow.continue_as_new(dataclasses.replace(inp, state=self._state))

    async def _park(self, inp: MissionInput, *, reason: str) -> None:
        """Durably sleep with capped exponential backoff until dependencies are healthy again."""
        state = self._state
        state.status = STATUS_DEGRADED_PARK
        state.parks += 1
        self._park_reason = reason
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
                self._park_reason = f"health check failed: {err.cause or err}"
            delay = min(delay * 2, cap)
            self._maybe_continue_as_new(inp)
        state.status = STATUS_RUNNING
        self._park_reason = ""

    async def _resolve_deadlock(self, inp: MissionInput, result: CycleResult) -> bool:
        """Optionally ask a human; True if blocked items were reset and the mission continues."""
        if inp.deadlock_gate_seconds <= 0:
            return False
        decision = await self.await_human_gate(
            question=f"Mission {inp.mission_id} is deadlocked ({result.reason}). Retry or abort?",
            options=list(DEADLOCK_OPTIONS),
            default_action="abort",
            timeout_seconds=inp.deadlock_gate_seconds,
        )
        if decision != "retry":
            return False
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
        return unblocked.advanced

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
        deadline = workflow.now() + timedelta(seconds=timeout_seconds)
        try:
            while True:
                pending = state.pending_decision
                if pending is not None:
                    state.pending_decision = None  # consumed
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
