"""Thin runners for the remaining roles (Integrator / Auditor / Librarian / Implementer).

Each wraps ``SubAgent`` with its role's prompt + a scoped dispatcher. They share one base so the
org has a named, typed entry point per role even though the loop mechanics are identical.
"""

from __future__ import annotations

from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent, SubAgentResult
from lha.contracts.model import ModelProvider
from lha.contracts.tools import ToolContext, ToolDispatcher


class _RoleRunner:
    role_name: str

    def __init__(self, model: ModelProvider, dispatcher: ToolDispatcher) -> None:
        self._agent = SubAgent(role=ROLES[self.role_name], model=model, dispatcher=dispatcher)

    async def run(
        self, *, objective: str, ctx: ToolContext, extra_context: str = ""
    ) -> SubAgentResult:
        return await self._agent.run(objective=objective, ctx=ctx, extra_context=extra_context)


class Integrator(_RoleRunner):
    role_name = "integrator"


class Auditor(_RoleRunner):
    role_name = "auditor"


class Librarian(_RoleRunner):
    role_name = "librarian"


class Implementer(_RoleRunner):
    role_name = "implementer"
