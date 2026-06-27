"""The agent org: role definitions + the role 'brains' (Planner, Reviewer, Researcher, ...)."""

from lha.agents.evolution import (
    EvolutionResult,
    PromptEvalCase,
    evaluate_prompt,
    evolve_and_gate,
)
from lha.agents.evolver import PromptCandidate, PromptEvolver
from lha.agents.judge import AgentAsJudge, Verdict
from lha.agents.planner import Planner, parse_checklist
from lha.agents.reflection import reflect_on_failure
from lha.agents.reviewer import Reviewer, ReviewResult, Tester, parse_review
from lha.agents.roles import ROLES, ModelTier, RoleSpec, claude_model_for
from lha.agents.router import model_for_role
from lha.agents.specialists import Auditor, Implementer, Integrator, Librarian
from lha.agents.subagent import SubAgent, SubAgentResult
from lha.agents.team import research_fanout

__all__ = [
    "ROLES",
    "AgentAsJudge",
    "Auditor",
    "EvolutionResult",
    "Implementer",
    "Integrator",
    "Librarian",
    "ModelTier",
    "Planner",
    "PromptCandidate",
    "PromptEvalCase",
    "PromptEvolver",
    "ReviewResult",
    "Reviewer",
    "RoleSpec",
    "SubAgent",
    "SubAgentResult",
    "Tester",
    "Verdict",
    "claude_model_for",
    "evaluate_prompt",
    "evolve_and_gate",
    "model_for_role",
    "parse_checklist",
    "parse_review",
    "reflect_on_failure",
    "research_fanout",
]
