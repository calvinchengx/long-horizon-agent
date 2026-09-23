"""Temporal activities — where all the non-deterministic work happens.

The cycle activity runs the REAL integrated ``AgentLoop`` (model → tools-in-sandbox → deterministic
verify → git checkpoint) inside the configured sandbox, with every model call metered against the
mission budget. Temporal journals each activity's *result*: a completed cycle is never re-run on
replay. Work inside an activity attempt that crashes is NOT journaled — the attempt is retried from
scratch (its model calls are re-spent), so each attempt is made safe to repeat:

  * **Exclusive**: a per-workdir file lock (under ``.git/``) means two attempts (e.g. a zombie
    attempt after a heartbeat timeout and its retry) can never mutate the same checkout at once.
  * **Clean start**: every attempt first resets the checkout to ``HEAD`` (``reset --hard`` +
    ``clean -ffdx``), so partial edits from a crashed attempt are discarded, never committed.
  * **Exactly-once commit per cycle id**: if ``HEAD`` already carries this cycle's checkpoint (the
    previous attempt committed, then crashed before reporting), the attempt returns that result
    instead of advancing another item.
  * **Heartbeats** every few seconds while the cycle runs, so a dead worker is detected by the
    activity's ``heartbeat_timeout`` instead of its (long) start-to-close timeout.

Budget: spend of every attempt is appended to a journal under ``.git/lha/`` (outside the
worktree, so the reset keeps it) and seeds the next attempt's ledger — the governor sees the whole
mission's spend, including failed attempts. ``BudgetExceeded`` and configuration errors are raised
as NON-retryable ``ApplicationError``\\ s (types ``ERROR_BUDGET_EXCEEDED`` / ``ERROR_CONFIG``).
"""

from __future__ import annotations

import asyncio
import contextlib
import fcntl
import json
import os
from collections.abc import AsyncIterator, Awaitable, Callable
from pathlib import Path

import httpx
from temporalio import activity
from temporalio.exceptions import ApplicationError

from lha.agent.assembly import build_lead_loop, open_lead_sandbox
from lha.config import Settings, get_settings
from lha.contracts.model import ModelProvider
from lha.contracts.state import Checkpoint, EventRecord, SituationSnapshot
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check, checks_from_commands
from lha.coordination.decision_log import DecisionChainError
from lha.durable.signals import (
    STATUS_ABORTED,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_RUNNING,
    STATUS_WAITING_ON_HUMAN,
)
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    CycleInput,
    CycleResult,
    FinalizeInput,
    GateNotice,
    HealthInput,
    HealthReport,
    NoticeResult,
    PendingApproval,
    UnblockInput,
)
from lha.execution import UnsafeSandboxError
from lha.governor.cost import CostEntry, CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.hitl.approvals import DeferredApprovalGate
from lha.hitl.notify import post_webhook
from lha.ids import idempotency_key
from lha.model import build_provider
from lha.model.health import probe_model
from lha.obs.redact import redact_text
from lha.ops.degradation import DependencyStatus, Health, decide_safe_park
from lha.persistence.services import open_run_services
from lha.persistence.store import StoreUnavailableError
from lha.state import git_ops
from lha.state.mission_anchor import ANCHOR_DIR, EVENTS_FILE, GitMissionAnchor
from lha.verify.verifier import default_python_checks

#: Builds the lead model for one cycle (tests inject scripted models here).
ModelFactory = Callable[[Settings, SituationSnapshot], ModelProvider]

HEARTBEAT_EVERY_S = 5.0
LOCK_WAIT_S = 300.0
_LOCK_POLL_S = 0.5
_LOCK_FILE = "lha-cycle.lock"
_SPEND_FILE = "lha/spend.ndjson"
# How many trailing committed events to scan for an already-committed cycle id.
_RECENT_EVENTS = 64


class WorkdirBusyError(RuntimeError):
    """Another attempt holds the workdir lock (retryable)."""


def _default_model_factory(settings: Settings, _snapshot: SituationSnapshot) -> ModelProvider:
    return build_provider(settings)


def _config_error(message: str, exc: BaseException | None = None) -> ApplicationError:
    err = ApplicationError(message, type=ERROR_CONFIG, non_retryable=True)
    if exc is not None:
        err.__cause__ = exc
    return err


