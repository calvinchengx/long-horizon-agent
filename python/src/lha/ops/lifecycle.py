"""Mission lifecycle — including the negative paths (abort / impossible).

Completion is the happy path; this covers the rest: a mission that keeps failing the same item is
escalated as 'possibly impossible' (a human gate), and terminal outcomes are named so partial work
can be salvaged rather than silently looping forever.
"""

from __future__ import annotations

from enum import Enum


class MissionOutcome(str, Enum):
    DONE = "DONE"
    ABORTED = "ABORTED"
    IMPOSSIBLE = "IMPOSSIBLE"
    PARKED = "DEGRADED_PARK"


def should_declare_impossible(*, consecutive_failures: int, threshold: int = 3) -> bool:
    """True when an item has failed verification too many times in a row (escalate to a human)."""
    return consecutive_failures >= threshold
