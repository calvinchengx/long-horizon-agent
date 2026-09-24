"""Serializable types that cross the Temporal boundary.

Plain dataclasses with primitive fields only, so Temporal's default JSON converter handles them
and the workflow sandbox can import this module with zero non-determinism. Rich pydantic models
(Checklist, etc.) live in the durable git anchor and are read/written *inside activities*, never
passed through workflow code.

Workflow ``run`` methods take exactly ONE argument: Temporal only applies type hints when the
number of payloads equals the number of parameters, so an optional second parameter silently
turns the input into a ``dict``. State carried across Continue-As-New therefore rides inside
``MissionInput.state``.
"""

from __future__ import annotations

from dataclasses import dataclass, field

# Mission outcomes reported in ``MissionResult.outcome``.
OUTCOME_COMPLETED = "completed"
OUTCOME_DEADLOCKED = "deadlocked"
OUTCOME_BUDGET_EXHAUSTED = "budget_exhausted"
OUTCOME_MAX_CYCLES = "max_cycles"
OUTCOME_ABORTED = "aborted"
# A human (or the deadlock gate's default) declared the remaining work impossible.
OUTCOME_IMPOSSIBLE = "impossible"

# ApplicationError ``type`` values raised (non-retryable) by the cycle activity.
ERROR_BUDGET_EXCEEDED = "BudgetExceeded"
ERROR_CONFIG = "MissionConfigError"


@dataclass
class PendingApproval:
    """An irreversible tool call the agent attempted that needs a human decision."""

    fingerprint: str
    tool: str
    reason: str
    arguments: str = ""


@dataclass
class ApprovedAction:
    """A human-approved tool call (by fingerprint), allowed once in a later cycle."""

    fingerprint: str
    summary: str


@dataclass
class GateView:
    """The open human gate (``gate`` query; ``lha mission-status``)."""

    gate_id: str
    kind: str  # "tool_call" | "deadlock"
    question: str
    options: list[str]
    default_action: str
    opened_at: str = ""
    deadline: str = ""
    escalations_sent: int = 0
    next_escalation_at: str = ""
    recommended: str = ""
    request: PendingApproval | None = None


@dataclass
class GateNotice:
    """Input of the ``notify_gate`` activity: one gate event (anchor event, ``hitl_gates`` row,
    optional webhook)."""

    mission_id: str
    workdir: str
    gate_id: str
    kind: str
    event: str  # "opened" | "reminder" | "resolved" | "defaulted"
    question: str = ""
    options: list[str] = field(default_factory=list)
    default_action: str = ""
    decision: str = ""
    step: int = 0
    deadline: str = ""
    request: PendingApproval | None = None
    # When the event happened, in workflow time (ISO-8601 UTC); "" from workflows built before
    # it existed (the activity then uses its own clock).
    at: str = ""


@dataclass
class NoticeResult:
    recorded: bool
    webhook: str = "off"  # "off" | "sent" | "failed: <reason>"
    stored: bool = False  # written to the mission store's ``hitl_gates`` table


@dataclass
class FinalizeInput:
    """Final checkpoint of a mission declared impossible."""

    mission_id: str
    workdir: str
    cycle_id: str
    reason: str = ""


@dataclass
class MissionStatusInput:
    """A ``missions`` row status the workflow decided (written by ``record_mission_status``)."""

    mission_id: str
    workdir: str
    status: str
    head_sha: str | None = None
    reason: str = ""


@dataclass
class MissionState:
    """State carried across Continue-As-New (pointers + small counters only — never history)."""

    cycles_done: int = 0
    status: str = "RUNNING"
    # Last committed truth reported by a cycle (so terminal results never report 0/0 or '').
    head_sha: str = ""
    items_done: int = 0
    items_total: int = 0
    last_item: str | None = None
    # Human-in-the-loop: a decision signalled but not yet consumed by a gate.
    pending_decision: str | None = None
    # Operator steering notes (``steer`` signal); every following cycle's prompt includes them.
    steer_notes: list[str] = field(default_factory=list)
    # How many times the mission parked on a degraded dependency (observability).
    parks: int = 0
    # How many times a human chose to retry blocked items after a deadlock.
    deadlock_retries: int = 0
    # Human-approved irreversible actions not yet used, and fingerprints a human rejected.
    approved_actions: list[ApprovedAction] = field(default_factory=list)
    rejected_actions: list[str] = field(default_factory=list)
    # Consecutive non-passing cycles on ``fail_item`` (drives the "declare impossible?" advice).
    fail_item: str | None = None
    fail_streak: int = 0
    # Durable sleep: no cycle starts before this epoch time (``snooze`` signal, scheduled start,
    # pause between cycles); 0 = none.
    resume_at: float = 0.0
    # Escalation ladder: reminders sent over the mission's life + a bounded gate/sleep event log.
    escalations: int = 0
    gate_log: list[str] = field(default_factory=list)