def resolve_checks(check_commands: list[list[str]] | None) -> list[Check]:
    """``None`` → the default Python gate; an explicit empty list is a configuration error."""
    if check_commands is None:
        return default_python_checks()
    commands = [cmd for cmd in check_commands if cmd]
    if not commands:
        raise _config_error(
            "check_commands is empty: at least one gating check is required (an item is only "
            "marked done by a passing check); omit it to use the default Python checks"
        )
    return checks_from_commands(commands)


def _heartbeat(*details: object) -> None:
    if activity.in_activity():
        activity.heartbeat(*details)


async def _with_heartbeat[T](awaitable: Awaitable[T], detail: str) -> T:
    """Await ``awaitable`` while heartbeating every ``HEARTBEAT_EVERY_S`` seconds."""

    async def _beat() -> None:
        while True:
            _heartbeat(detail)
            await asyncio.sleep(HEARTBEAT_EVERY_S)

    beater = asyncio.create_task(_beat())
    try:
        return await awaitable
    finally:
        beater.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await beater


@contextlib.asynccontextmanager
async def workdir_lock(workdir: str, *, wait_s: float = LOCK_WAIT_S) -> AsyncIterator[None]:
    """Exclusive, per-checkout lock (``flock`` on a file in ``.git/``), heartbeating while waiting."""
    path = await asyncio.to_thread(git_ops.git_dir, workdir) / _LOCK_FILE
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o644)
    try:
        waited = 0.0
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if waited >= wait_s:
                    raise WorkdirBusyError(
                        f"workdir {workdir} is locked by another cycle attempt"
                    ) from None
                _heartbeat("waiting for workdir lock")
                await asyncio.sleep(_LOCK_POLL_S)
                waited += _LOCK_POLL_S
        try:
            yield
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)


# --- spend journal (outside the worktree; survives reset_to_head) --------------------------
def _spend_path(workdir: str) -> Path:
    return git_ops.git_dir(workdir) / _SPEND_FILE


def read_prior_spend(workdir: str) -> tuple[float, int]:
    """(known USD, unknown-cost call count) over every recorded attempt of this mission."""
    path = _spend_path(workdir)
    if not path.exists():
        return 0.0, 0
    by_key: dict[str, tuple[float, int]] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        try:
            row = json.loads(line)
            by_key[str(row["key"])] = (float(row["usd"]), int(row["unknown"]))
        except (ValueError, KeyError, TypeError):
            continue  # a torn last line from a crash mid-append
    return sum(v[0] for v in by_key.values()), sum(v[1] for v in by_key.values())


def record_spend(workdir: str, *, key: str, cycle_id: str, ledger: CostLedger) -> None:
    """Append one attempt's spend (idempotent per ``key``: readers keep the last row per key)."""
    own = [e for e in ledger.entries if e.cycle_id != _PRIOR_CYCLE]
    if not own:
        return
    path = _spend_path(workdir)
    path.parent.mkdir(parents=True, exist_ok=True)
    row = {
        "key": key,
        "cycle_id": cycle_id,
        "usd": sum(e.usd for e in own),
        "unknown": sum(1 for e in own if not e.cost_known),
        "calls": len(own),
    }
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(row) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


_PRIOR_CYCLE = "(prior)"


def build_cycle_meter(settings: Settings, inp: CycleInput) -> CostMeter:
    """A meter whose ledger is seeded with the mission's prior spend and whose ceiling is the
    mission budget (``inp.budget_usd`` or the worker's ``budget_usd_ceiling``)."""
    ledger = CostLedger()
    prior_usd, prior_unknown = read_prior_spend(inp.workdir)
    if prior_usd:
        ledger.entries.append(
            CostEntry(
                cycle_id=_PRIOR_CYCLE,
                model="(prior)",
                input_tokens=0,
                output_tokens=0,
                usd=prior_usd,
            )
        )
    for _ in range(prior_unknown):
        ledger.entries.append(
            CostEntry(
                cycle_id=_PRIOR_CYCLE,
                model="(prior)",
                input_tokens=0,
                output_tokens=0,
                usd=0.0,
                cost_known=False,
            )
        )
    governor = BudgetGovernor(
        ceiling_usd=inp.budget_usd if inp.budget_usd is not None else settings.budget_usd_ceiling,
        max_cycles=inp.max_cycles,
        allow_unknown_cost=settings.allow_unpriced_models,
    )
    meter = CostMeter(ledger=ledger, governor=governor)
    meter.cycle_id = inp.cycle_id
    return meter


