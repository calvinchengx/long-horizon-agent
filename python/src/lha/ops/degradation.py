"""Dependency-degradation → safe-park decisions.

When a dependency fails, the agent shouldn't blindly retry forever. Critical deps (git, the model,
the sandbox where code runs and checks verify) being DOWN means the agent can't make verified
progress → park safely (the durable workflow sleeps with backoff, status ``DEGRADED_PARK``, and
re-probes health). Optional deps (Postgres, pgvector, embeddings, Langfuse, egress proxy) DOWN
means degrade gracefully and keep going.

Memory degradation (``decide_memory_mode``, used by ``lha.memory.service``): the dense channel of
hybrid retrieval needs its embedder and, on Postgres, pgvector. If any of ``postgres`` /
``pgvector`` / ``embeddings`` is not OK, retrieval drops to LEXICAL only — BM25 over the stored
memory + repo chunks, plus ``git grep`` over the checkout — and the mission continues.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

# Without these, no verified progress is possible → park.
CRITICAL: frozenset[str] = frozenset({"git", "model", "sandbox"})
# These degrade gracefully (e.g. semantic retrieval falls back to lexical / git-grep).
OPTIONAL: frozenset[str] = frozenset(
    {"postgres", "pgvector", "embeddings", "langfuse", "egress_proxy"}
)
# The optional deps the dense (vector) memory channel needs.
MEMORY_DENSE_DEPS: frozenset[str] = frozenset({"postgres", "pgvector", "embeddings"})


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


@dataclass
class MemoryMode:
    """How memory retrieval runs given the optional deps' health."""

    dense: bool  # use the embedder / vector index channel
    git_grep: bool  # add ``git grep`` over the checkout as a lexical channel
    reason: str
    degraded: list[str] = field(default_factory=list)

    @property
    def label(self) -> str:
        return "hybrid" if self.dense else "lexical"


def decide_memory_mode(statuses: list[DependencyStatus]) -> MemoryMode:
    """Hybrid (lexical + dense) iff every dense-channel dep is OK; else lexical + ``git grep``.

    Never parks: memory is optional, so a degraded memory plane only narrows retrieval.
    """
    degraded = sorted(
        s.name for s in statuses if s.name in MEMORY_DENSE_DEPS and s.health is not Health.OK
    )
    if degraded:
        details = "; ".join(
            f"{s.name}: {s.detail or s.health.value}"
            for s in sorted(statuses, key=lambda s: s.name)
            if s.name in degraded
        )
        return MemoryMode(
            dense=False,
            git_grep=True,
            reason=f"lexical-only retrieval (BM25 + git grep): {details}",
            degraded=degraded,
        )
    return MemoryMode(dense=True, git_grep=False, reason="hybrid retrieval (BM25 + dense)")
