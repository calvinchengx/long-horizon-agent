"""Tests for the offline eval harness: aggregation, deterministic checkers, judge fallback."""

from __future__ import annotations

import pytest

from lha.contracts.model import ModelMessage, TurnResult
from lha.evals.harness import EvalCase, run_eval_suite
from lha.model.stub import StubModel


@pytest.mark.asyncio
async def test_eval_suite_aggregates_pass_rate() -> None:
    model = StubModel(
        script=[
            TurnResult(text='{"pass": true, "score": 1.0, "rationale": "ok"}'),
            TurnResult(text='{"pass": false, "score": 0.2, "rationale": "bad"}'),
        ]
    )
    report = await run_eval_suite(
        model=model,
        cases=[
            EvalCase(name="a", candidate="x", rubric="r"),
            EvalCase(name="b", candidate="y", rubric="r"),
        ],
    )
    assert len(report.results) == 2
    assert report.pass_rate == 0.5
    assert report.results[0].passed
    assert not report.results[1].passed


def _defines_working_add(candidate: str) -> bool:
    """A real deterministic check: execute the candidate and test its behaviour."""
    namespace: dict[str, object] = {}
    try:
        exec(compile(candidate, "<candidate>", "exec"), namespace)
    except Exception:
        return False
    add = namespace.get("add")
    return callable(add) and add(2, 3) == 5 and add(-1, 1) == 0


class _CountingModel(StubModel):
    def __init__(self, script: list[TurnResult]) -> None:
        super().__init__(script=script)
        self.prompts: list[str] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.prompts.append(messages[-1].content)
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


@pytest.mark.asyncio
async def test_checker_cases_score_real_behaviour_without_the_judge() -> None:
    # The judge would (wrongly) pass everything; checker cases must not consult it.
    model = _CountingModel([TurnResult(text='{"pass": true, "score": 1.0, "rationale": "ok"}')])
    report = await run_eval_suite(
        model=model,
        cases=[
            EvalCase(
                name="correct",
                candidate="def add(a, b):\n    return a + b\n",
                rubric="adds two ints",
                checker=_defines_working_add,
            ),
            EvalCase(
                name="off_by_one",
                candidate="def add(a, b):\n    return a + b + 1\n",
                rubric="adds two ints",
                checker=_defines_working_add,
            ),
            EvalCase(
                name="syntax_error",
                candidate="def add(a, b) return a + b",
                rubric="adds two ints",
                checker=_defines_working_add,
            ),
            EvalCase(name="fuzzy", candidate="a clear summary", rubric="is it clear?"),
        ],
    )
    by_name = {r.name: r for r in report.results}
    assert by_name["correct"].passed and by_name["correct"].score == 1.0
    assert not by_name["off_by_one"].passed and by_name["off_by_one"].score == 0.0
    assert not by_name["syntax_error"].passed
    assert by_name["fuzzy"].passed
    # Only the fuzzy case reached the judge, and the judge saw its candidate + rubric.
    assert len(model.prompts) == 1
    assert "a clear summary" in model.prompts[0]
    assert "is it clear?" in model.prompts[0]
    assert report.pass_rate == 0.5


@pytest.mark.asyncio
async def test_unparseable_judge_output_fails_the_case() -> None:
    model = StubModel(script=[TurnResult(text="looks great to me!")])
    report = await run_eval_suite(model=model, cases=[EvalCase("a", "x", "r")])
    assert report.pass_rate == 0.0
    assert report.results[0].rationale == "unparseable judge output"