# --- exactly-once per cycle id ------------------------------------------------------------
def committed_cycle_event(workdir: str, cycle_id: str) -> dict[str, object] | None:
    """The payload of ``cycle_id``'s checkpoint event if ``HEAD`` already contains it."""
    rel = f"{ANCHOR_DIR}/{EVENTS_FILE}"
    try:
        raw = git_ops.run_git(workdir, "show", f"HEAD:{rel}")
    except git_ops.GitError:
        return None
    for line in reversed(raw.splitlines()[-_RECENT_EVENTS:]):
        try:
            event = EventRecord.model_validate_json(line)
        except ValueError:
            continue
        if event.kind == "cycle" and event.cycle_id == cycle_id:
            return event.payload
    return None


def _result_from_snapshot(
    snapshot: SituationSnapshot,
    *,
    item_id: str | None,
    advanced: bool,
    note: str,
    verdict: str = "",
    item_blocked: bool = False,
    reason: str = "",
    spent_usd: float = 0.0,
    item_split: bool = False,
    pending_approvals: list[PendingApproval] | None = None,
    used_approvals: list[str] | None = None,
) -> CycleResult:
    return CycleResult(
        item_id=item_id,
        advanced=advanced,
        head_sha=snapshot.head_sha,
        is_complete=snapshot.is_complete,
        items_done=snapshot.items_done,
        items_total=snapshot.items_total,
        note=note,
        verdict=verdict,
        is_deadlocked=snapshot.is_deadlocked,
        item_blocked=item_blocked,
        reason=reason or snapshot.deadlock_reason,
        spent_usd=spent_usd,
        item_split=item_split,
        pending_approvals=list(pending_approvals or []),
        used_approvals=list(used_approvals or []),
    )


def _anchor_text(snapshot: SituationSnapshot, inp: CycleInput) -> str:
    """Extra prompt context: operator steering (the loop recites the mission spec itself)."""
    parts = [] if snapshot.mission else [f"Mission {inp.mission_id}"]
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


async def _execute_cycle(
    inp: CycleInput,
    *,
    settings: Settings | None = None,
    model_factory: ModelFactory | None = None,
) -> CycleResult:
    """Advance the mission by one verified item via the real agent loop (safe to retry)."""
    settings = settings or get_settings()
    factory = model_factory or _default_model_factory
    checks = resolve_checks(inp.check_commands)
    attempt = activity.info().attempt if activity.in_activity() else 1

    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(git_ops.reset_to_head, inp.workdir)
        anchor = GitMissionAnchor(inp.workdir)
        snapshot = await anchor.read_situational_awareness()

        payload = await asyncio.to_thread(committed_cycle_event, inp.workdir, inp.cycle_id)
        if payload is not None:  # a previous attempt committed this cycle, then crashed
            item = payload.get("item_id")
            return _result_from_snapshot(
                snapshot,
                item_id=str(item) if item is not None else None,
                advanced=True,
                verdict=str(payload.get("verdict", "")),
                item_blocked=payload.get("status") == "blocked",
                note="already committed by a previous attempt",
            )
        if snapshot.is_complete or snapshot.is_deadlocked:
            return _result_from_snapshot(
                snapshot, item_id=None, advanced=False, note="nothing actionable"
            )

        meter = await asyncio.to_thread(build_cycle_meter, settings, inp)
        try:
            model = meter.wrap(factory(settings, snapshot), role="lead")
        except ValueError as exc:  # e.g. missing API key / base URL for the configured backend
            raise _config_error(f"cannot build the model: {exc}", exc) from exc
        try:
            session = await open_lead_sandbox(settings, inp.workdir)
        except (UnsafeSandboxError, ValueError) as exc:
            await model.aclose()
            raise _config_error(f"cannot open the sandbox: {exc}", exc) from exc

        # Persistence + memory (activity-side only; the workflow never touches a database).
        # Ledger keys carry the attempt, so a retried attempt's re-spent calls are new rows
        # while a replayed write of the same call is a no-op. The seeded "(prior)" entries are
        # already in the ledger from earlier attempts: no backfill.
        try:
            services = await open_run_services(
                settings,
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                meter=meter,
                title=snapshot.mission.title if snapshot.mission else "",
                description=snapshot.mission.description if snapshot.mission else "",
                model=meter.wrap(model.inner, role="librarian"),
                key_prefix=f"{inp.cycle_id}@{attempt}",
                backfill=False,
                workflow_id=activity.info().workflow_id if activity.in_activity() else None,
            )
        except BaseException as exc:
            await session.close()
            await model.aclose()
            if not isinstance(exc, StoreUnavailableError):
                raise  # e.g. an unwritable SQLite path: retryable
            raise _config_error(f"cannot open the mission store: {exc}", exc) from exc

        gate = DeferredApprovalGate(approved=[a.fingerprint for a in inp.approved_actions])
        try:
            await services.tracker.running(head_sha=snapshot.head_sha)
            try:
                try:
                    loop = build_lead_loop(
                        settings,
                        model=model,
                        anchor=anchor,
                        workdir=inp.workdir,
                        gate=gate,
                        memory=services.memory,
                    )
                except ValueError as exc:  # e.g. malformed LHA_TRUSTED_CHECKS
                    raise _config_error(f"invalid configuration: {exc}", exc) from exc
                outcome = await _with_heartbeat(
                    loop.run_cycle(
                        ctx=ToolContext(mission_id=inp.mission_id, session=session),
                        mission_id=inp.mission_id,
                        cycle_id=inp.cycle_id,
                        anchor_text=_anchor_text(snapshot, inp),
                        checks=checks,
                    ),
                    inp.cycle_id,
                )
            except BudgetExceeded as exc:
                await services.tracker.set_status(STATUS_ABORTED)
                raise ApplicationError(
                    str(exc), type=ERROR_BUDGET_EXCEEDED, non_retryable=True
                ) from exc
            finally:
                await session.close()
                await model.aclose()
                await asyncio.to_thread(
                    record_spend,
                    inp.workdir,
                    key=idempotency_key(inp.mission_id, inp.cycle_id, attempt),
                    cycle_id=inp.cycle_id,
                    ledger=meter.ledger,
                )

            after = await anchor.read_situational_awareness()
            await services.tracker.set_status(
                cycle_status(after, awaiting_approval=bool(gate.pending)),
                head_sha=after.head_sha,
            )
        finally:
            await services.close()
        return _result_from_snapshot(
            after,
            item_id=outcome.item_id,
            advanced=outcome.advanced,
            verdict=outcome.verdict,
            item_blocked=outcome.item_blocked,
            reason=outcome.reason,
            note=f"verdict={outcome.verdict} tools={outcome.tool_calls} turns={outcome.turns}",
            spent_usd=sum(e.usd for e in meter.ledger.entries if e.cycle_id == inp.cycle_id),
            item_split=outcome.item_split,
            pending_approvals=[PendingApproval(**p) for p in gate.pending],
            used_approvals=list(gate.used),
        )


