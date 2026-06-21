"""Activity that runs a sub-agent (the non-deterministic body of a sub-agent child workflow)."""

from __future__ import annotations

from temporalio import activity
from temporalio.exceptions import ApplicationError

from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent
from lha.config import get_settings
from lha.contracts.tools import ToolContext
from lha.durable.activities import _with_heartbeat
from lha.durable.types import ERROR_BUDGET_EXCEEDED, ERROR_CONFIG, SubAgentInput, SubAgentOutput
from lha.execution import UnsafeSandboxError, open_sandbox
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.tools import default_local_tools
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.model import build_provider


@activity.defn
async def run_subagent(inp: SubAgentInput) -> SubAgentOutput:
    """Construct + run one sub-agent (metered, in the configured sandbox); return its brief."""
    settings = get_settings()
    role = ROLES.get(inp.role_name)
    if role is None:
        raise ApplicationError(
            f"unknown sub-agent role {inp.role_name!r}", type=ERROR_CONFIG, non_retryable=True
        )
    meter = CostMeter(
        ledger=CostLedger(),
        governor=BudgetGovernor(
            ceiling_usd=settings.budget_usd_ceiling,
            max_cycles=settings.max_cycles,
            allow_unknown_cost=settings.allow_unpriced_models,
        ),
    )
    try:
        model = meter.wrap(build_provider(settings), role=inp.role_name)
        session = await open_sandbox(
            settings.sandbox, workdir=inp.workdir, allow_unsafe_local=settings.allow_unsafe_local
        )
    except (UnsafeSandboxError, ValueError) as exc:
        raise ApplicationError(
            f"cannot start sub-agent: {exc}", type=ERROR_CONFIG, non_retryable=True
        ) from exc
    dispatcher = AllowListDispatcher.for_tools(
        default_local_tools(),
        allow_mutating=role.allow_mutating,
        allow_egress=role.allow_egress and inp.allow_egress,
    )
    agent = SubAgent(role=role, model=model, dispatcher=dispatcher)
    try:
        result = await _with_heartbeat(
            agent.run(
                objective=inp.objective,
                ctx=ToolContext(mission_id=inp.mission_id, session=session),
            ),
            f"subagent:{inp.role_name}",
        )
    except BudgetExceeded as exc:
        raise ApplicationError(str(exc), type=ERROR_BUDGET_EXCEEDED, non_retryable=True) from exc
    finally:
        await session.close()
        await model.aclose()
    return SubAgentOutput(
        role=result.role, brief=result.brief, tool_calls=result.tool_calls, turns=result.turns
    )
