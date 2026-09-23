"""The escalation ladder shared by every human gate (pure, deterministic — workflow-safe).

A gate opens, sends a reminder at each escalation offset that falls strictly inside its timeout,
and applies its default action at the timeout. ``escalation_schedule`` normalizes the configured
offsets; ``next_rung`` says how long to wait before the next rung. Nothing here reads a clock:
callers pass elapsed time (``workflow.now()`` in the workflow, ``time.monotonic`` locally).
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass


def escalation_schedule(timeout_seconds: float, steps: Iterable[int | float]) -> list[float]:
    """Sorted, unique reminder offsets strictly between 0 and ``timeout_seconds``."""
    return sorted({float(s) for s in steps if 0 < float(s) < timeout_seconds})


@dataclass(frozen=True)
class Rung:
    """The next rung of the ladder: wait ``wait_seconds``; then remind (``step`` > 0) or default."""

    wait_seconds: float
    step: int  # 1-based reminder number; 0 = the timeout (apply the default)


def next_rung(elapsed: float, timeout_seconds: float, schedule: list[float], sent: int) -> Rung:
    """What to wait for next, given ``sent`` reminders already emitted at ``elapsed`` seconds."""
    if sent < len(schedule):
        return Rung(wait_seconds=max(0.0, schedule[sent] - elapsed), step=sent + 1)
    return Rung(wait_seconds=max(0.0, timeout_seconds - elapsed), step=0)