def cycle_status(after: SituationSnapshot, *, awaiting_approval: bool = False) -> str:
    """``missions.status`` after a cycle, from the committed truth (activity-side).

    complete → DONE; the cycle asked for approval of an irreversible action → WAITING_ON_HUMAN
    (the workflow now asks a human); deadlocked → IMPOSSIBLE (a human "retry" makes the next cycle
    RUNNING again); otherwise (including an item split by the replanner) → RUNNING. The
    workflow's own parks are not visible here.
    """
    if after.is_complete:
        return STATUS_DONE
    if awaiting_approval:
        return STATUS_WAITING_ON_HUMAN
    if after.is_deadlocked:
        return STATUS_IMPOSSIBLE
    return STATUS_RUNNING


def make_cycle_activity(
    *, settings: Settings | None = None, model_factory: ModelFactory | None = None
) -> Callable[[CycleInput], Awaitable[CycleResult]]:
    """A ``run_agent_cycle`` activity bound to explicit settings / model factory (tests, embeds)."""

    @activity.defn(name="run_agent_cycle")
    async def run_agent_cycle_bound(inp: CycleInput) -> CycleResult:
        return await _refuse_tampered_chain(
            _execute_cycle(inp, settings=settings, model_factory=model_factory)
        )

    return run_agent_cycle_bound


@activity.defn
async def run_agent_cycle(inp: CycleInput) -> CycleResult:
    """Activity wrapper around one retry-safe agent cycle (worker settings, configured model)."""
    return await _refuse_tampered_chain(_execute_cycle(inp))


async def _refuse_tampered_chain(cycle: Awaitable[CycleResult]) -> CycleResult:
    """An altered ``.lha/decisions.ndjson`` is not transient: retrying cannot fix it, so fail the
    mission with a non-retryable ``ERROR_CONFIG`` (as the local runners stop) instead of retrying
    and parking."""
    try:
        return await cycle
    except DecisionChainError as exc:
        raise _config_error(f"decision log failed verification: {exc}", exc) from exc


