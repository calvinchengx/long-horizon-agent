"""Sub-agent fan-out: unique child ids across fan-outs, and failures surfaced (not swallowed)."""

from __future__ import annotations

import asyncio

import pytest
from temporalio import activity, workflow
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.durable.subagent_workflow import SubAgentWorkflow, research_children
from lha.durable.types import FanOutResult, SubAgentInput, SubAgentOutput

_child_ids: list[str] = []


@activity.defn(name="run_subagent")
async def _fake_subagent(inp: SubAgentInput) -> SubAgentOutput:
    _child_ids.append(activity.info().workflow_id)
    if inp.role_name == "boom":
        raise ApplicationError("auth failed (401)", type="AuthError", non_retryable=True)
    return SubAgentOutput(role=inp.role_name, brief=f"brief:{inp.objective}", tool_calls=0, turns=1)


@workflow.defn(sandboxed=False)
class _TwoFanOuts:
    @workflow.run
    async def run(self, mission_id: str) -> list[FanOutResult]:
        inputs = [
            SubAgentInput(
                role_name="researcher", objective="a", workdir=".", mission_id=mission_id
            ),
            SubAgentInput(role_name="boom", objective="b", workdir=".", mission_id=mission_id),
        ]
        first = await research_children(inputs)
        second = await research_children(inputs)  # same roles, same indices: ids must not clash
        return [first, second]


@pytest.mark.asyncio
async def test_fanout_ids_unique_and_failures_surfaced() -> None:
    _child_ids.clear()
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue="lha-fanout",
            workflows=[_TwoFanOuts, SubAgentWorkflow],
            activities=[_fake_subagent],
        ),
    ):
        results = await asyncio.wait_for(
            env.client.execute_workflow(
                _TwoFanOuts.run, "m1", id="fanout-test", task_queue="lha-fanout"
            ),
            120,
        )
    assert len(_child_ids) == 4 and len(set(_child_ids)) == 4
    assert all(cid.startswith("subagent:m1:") for cid in _child_ids)
    for fan in results:
        assert [o.brief for o in fan.outputs] == ["brief:a"]
        assert len(fan.failures) == 1
        assert fan.failures[0].startswith("boom:") and "auth failed" in fan.failures[0]
