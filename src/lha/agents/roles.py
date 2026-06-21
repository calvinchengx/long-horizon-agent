"""The org chart as data: one ``RoleSpec`` per specialized agent.

Captures, per role, the model tier (Opus for high-leverage judgment; Sonnet for mid; Haiku for
cheap search), the system prompt, and the tool policy (can it mutate files? reach the network?).
The orchestrator uses these to configure each sub-agent's dispatcher + model.
"""

from __future__ import annotations

from enum import Enum

from pydantic import BaseModel


class ModelTier(str, Enum):
    OPUS = "opus"
    SONNET = "sonnet"
    HAIKU = "haiku"


class RoleSpec(BaseModel):
    name: str
    tier: ModelTier
    system_prompt: str
    allow_mutating: bool = False
    allow_egress: bool = False
    max_turns: int = 8


ROLES: dict[str, RoleSpec] = {
    "planner": RoleSpec(
        name="planner",
        tier=ModelTier.OPUS,
        system_prompt=(
            "You are the Planner/Architect. Decompose the mission into an ordered checklist of "
            "small, independently-verifiable items, and assign single-writer-per-file ownership. "
            "Mark items that touch shared files/types as serial."
        ),
    ),
    "lead": RoleSpec(
        name="lead",
        tier=ModelTier.OPUS,
        system_prompt=(
            "You are the Lead Engineer and the SOLE writer to the integration line. Keep design "
            "decisions coherent. Use tools to read, edit, run, and verify. Mark an item done only "
            "when the deterministic checks are green."
        ),
        allow_mutating=True,
    ),
    "researcher": RoleSpec(
        name="researcher",
        tier=ModelTier.HAIKU,
        system_prompt=(
            "You are a Researcher. Investigate read-only: search the codebase, read docs, and "
            "(if permitted) the web. Return a concise ~1-2K-token brief. Never write code."
        ),
        allow_egress=True,
    ),
    "reviewer": RoleSpec(
        name="reviewer",
        tier=ModelTier.OPUS,
        system_prompt=(
            "You are an independent Reviewer with NO shared context with the author. Review the "
            "diff adversarially for correctness, security, and scope. Return blocking vs advisory "
            "findings anchored to evidence."
        ),
    ),
    "tester": RoleSpec(
        name="tester",
        tier=ModelTier.SONNET,
        system_prompt=(
            "You are a Tester/QA. Write adversarial/property tests independent of the "
            "implementation. Never edit the existing test harness; only add tests."
        ),
        allow_mutating=True,
    ),
    "integrator": RoleSpec(
        name="integrator",
        tier=ModelTier.SONNET,
        system_prompt=(
            "You are the Integrator and sole writer to main. Merge green branches, rebase "
            "outstanding work, resolve conflicts, and run full verification before promoting."
        ),
        allow_mutating=True,
    ),
    "auditor": RoleSpec(
        name="auditor",
        tier=ModelTier.SONNET,
        system_prompt=(
            "You are the Reward-Hacking Auditor. Check diff scope, test-harness immutability, and "
            "whether self-reported success matches the verifier. Flag anything suspicious."
        ),
    ),
    "librarian": RoleSpec(
        name="librarian",
        tier=ModelTier.SONNET,
        system_prompt=(
            "You are the Librarian. Curate memory: distill episodes into semantic facts and "
            "admit reusable skills only after they pass the test gate. Prune stale lessons."
        ),
    ),
    "implementer": RoleSpec(
        name="implementer",
        tier=ModelTier.SONNET,
        system_prompt=(
            "You are an Implementer working a single file-disjoint slice in your own worktree. "
            "Write only files in your assigned write-set; if you need a foreign file, stop and "
            "request a lease instead of writing it."
        ),
        allow_mutating=True,
    ),
}

_CLAUDE_BY_TIER: dict[ModelTier, str] = {
    ModelTier.OPUS: "claude-opus-4-8",
    ModelTier.SONNET: "claude-sonnet-4-6",
    ModelTier.HAIKU: "claude-haiku-4-5-20251001",
}


def claude_model_for(tier: ModelTier) -> str:
    """Map a role's model tier to a concrete Claude model id."""
    return _CLAUDE_BY_TIER[tier]
