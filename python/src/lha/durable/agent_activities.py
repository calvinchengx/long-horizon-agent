"""Activity that runs a sub-agent (the non-deterministic body of a sub-agent child workflow).

Every metered call of the sub-agent is also written to the persistent cost ledger (SQLite or
Postgres, ``lha.persistence``) under the parent mission id, keyed by workflow/activity/attempt so a
retried attempt's calls are new rows and a replayed write is a no-op.
"""

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
from lha.persistence.store import StoreUnavailableError, open_store
from lha.persistence.tracking import LedgerSink


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
    meter.cycle_id = f"subagent:{inp.role_name}"
    try:
        store = await open_store(settings, workdir=inp.workdir)
    except BaseException as exc:
        await session.close()
        await model.aclose()
        if not isinstance(exc, StoreUnavailableError):
            raise  # e.g. an unwritable SQLite path: retryable
        raise ApplicationError(
            f"cannot open the mission store: {exc}", type=ERROR_CONFIG, non_retryable=True
        ) from exc
    if activity.in_activity():
        info = activity.info()
        prefix = f"sub:{info.workflow_id}:{info.activity_id}@{info.attempt}"
    else:
        prefix = f"sub:{inp.role_name}"
    LedgerSink(store, inp.mission_id, key_prefix=prefix).attach(meter)
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
        try:
            await session.close()
            await model.aclose()
        finally:
            meter.on_record = None
            await store.close()
    return SubAgentOutput(
        role=result.role, brief=result.brief, tool_calls=result.tool_calls, turns=result.turns
    )
