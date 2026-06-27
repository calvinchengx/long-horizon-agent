"""Activity that runs a sub-agent (the non-deterministic body of a sub-agent child workflow).

The sub-agent is budgeted like a cycle: when the workdir is a mission checkout, its meter is
seeded with the mission's recorded spend (``.git/lha/spend.ndjson``), the ceiling is
``SubAgentInput.budget_usd`` (else the worker's), and its own spend is appended to that journal,
so researchers count against the same mission budget as the Lead.

Every metered call of the sub-agent is also written to the persistent cost ledger (SQLite or
Postgres, ``lha.persistence``) under the parent mission id, keyed by workflow/activity/attempt so a
retried attempt's calls are new rows and a replayed write is a no-op.
"""

from __future__ import annotations

import asyncio

from temporalio import activity
from temporalio.exceptions import ApplicationError

from lha.agent.assembly import open_lead_sandbox
from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent
from lha.config import get_settings
from lha.contracts.tools import ToolContext
from lha.durable.activities import _with_heartbeat, build_cycle_meter, record_spend
from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    CycleInput,
    SubAgentInput,
    SubAgentOutput,
)
from lha.execution import UnsafeSandboxError
from lha.execution.tools.toolset import build_run_dispatcher
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.ids import idempotency_key
from lha.model import build_provider
from lha.persistence.store import StoreUnavailableError, open_store
from lha.persistence.tracking import LedgerSink
from lha.state import git_ops


@activity.defn
async def run_subagent(inp: SubAgentInput) -> SubAgentOutput:
    """Construct + run one sub-agent (metered, in the configured sandbox); return its brief."""
    settings = get_settings()
    role = ROLES.get(inp.role_name)
    if role is None:
        raise ApplicationError(
            f"unknown sub-agent role {inp.role_name!r}", type=ERROR_CONFIG, non_retryable=True
        )
    journal = await asyncio.to_thread(git_ops.is_repo, inp.workdir)
    cycle_id = f"{inp.cycle_id}:{inp.role_name}" if inp.cycle_id else f"subagent:{inp.role_name}"
    if journal:  # a mission checkout: the mission's spend so far counts
        meter = await asyncio.to_thread(
            build_cycle_meter,
            settings,
            CycleInput(
                mission_id=inp.mission_id,
                workdir=inp.workdir,
                cycle_id=cycle_id,
                budget_usd=inp.budget_usd,
                max_cycles=settings.max_cycles,
            ),
        )
    else:
        meter = CostMeter(
            ledger=CostLedger(),
            governor=BudgetGovernor(
                ceiling_usd=(
                    settings.budget_usd_ceiling if inp.budget_usd is None else inp.budget_usd
                ),
                max_cycles=settings.max_cycles,
                allow_unknown_cost=settings.allow_unpriced_models,
            ),
        )
    try:  # web tools iff LHA_EGRESS_ALLOW_HOSTS and the role + input allow egress
        dispatcher = build_run_dispatcher(
            settings,
            allow_mutating=role.allow_mutating,
            allow_egress=None if role.allow_egress and inp.allow_egress else False,
        )
    except ValueError as exc:  # RuleOfTwoViolation / WebConfigError: fail closed
        raise ApplicationError(
            f"cannot assemble the sub-agent's tools: {exc}", type=ERROR_CONFIG, non_retryable=True
        ) from exc
    try:
        model = meter.wrap(build_provider(settings), role=inp.role_name)
        # The same sandbox as the lead: image, egress allow-list and resource limits.
        session = await open_lead_sandbox(settings, inp.workdir)
    except (UnsafeSandboxError, ValueError) as exc:
        raise ApplicationError(
            f"cannot start sub-agent: {exc}", type=ERROR_CONFIG, non_retryable=True
        ) from exc
    meter.cycle_id = cycle_id
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
    spend_key = idempotency_key(inp.mission_id, prefix)
    LedgerSink(store, inp.mission_id, key_prefix=prefix).attach(meter)
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
            if journal:
                await asyncio.to_thread(
                    record_spend,
                    inp.workdir,
                    key=spend_key,
                    cycle_id=cycle_id,
                    ledger=meter.ledger,
                )
        finally:
            meter.on_record = None
            await store.close()
    return SubAgentOutput(
        role=result.role, brief=result.brief, tool_calls=result.tool_calls, turns=result.turns
    )
