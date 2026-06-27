"""Reviewer verdicts, strict judge, evolution gate, research fan-out failures, role tool policy."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import httpx
import pytest

from lha.agents.claude_sdk_lead import collect_sdk_messages
from lha.agents.evolution import PromptEvalCase, evolve_and_gate
from lha.agents.judge import parse_verdict
from lha.agents.reviewer import Reviewer, parse_review
from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent
from lha.agents.team import research_fanout
from lha.contracts.model import ModelMessage, ToolCall, TurnResult, Usage
from lha.contracts.tools import ToolContext
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.model.stub import StubModel


async def _ctx(tmp_path: Path) -> ToolContext:
    session = await LocalSandbox().open(workdir=str(tmp_path))
    return ToolContext(mission_id="m1", session=session)


# --- reviewer (M5) ------------------------------------------------------------------------


def test_no_blocking_issues_is_not_blocking() -> None:
    verdict, blocking, issues, _ = parse_review(
        '{"done": true, "verdict": "approve", "blocking_issues": [], '
        '"summary": "No blocking issues found."}'
    )
    assert (verdict, blocking, issues) == ("approve", False, [])


def test_block_verdict_is_blocking_with_issues() -> None:
    verdict, blocking, issues, advisory = parse_review(
        '{"verdict": "block", "blocking_issues": ["SQL injection in q()"], "advisory": ["naming"]}'
    )
    assert (verdict, blocking, issues, advisory) == (
        "block",
        True,
        ["SQL injection in q()"],
        ["naming"],
    )


def test_approve_with_listed_blocking_issues_is_blocking() -> None:
    _, blocking, issues, _ = parse_review('{"verdict": "approve", "blocking_issues": ["x"]}')
    assert blocking and issues == ["x"]


def test_unparseable_review_is_conservatively_blocking() -> None:
    assert parse_review("Looks fine to me, no blocking issues.")[1] is True
    verdict, blocking, issues, _ = parse_review("notes\nBLOCK: missing test\n")
    assert (verdict, blocking, issues) == ("block", True, ["missing test"])


@pytest.mark.asyncio
async def test_reviewer_returns_structured_verdict(tmp_path: Path) -> None:
    model = StubModel(
        script=[TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')]
    )
    reviewer = Reviewer(
        model, AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=False)
    )
    review = await reviewer.review(diff="+x", criteria="c", ctx=await _ctx(tmp_path))
    assert not review.blocking and review.verdict == "approve"


# --- judge (M5) ---------------------------------------------------------------------------


def test_judge_rejects_string_booleans() -> None:
    verdict = parse_verdict('{"pass": "false", "score": 0.9}')
    assert not verdict.passed
    assert "invalid" in verdict.rationale


def test_judge_clamps_score_and_rejects_non_numbers() -> None:
    assert parse_verdict('{"pass": true, "score": 7}').score == 1.0
    assert parse_verdict('{"pass": false, "score": -2}').score == 0.0
    assert not parse_verdict('{"pass": true, "score": "0.9"}').passed
    assert not parse_verdict('{"pass": true, "score": true}').passed


# --- evolution gate (M6) ------------------------------------------------------------------


class _RoleUnderTest:
    """Answers well only when the system prompt contains 'GOOD'; records the prompts it saw."""

    name = "fake:role"

    def __init__(self, proposal: str) -> None:
        self._proposal = proposal
        self.system_prompts: list[str] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        system = messages[0].content
        if "optimize an agent's SYSTEM PROMPT" in system:
            return TurnResult(text=self._proposal)
        self.system_prompts.append(system)
        return TurnResult(text="great answer" if "GOOD" in system else "bad answer")

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


class _Judge:
    name = "fake:judge"

    def __init__(self) -> None:
        self.calls = 0

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls += 1
        passed = "great answer" in messages[-1].content
        return TurnResult(text=f'{{"pass": {"true" if passed else "false"}, "score": 1}}')

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


_CASES = [
    PromptEvalCase(name="a", task="t1", rubric="r"),
    PromptEvalCase(name="b", task="t2", rubric="r"),
]


@pytest.mark.asyncio
async def test_evolution_evaluates_candidate_prompt_on_same_cases() -> None:
    role = _RoleUnderTest(proposal="GOOD prompt")
    judge = _Judge()
    result = await evolve_and_gate(
        model=role,
        judge_model=judge,
        role="lead",
        current_prompt="old prompt",
        failure_traces=["t"],
        cases=_CASES,
    )
    assert role.system_prompts == ["old prompt", "old prompt", "GOOD prompt", "GOOD prompt"]
    assert judge.calls == 4  # the separate judge model scored every output
    assert result.baseline_pass_rate == 0.0 and result.candidate_pass_rate == 1.0
    assert result.promoted


@pytest.mark.asyncio
async def test_evolution_rejects_non_improvement_and_empty_cases() -> None:
    result = await evolve_and_gate(
        model=_RoleUnderTest(proposal="still bad"),
        judge_model=_Judge(),
        role="lead",
        current_prompt="old prompt",
        failure_traces=[],
        cases=_CASES,
    )
    assert not result.promoted
    with pytest.raises(ValueError):
        await evolve_and_gate(
            model=_RoleUnderTest(proposal="x"),
            role="lead",
            current_prompt="old",
            failure_traces=[],
            cases=[],
        )


# --- research fan-out + role tool policy (M7) ---------------------------------------------


class _FailsWith:
    name = "fake:fails"

    def __init__(self, exc: Exception) -> None:
        self._exc = exc

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        raise self._exc

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


def _http_error(status: int) -> httpx.HTTPStatusError:
    request = httpx.Request("POST", "http://x/")
    return httpx.HTTPStatusError(
        str(status), request=request, response=httpx.Response(status, request=request)
    )


@pytest.mark.asyncio
async def test_research_fanout_surfaces_failures(tmp_path: Path) -> None:
    results = await research_fanout(
        model=_FailsWith(ValueError("model exploded")),
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=False),
        ctx=await _ctx(tmp_path),
        queries=["q1", "q2"],
    )
    assert len(results) == 2
    assert all(r.error and "model exploded" in r.error for r in results)


@pytest.mark.asyncio
async def test_research_fanout_raises_on_auth_errors(tmp_path: Path) -> None:
    with pytest.raises(httpx.HTTPStatusError):
        await research_fanout(
            model=_FailsWith(_http_error(401)),
            dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=False),
            ctx=await _ctx(tmp_path),
            queries=["q1"],
        )


@pytest.mark.asyncio
async def test_researcher_never_sees_or_runs_mutating_tools(tmp_path: Path) -> None:
    ctx = await _ctx(tmp_path)
    permissive = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True
    )  # would allow writes
    model = StubModel(
        script=[
            TurnResult(
                text="",
                tool_calls=[
                    ToolCall(
                        id="t1", name="write_file", arguments={"path": "x.txt", "content": "pwn"}
                    )
                ],
            ),
            TurnResult(text='{"done": true, "summary": "done"}'),
        ]
    )
    agent = SubAgent(role=ROLES["researcher"], model=model, dispatcher=permissive)
    assert all(not s.mutating for s in agent.visible_specs())
    result = await agent.run(objective="look around", ctx=ctx)
    assert result.tool_calls == 1
    assert not (tmp_path / "x.txt").exists()


# --- Claude Agent SDK lead (M7) -----------------------------------------------------------


@dataclass
class TextBlock:
    text: str


@dataclass
class AssistantMessage:
    content: list[object]
    model: str = "claude-sonnet-4-6"


@dataclass
class UserMessage:
    content: list[object]


@dataclass
class ResultMessage:
    result: str | None
    session_id: str
    total_cost_usd: float | None = None
    is_error: bool = False


def test_sdk_lead_reads_result_and_content_blocks() -> None:
    out = collect_sdk_messages(
        [
            AssistantMessage(content=[TextBlock("working on it")]),
            UserMessage(content=[{"type": "tool_result", "content": "file body"}]),
            ResultMessage(result="all done", session_id="s-1", total_cost_usd=0.12),
        ]
    )
    assert out.text == "all done"
    assert out.session_id == "s-1"
    assert out.cost_usd == pytest.approx(0.12)


def test_sdk_lead_falls_back_to_assistant_text_blocks() -> None:
    out = collect_sdk_messages(
        [
            AssistantMessage(content=[TextBlock("part one"), {"type": "text", "text": "part two"}]),
            ResultMessage(result=None, session_id="s-2"),
        ]
    )
    assert out.text == "part one\npart two"
    assert out.session_id == "s-2"
