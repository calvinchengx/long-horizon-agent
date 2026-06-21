"""Parallel read fan-out — the one place multi-agent genuinely wins.

Spawns N read-only sub-agents concurrently (each an isolated investigation) and returns their
condensed briefs. This is the orchestrator-worker pattern applied ONLY to side-effect-free reads;
coupled writes stay single-threaded on the Lead. In the durable build each sub-agent is a Temporal
child workflow; here it's ``asyncio.gather`` for the local runner.

Failures are never silently dropped: a failed researcher comes back as a ``SubAgentResult`` with
``error`` set (and is logged). Failures that no other researcher could fix — authentication /
permission errors and budget refusals — are raised, as is cancellation.
"""

from __future__ import annotations

import asyncio
import logging

import httpx

from lha.agents.roles import ROLES, RoleSpec
from lha.agents.subagent import SubAgent, SubAgentResult
from lha.contracts.model import ModelProvider
from lha.contracts.tools import ToolContext, ToolDispatcher
from lha.governor.metering import BudgetExceeded

logger = logging.getLogger(__name__)

_FATAL_STATUS = frozenset({401, 403})


def _is_fatal(exc: BaseException) -> bool:
    if isinstance(exc, BudgetExceeded):
        return True
    return isinstance(exc, httpx.HTTPStatusError) and exc.response.status_code in _FATAL_STATUS


async def research_fanout(
    *,
    model: ModelProvider,
    dispatcher: ToolDispatcher,
    ctx: ToolContext,
    queries: list[str],
    role: RoleSpec | None = None,
) -> list[SubAgentResult]:
    """Investigate ``queries`` in parallel; one result per query, failures marked with ``error``."""
    role = role or ROLES["researcher"]
    if role.allow_mutating:
        raise ValueError(f"research fan-out requires a read-only role; {role.name!r} can mutate")
    agents = [SubAgent(role=role, model=model, dispatcher=dispatcher) for _ in queries]
    outcomes = await asyncio.gather(
        *(
            agent.run(objective=query, ctx=ctx)
            for agent, query in zip(agents, queries, strict=True)
        ),
        return_exceptions=True,
    )
    results: list[SubAgentResult] = []
    for query, outcome in zip(queries, outcomes, strict=True):
        if isinstance(outcome, SubAgentResult):
            results.append(outcome)
            continue
        if not isinstance(outcome, Exception) or _is_fatal(outcome):
            raise outcome  # cancellation / auth / budget: stop, don't paper over it
        logger.warning("researcher failed on %r: %r", query[:120], outcome)
        results.append(
            SubAgentResult(
                role=role.name,
                brief="",
                tool_calls=0,
                turns=0,
                error=f"{type(outcome).__name__}: {outcome}"[:500],
            )
        )
    return results
