"""Durable sub-agent child workflow + fan-out helper.

Each sub-agent (Researcher/Reviewer/...) is a Temporal child workflow whose body is a single
``run_subagent`` activity — so a sub-agent is independently retried and its result journaled.
``research_children`` fans out N children concurrently (the read-parallelism win) and returns
every brief AND every failure; cancellation of the parent propagates to the children and is
re-raised, never swallowed.
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ChildWorkflowError

with workflow.unsafe.imports_passed_through():
    from lha.durable.agent_activities import run_subagent

from lha.durable.types import (
    ERROR_BUDGET_EXCEEDED,
    ERROR_CONFIG,
    FanOutResult,
    SubAgentInput,
    SubAgentOutput,
)

_SUBAGENT_RETRY = RetryPolicy(
    maximum_attempts=3, non_retryable_error_types=[ERROR_BUDGET_EXCEEDED, ERROR_CONFIG]
)


@workflow.defn
class SubAgentWorkflow:
    """Runs one sub-agent durably."""

    @workflow.run
    async def run(self, inp: SubAgentInput) -> SubAgentOutput:
        return await workflow.execute_activity(
            run_subagent,
            inp,
            start_to_close_timeout=timedelta(minutes=15),
            heartbeat_timeout=timedelta(minutes=2),
            retry_policy=_SUBAGENT_RETRY,
        )


def _describe(inp: SubAgentInput, exc: BaseException) -> str:
    cause: BaseException = exc
    while isinstance(cause, ChildWorkflowError) and cause.cause is not None:
        cause = cause.cause
    return f"{inp.role_name}: {type(cause).__name__}: {cause}"


async def research_children(inputs: list[SubAgentInput]) -> FanOutResult:
    """Fan out sub-agent child workflows concurrently; return all briefs and all failures.

    Must be called from workflow code. Child ids are ``subagent:<mission>:<role>:<uuid>`` with a
    deterministic (replay-safe) ``workflow.uuid4()``, so they never collide across fan-outs or
    Continue-As-New runs. If the calling workflow is cancelled, the children are cancelled too and
    ``CancelledError`` propagates.
    """
    tasks = [
        asyncio.ensure_future(
            workflow.execute_child_workflow(
                SubAgentWorkflow.run,
                inp,
                id=f"subagent:{inp.mission_id}:{inp.role_name}:{workflow.uuid4().hex[:12]}",
            )
        )
        for inp in inputs
    ]
    try:
        results = await asyncio.gather(*tasks, return_exceptions=True)
    except asyncio.CancelledError:
        for task in tasks:
            task.cancel()
        raise
    out = FanOutResult()
    for inp, res in zip(inputs, results, strict=True):
        if isinstance(res, SubAgentOutput):
            out.outputs.append(res)
        elif isinstance(res, asyncio.CancelledError) or not isinstance(res, Exception):
            raise res  # cancellation (or a BaseException) is never swallowed
        else:
            out.failures.append(_describe(inp, res))
    return out
