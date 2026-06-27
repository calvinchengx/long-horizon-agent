"""Offline eval harness.

Runs a versioned gold set of cases through a scorer and reports a pass rate. A case with a
``checker`` is scored deterministically (no model call); only cases without one go to the
Agent-as-Judge (for genuinely fuzzy criteria). Gated in CI to block self-inflicted
regressions, and used as the promotion gate for offline-evolved prompts/skills. The same cases can
be replayed against a candidate prompt to confirm no regression before promotion.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field

from lha.contracts.model import ModelProvider


@dataclass
class EvalCase:
    name: str
    candidate: str
    rubric: str
    # Deterministic scorer: when set, ``checker(candidate)`` decides pass/fail and the judge is
    # not consulted. Prefer this wherever the criterion is machine-checkable.
    checker: Callable[[str], bool] | None = None


@dataclass
class EvalCaseResult:
    name: str
    passed: bool
    score: float
    rationale: str


@dataclass
class EvalReport:
    results: list[EvalCaseResult] = field(default_factory=list)

    @property
    def pass_rate(self) -> float:
        return sum(1 for r in self.results if r.passed) / len(self.results) if self.results else 0.0

    @property
    def mean_score(self) -> float:
        return sum(r.score for r in self.results) / len(self.results) if self.results else 0.0


async def run_eval_suite(*, model: ModelProvider, cases: list[EvalCase]) -> EvalReport:
    """Score each case (checker, else judge) and aggregate (pass rate + mean score)."""
    # Imported lazily to avoid an import cycle (agents -> evals.harness -> agents.judge).
    from lha.agents.judge import AgentAsJudge

    judge = AgentAsJudge(model)
    results: list[EvalCaseResult] = []
    for case in cases:
        if case.checker is not None:
            passed = bool(case.checker(case.candidate))
            results.append(
                EvalCaseResult(
                    name=case.name,
                    passed=passed,
                    score=1.0 if passed else 0.0,
                    rationale="deterministic check " + ("passed" if passed else "failed"),
                )
            )
            continue
        verdict = await judge.judge(candidate=case.candidate, rubric=case.rubric)
        results.append(
            EvalCaseResult(
                name=case.name,
                passed=verdict.passed,
                score=verdict.score,
                rationale=verdict.rationale,
            )
        )
    return EvalReport(results=results)
