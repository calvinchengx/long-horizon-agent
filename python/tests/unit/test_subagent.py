"""Tests for the generic sub-agent + parallel research fan-out."""

from __future__ import annotations

from pathlib import Path

import pytest

from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent
from lha.agents.team import research_fanout
from lha.contracts.model import TurnResult
from lha.contracts.tools import ToolContext
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.model.stub import StubModel


async def _ctx(tmp_path: Path) -> ToolContext:
    session = await LocalSandbox().open(workdir=str(tmp_path))
    return ToolContext(mission_id="m1", session=session)


@pytest.mark.asyncio
async def test_subagent_returns_a_brief(tmp_path: Path) -> None:
    agent = SubAgent(
        role=ROLES["researcher"],
        model=StubModel(),
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
    )
    result = await agent.run(objective="describe the repo layout", ctx=await _ctx(tmp_path))
    assert result.role == "researcher"
    assert result.brief


@pytest.mark.asyncio
async def test_subagent_uses_a_tool_then_finishes(tmp_path: Path) -> None:
    ctx = await _ctx(tmp_path)
    await ctx.session.write_file("README.md", "hello repo")
    model = StubModel(
        script=[
            TurnResult(text='{"tool": "read_file", "arguments": {"path": "README.md"}}'),
            TurnResult(text='{"done": true, "summary": "the readme says hello repo"}'),
        ]
    )
    agent = SubAgent(
        role=ROLES["researcher"],
        model=model,
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
    )
    result = await agent.run(objective="read the readme", ctx=ctx)
    assert result.tool_calls == 1
    assert "hello repo" in result.brief


@pytest.mark.asyncio
async def test_research_fanout_one_brief_per_query(tmp_path: Path) -> None:
    results = await research_fanout(
        model=StubModel(),
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        ctx=await _ctx(tmp_path),
        queries=["q1", "q2", "q3"],
    )
    assert len(results) == 3
    assert all(r.role == "researcher" for r in results)