@dataclass
class MissionInput:
    """Starts a mission; the anchor + checklist must already be initialized at ``workdir``."""

    mission_id: str
    workdir: str
    max_cycles: int = 1000
    # Continue-As-New after this many cycles, to keep Temporal event history bounded over weeks.
    # Must be >= 1.
    cycles_before_can: int = 200
    # Deterministic verification commands (each an argv) that gate every item. ``None`` = the
    # default Python gate (ruff + ty + pytest via uv). An explicit EMPTY list is rejected: an item
    # is only ever marked done by at least one passing gating check.
    check_commands: list[list[str]] | None = None
    # Spend ceiling for the whole mission (USD); ``None`` = the worker's ``budget_usd_ceiling``.
    budget_usd: float | None = None
    # Degraded-dependency parking: exponential backoff between health checks, capped.
    park_initial_seconds: int = 60
    park_max_seconds: int = 3600
    # On deadlock (blocked items), wait this long for a human "retry"/"abort" decision; 0 = report
    # the deadlock immediately.
    deadlock_gate_seconds: int = 0
    # How long to wait for a human to approve/reject an irreversible action before rejecting it.
    approval_timeout_seconds: int = 86_400
    # Escalation ladder for every gate: reminder offsets in seconds after the gate opens.
    gate_escalation_seconds: list[int] = field(default_factory=lambda: [900, 2700, 14_400, 43_200])
    # Deadlock gate decision on timeout: "abort" | "impossible" (never "retry").
    deadlock_gate_default: str = "abort"
    # The deadlock gate recommends "impossible" after this many consecutive failed cycles on one
    # item (``ops.lifecycle.should_declare_impossible``).
    impossible_after_failures: int = 3
    # Durable pause between cycles (status SLEEPING); 0 = none.
    cycle_pause_seconds: int = 0
    # Scheduled start: sleep (status SLEEPING) until this epoch time; 0 = start now.
    resume_at: float = 0.0
    # Carried across Continue-As-New; ``None`` on the first run.
    state: MissionState | None = None


@dataclass
class CycleInput:
    """One agent-cycle invocation."""

    mission_id: str
    workdir: str
    cycle_id: str
    check_commands: list[list[str]] | None = None
    budget_usd: float | None = None
    max_cycles: int = 1000
    steer_notes: list[str] = field(default_factory=list)
    approved_actions: list[ApprovedAction] = field(default_factory=list)


@dataclass
class CycleResult:
    """The small, journaled result of one cycle (no raw transcripts — pointers/summary only).

    Completeness comes from the committed checklist: ``is_complete`` = every item verified done;
    ``is_deadlocked`` = nothing actionable but items remain (blocked / unsatisfiable deps).
    """

    item_id: str | None
    advanced: bool
    head_sha: str
    is_complete: bool
    items_done: int
    items_total: int
    note: str = ""
    verdict: str = ""
    is_deadlocked: bool = False
    item_blocked: bool = False
    reason: str = ""
    spent_usd: float = 0.0
    item_split: bool = False
    # Irreversible actions the agent attempted this cycle (need a human), and approvals it used.
    pending_approvals: list[PendingApproval] = field(default_factory=list)
    used_approvals: list[str] = field(default_factory=list)


@dataclass
class HealthInput:
    """Dependency health probe for a parked mission."""

    mission_id: str
    workdir: str


@dataclass
class HealthReport:
    healthy: bool
    reason: str = ""
    degraded: list[str] = field(default_factory=list)


@dataclass
class UnblockInput:
    """Human-approved retry of blocked checklist items."""

    mission_id: str
    workdir: str
    cycle_id: str


@dataclass
class MissionResult:
    """Terminal mission summary."""

    mission_id: str
    completed: bool
    cycles: int
    head_sha: str
    items_done: int
    items_total: int
    # One of the ``OUTCOME_*`` constants.
    outcome: str = OUTCOME_COMPLETED
    reason: str = ""
    status: str = ""


@dataclass
class SubAgentInput:
    """Input to a durable sub-agent child workflow (a typed task contract, journaled)."""

    role_name: str
    objective: str
    workdir: str
    mission_id: str
    allow_egress: bool = False


@dataclass
class SubAgentOutput:
    """The condensed artifact a sub-agent returns (no raw transcripts)."""

    role: str
    brief: str
    tool_calls: int
    turns: int


@dataclass
class FanOutResult:
    """Outcome of a sub-agent fan-out: every success AND every failure (nothing is swallowed)."""

    outputs: list[SubAgentOutput] = field(default_factory=list)
    failures: list[str] = field(default_factory=list)
