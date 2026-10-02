"""Versioned signal / query / update names + mission status constants.

Centralized and versioned so the durable handoff contract can evolve without ambiguity. The
mission workflow exposes a ``status`` query (so a human can tell SLEEPING from WAITING_ON_HUMAN)
and a ``human_decision`` signal (to resolve a gate).
"""

from __future__ import annotations

# Signals / queries / updates (versioned).
SIGNAL_HUMAN_DECISION = "human_decision_v1"
#: The same decision with who made it: ``{"decision": str, "by": str}`` (``mission-approve --as``).
SIGNAL_HUMAN_DECISION_V2 = "human_decision_v2"
SIGNAL_STEER = "steer_v1"
#: Steering notes kept per mission and the length of one note (``lha mission-steer``).
MAX_STEER_NOTES = 20
MAX_STEER_CHARS = 2000
SIGNAL_SNOOZE = "snooze_v1"
QUERY_STATUS = "status_v1"
QUERY_CYCLES = "cycles_done"
QUERY_GATE = "gate_v1"
QUERY_GATE_LOG = "gate_log_v1"
QUERY_STEER_NOTES = "steer_notes"
UPDATE_VERIFY_VERDICT = "verify_verdict_v1"

# Mission status values (the values of the text column `missions.status`).
STATUS_RUNNING = "RUNNING"
STATUS_SLEEPING = "SLEEPING"  # on a durable timer by design (pause / scheduled start / snooze)
STATUS_WAITING_ON_HUMAN = "WAITING_ON_HUMAN"  # a gate is open
STATUS_DEGRADED_PARK = "DEGRADED_PARK"  # a critical dependency is down
STATUS_DONE = "DONE"
STATUS_ABORTED = "ABORTED"
STATUS_IMPOSSIBLE = "IMPOSSIBLE"

# Gate kinds + options.
GATE_TOOL_CALL = "tool_call"
GATE_DEADLOCK = "deadlock"
APPROVAL_OPTIONS = ("approve", "reject")
DEADLOCK_OPTIONS = ("retry", "abort", "impossible")
DEADLOCK_DEFAULTS = ("abort", "impossible")  # "retry" is never an unattended default
