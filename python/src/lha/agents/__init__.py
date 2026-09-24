"""The agent org: role definitions + the role 'brains' (Planner, Reviewer, Researcher, ...)."""

from lha.agents.planner import Planner, parse_checklist
from lha.agents.reflection import reflect_on_failure
from lha.agents.reviewer import Reviewer, ReviewResult, parse_review
from lha.agents.roles import ROLES, ModelTier, RoleSpec, claude_model_for
from lha.agents.router import model_for_role
from lha.agents.specialists import Implementer
from lha.agents.subagent import SubAgent, SubAgentResult
from lha.agents.team import research_fanout

__all__ = [
    "ROLES",
    "Implementer",
    "ModelTier",
    "Planner",
    "ReviewResult",
    "Reviewer",
    "RoleSpec",
    "SubAgent",
    "SubAgentResult",
    "claude_model_for",
    "model_for_role",
    "parse_checklist",
    "parse_review",
    "reflect_on_failure",
    "research_fanout",
]
