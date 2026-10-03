"""The ``Implementer`` role runner: ``SubAgent`` with the implementer's prompt.

Used by ``orchestrate``'s parallel waves, each implementer in its own worktree behind an
``OwnershipGuard``. The integration step is deterministic code
(``lha.agents.integrator.BranchIntegrator``), not a model-backed role.
"""

from __future__ import annotations

from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent, SubAgentResult
from lha.contracts.model import ModelProvider
from lha.contracts.tools import ToolContext, ToolDispatcher
from lha.obs.events import TraceRecorder


class Implementer:
    """Works one file-disjoint checklist item with a scoped dispatcher."""

    def __init__(
        self,
        model: ModelProvider,
        dispatcher: ToolDispatcher,
        *,
        max_turns: int | None = None,
        recorder: TraceRecorder | None = None,
        cycle_id: str = "",
    ) -> None:
        self._agent = SubAgent(
            role=ROLES["implementer"],
            model=model,
            dispatcher=dispatcher,
            max_turns=max_turns,
            recorder=recorder,
            cycle_id=cycle_id,
        )

    async def run(
        self, *, objective: str, ctx: ToolContext, extra_context: str = ""
    ) -> SubAgentResult:
        return await self._agent.run(objective=objective, ctx=ctx, extra_context=extra_context)
