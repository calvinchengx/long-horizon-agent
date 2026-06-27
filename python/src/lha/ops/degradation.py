"""Dependency-degradation → safe-park decisions.

When a dependency fails, the agent shouldn't blindly retry forever. Critical deps (git, the model,
the sandbox where code runs and checks verify) being DOWN means the agent can't make verified
progress → park safely (the durable workflow sleeps with backoff, status ``DEGRADED_PARK``, and
re-probes health). Optional deps (pgvector, Langfuse, egress proxy) DOWN means degrade gracefully
and keep going.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

# Without these, no verified progress is possible → park.
CRITICAL: frozenset[str] = frozenset({"git", "model", "sandbox"})
# These degrade gracefully (e.g. semantic retrieval falls back to lexical / git-grep).
OPTIONAL: frozenset[str] = frozenset({"pgvector", "langfuse", "egress_proxy"})


class Health(str, Enum):
    OK = "ok"
    DEGRADED = "degraded"
    DOWN = "down"


@dataclass
class DependencyStatus:
    name: str
    health: Health
    detail: str = ""


@dataclass
class SafeParkDecision:
    park: bool
    reason: str
    degraded: list[str] = field(default_factory=list)


def decide_safe_park(statuses: list[DependencyStatus]) -> SafeParkDecision:
    """Park iff a CRITICAL dependency is DOWN; otherwise continue (noting degraded optionals)."""
    down_critical = [s.name for s in statuses if s.health is Health.DOWN and s.name in CRITICAL]
    degraded = [s.name for s in statuses if s.health is not Health.OK and s.name in OPTIONAL]
    if down_critical:
        return SafeParkDecision(
            park=True,
            reason=f"critical dependency down: {sorted(down_critical)}",
            degraded=degraded,
        )
    return SafeParkDecision(
        park=False, reason="operational (some optionals degraded)", degraded=degraded
    )