# --- health probe (used while parked) -----------------------------------------------------
async def probe_health(inp: HealthInput, *, settings: Settings | None = None) -> HealthReport:
    """Probe the critical dependencies (git checkout, a real model round trip, sandbox)."""
    settings = settings or get_settings()
    statuses: list[DependencyStatus] = []

    def _git_ok() -> bool:
        try:
            return git_ops.is_repo(inp.workdir) and git_ops.has_commits(inp.workdir)
        except (OSError, git_ops.GitError):
            return False

    ok_repo = await asyncio.to_thread(_git_ok)
    statuses.append(
        DependencyStatus(
            "git", Health.OK if ok_repo else Health.DOWN, "" if ok_repo else "no usable repo"
        )
    )
    # The model is CONTACTED (cheap list-models / tags request, tight timeout), not just built:
    # an outage or a revoked key keeps the mission parked instead of resuming into failures.
    model = await probe_model(settings)
    statuses.append(
        DependencyStatus(
            "model", Health.OK if model.ok else Health.DOWN, "" if model.ok else model.detail
        )
    )
    try:
        session = await open_lead_sandbox(settings, inp.workdir)
        await session.close()
        statuses.append(DependencyStatus("sandbox", Health.OK))
    except Exception as exc:
        statuses.append(DependencyStatus("sandbox", Health.DOWN, f"{type(exc).__name__}: {exc}"))

    decision = decide_safe_park(statuses)
    detail = "; ".join(f"{s.name}: {s.detail}" for s in statuses if s.detail)
    return HealthReport(
        healthy=not decision.park,
        reason=decision.reason + (f" ({detail})" if detail else ""),
        degraded=decision.degraded,
    )


@activity.defn
async def check_mission_health(inp: HealthInput) -> HealthReport:
    """Activity: are the mission's critical dependencies up again?"""
    return await probe_health(inp)


# --- human gates: anchor events + webhook, final "impossible" checkpoint -------------------
def gate_notice_payload(notice: GateNotice) -> dict[str, object]:
    """The JSON a gate event is recorded / POSTed as (secrets in argv/question redacted)."""
    payload: dict[str, object] = {
        "source": "lha",
        "mission_id": notice.mission_id,
        "gate_id": notice.gate_id,
        "kind": notice.kind,
        "event": notice.event,
        "question": redact_text(notice.question),
        "options": list(notice.options),
        "default_action": notice.default_action,
        "deadline": notice.deadline,
    }
    if notice.decision:
        payload["decision"] = notice.decision
    if notice.step:
        payload["step"] = notice.step
    if notice.request is not None:
        payload["request"] = {
            "fingerprint": notice.request.fingerprint,
            "tool": notice.request.tool,
            "arguments": redact_text(notice.request.arguments),
            "reason": notice.request.reason,
        }
    return payload


async def _record_gate_event(notice: GateNotice, payload: dict[str, object]) -> bool:
    """Commit the gate event to the anchor's event log (no cycle runs while a gate is open, and
    the reset only discards the partial work of a cycle that ended waiting for approval)."""
    try:
        async with workdir_lock(notice.workdir, wait_s=60.0):
            await asyncio.to_thread(git_ops.reset_to_head, notice.workdir)
            anchor = GitMissionAnchor(notice.workdir)
            checklist = await anchor.read_checklist()
            await anchor.commit_checkpoint(
                Checkpoint(
                    cycle_id=f"gate:{notice.gate_id}",
                    progress_summary="",
                    checklist=checklist,
                    events=[
                        EventRecord(
                            kind=f"gate_{notice.event}",
                            cycle_id=f"gate:{notice.gate_id}",
                            payload=payload,
                        )
                    ],
                    commit_message=f"lha: gate {notice.event} ({notice.kind} {notice.gate_id})",
                )
            )
        return True
    except (WorkdirBusyError, git_ops.GitError, OSError, ValueError):
        return False


async def _notify_gate(
    notice: GateNotice,
    *,
    settings: Settings | None = None,
    transport: httpx.AsyncBaseTransport | None = None,
) -> NoticeResult:
    settings = settings or get_settings()
    payload = gate_notice_payload(notice)
    recorded = await _record_gate_event(notice, payload)
    url = settings.gate_webhook_url.get_secret_value() if settings.gate_webhook_url else None
    webhook = await post_webhook(
        url, payload, timeout=settings.gate_webhook_timeout_seconds, transport=transport
    )
    return NoticeResult(recorded=recorded, webhook=webhook)


