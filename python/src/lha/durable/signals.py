"""Versioned signal / query / update names + mission status constants.

Centralized and versioned so the durable handoff contract can evolve without ambiguity. The
mission workflow exposes a ``status`` query (so a human can tell SLEEPING from WAITING_ON_HUMAN)
and a ``human_decision`` signal (to resolve a gate).
"""

from __future__ import annotations

# Signals / queries / updates (versioned).
SIGNAL_HUMAN_DECISION = "human_decision_v1"
SIGNAL_STEER = "steer_v1"
QUERY_STATUS = "status_v1"
QUERY_CYCLES = "cycles_done"
UPDATE_VERIFY_VERDICT = "verify_verdict_v1"

# Mission status values (mirror the `missions.status` DB enum).
STATUS_RUNNING = "RUNNING"
STATUS_SLEEPING = "SLEEPING"
STATUS_WAITING_ON_HUMAN = "WAITING_ON_HUMAN"
STATUS_DEGRADED_PARK = "DEGRADED_PARK"
STATUS_DONE = "DONE"
STATUS_ABORTED = "ABORTED"
STATUS_IMPOSSIBLE = "IMPOSSIBLE"