def make_notify_activity(
    *, settings: Settings | None = None, transport: httpx.AsyncBaseTransport | None = None
) -> Callable[[GateNotice], Awaitable[NoticeResult]]:
    """A ``notify_gate`` activity bound to explicit settings / HTTP transport (tests, embeds)."""

    @activity.defn(name="notify_gate")
    async def notify_gate_bound(notice: GateNotice) -> NoticeResult:
        return await _notify_gate(notice, settings=settings, transport=transport)

    return notify_gate_bound


@activity.defn
async def notify_gate(notice: GateNotice) -> NoticeResult:
    """Activity: record a gate event in the anchor + POST it to the optional webhook.

    A notification problem never fails the gate: both outcomes are reported instead.
    """
    return await _notify_gate(notice)


async def _declare_impossible(inp: FinalizeInput) -> CycleResult:
    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(git_ops.reset_to_head, inp.workdir)
        anchor = GitMissionAnchor(inp.workdir)
        checklist = await anchor.read_checklist()
        blocked = [item.id for item in checklist.blocked_items]
        if await asyncio.to_thread(committed_cycle_event, inp.workdir, inp.cycle_id) is None:
            await anchor.commit_checkpoint(
                Checkpoint(
                    cycle_id=inp.cycle_id,
                    progress_summary=f"- {inp.cycle_id} mission declared IMPOSSIBLE: {inp.reason}",
                    checklist=checklist,
                    events=[
                        EventRecord(
                            kind="mission_impossible",
                            cycle_id=inp.cycle_id,
                            payload={
                                "reason": inp.reason,
                                "blocked": blocked,
                                "items_done": checklist.items_done,
                                "items_total": len(checklist.items),
                            },
                        ),
                        # Marks the final checkpoint as done for this id (a retry is a no-op).
                        EventRecord(
                            kind="cycle",
                            cycle_id=inp.cycle_id,
                            payload={"outcome": "impossible", "blocked": blocked},
                        ),
                    ],
                    commit_message="lha: mission declared impossible",
                )
            )
        snapshot = await anchor.read_situational_awareness()
        return _result_from_snapshot(
            snapshot, item_id=None, advanced=False, note="declared impossible", reason=inp.reason
        )


@activity.defn
async def declare_impossible(inp: FinalizeInput) -> CycleResult:
    """Activity: the final checkpoint of a mission declared impossible (idempotent per id)."""
    return await _declare_impossible(inp)


# --- human-approved retry of blocked items ------------------------------------------------
async def _unblock(inp: UnblockInput) -> CycleResult:
    async with workdir_lock(inp.workdir):
        await asyncio.to_thread(git_ops.reset_to_head, inp.workdir)
        anchor = GitMissionAnchor(inp.workdir)
        checklist = await anchor.read_checklist()
        blocked = [item.id for item in checklist.blocked_items]
        for item_id in blocked:
            checklist.unblock(item_id)
        if blocked:
            await anchor.commit_checkpoint(
                Checkpoint(
                    cycle_id=inp.cycle_id,
                    progress_summary=f"- {inp.cycle_id} human retry: unblocked {', '.join(blocked)}",
                    checklist=checklist,
                    events=[
                        EventRecord(
                            kind="unblock", cycle_id=inp.cycle_id, payload={"items": blocked}
                        )
                    ],
                    commit_message=f"lha: unblock {', '.join(blocked)} (human retry)",
                )
            )
        snapshot = await anchor.read_situational_awareness()
        return _result_from_snapshot(
            snapshot, item_id=None, advanced=bool(blocked), note=f"unblocked {blocked}"
        )


@activity.defn
async def unblock_items(inp: UnblockInput) -> CycleResult:
    """Activity: reset every ``blocked`` item to retryable (a human chose "retry")."""
    return await _unblock(inp)


# --- read-only snapshot (terminal summaries) ----------------------------------------------
async def _read_snapshot(inp: HealthInput) -> CycleResult:
    snapshot = await GitMissionAnchor(inp.workdir).read_situational_awareness()
    return _result_from_snapshot(snapshot, item_id=None, advanced=False, note="snapshot")


@activity.defn
async def read_mission_snapshot(inp: HealthInput) -> CycleResult:
    """Activity: the committed checklist counts + HEAD sha (read-only, no reset)."""
    return await _read_snapshot(inp)
