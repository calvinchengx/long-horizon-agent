"""Offline evolution gate: propose → evaluate → promote only if it strictly improves.

The offline self-improvement orchestration (runs between missions, never on a live run). It asks
the Evolver for an improved prompt, then runs the SAME held-out cases twice — once with the
baseline prompt as the role's system prompt and once with the candidate substituted — and has an
(ideally independent) judge model score each output against the case rubric. The candidate is
promoted ONLY if its pass rate beats the baseline's by MORE than ``min_improvement`` (ties are
rejected). An empty case set is refused: no evidence, no promotion. A real deployment also replays
recorded missions; this is the gate skeleton those plug into.
"""

from __future__ import annotations

from dataclasses import dataclass

from lha.agents.evolver import PromptCandidate, PromptEvolver
from lha.agents.judge import AgentAsJudge
from lha.contracts.model import ModelMessage, ModelProvider
from lha.evals.harness import EvalCaseResult, EvalReport


@dataclass
class PromptEvalCase:
    """One held-out case: a task given to the role, and the rubric its output is judged by."""

    name: str
    task: str
    rubric: str


@dataclass
class EvolutionResult:
    promoted: bool
    candidate: PromptCandidate
    baseline_pass_rate: float
    candidate_pass_rate: float
    reason: str


async def evaluate_prompt(
    *,
    model: ModelProvider,
    judge_model: ModelProvider,
    prompt: str,
    cases: list[PromptEvalCase],
    max_tokens: int | None = None,
) -> EvalReport:
    """Run each case with ``prompt`` as the system prompt and judge the output."""
    judge = AgentAsJudge(judge_model)
    results: list[EvalCaseResult] = []
    for case in cases:
        output = await model.complete(
            [
                ModelMessage(role="system", content=prompt),
                ModelMessage(role="user", content=case.task),
            ],
            max_tokens=max_tokens,
        )
        verdict = await judge.judge(candidate=output.text, rubric=case.rubric)
        results.append(
            EvalCaseResult(
                name=case.name,
                passed=verdict.passed,
                score=verdict.score,
                rationale=verdict.rationale,
            )
        )
    return EvalReport(results=results)


async def evolve_and_gate(
    *,
    model: ModelProvider,
    role: str,
    current_prompt: str,
    failure_traces: list[str],
    cases: list[PromptEvalCase],
    judge_model: ModelProvider | None = None,
    min_improvement: float = 0.0,
) -> EvolutionResult:
    """Propose an improved prompt; promote it only if it beats baseline by > ``min_improvement``.

    ``model`` proposes the candidate and plays the role under test; ``judge_model`` scores the
    outputs. Pass a DIFFERENT judge model where possible — a model grading its own proposal is a
    weak gate. Defaults to ``model`` only for convenience/offline use.
    """
    if not cases:
        raise ValueError("evolution gate needs at least one eval case (no evidence, no promotion)")
    if min_improvement < 0:
        raise ValueError("min_improvement must be >= 0")
    judge_model = judge_model or model

    candidate = await PromptEvolver(model).propose(
        role=role, current_prompt=current_prompt, failure_traces=failure_traces
    )
    if candidate.prompt.strip() == current_prompt.strip():
        return EvolutionResult(
            promoted=False,
            candidate=candidate,
            baseline_pass_rate=0.0,
            candidate_pass_rate=0.0,
            reason="candidate is identical to the current prompt; nothing to promote",
        )

    baseline = await evaluate_prompt(
        model=model, judge_model=judge_model, prompt=current_prompt, cases=cases
    )
    evolved = await evaluate_prompt(
        model=model, judge_model=judge_model, prompt=candidate.prompt, cases=cases
    )
    improvement = evolved.pass_rate - baseline.pass_rate
    promoted = improvement > min_improvement
    reason = (
        f"candidate improved pass rate by {improvement:+.3f} (> {min_improvement})"
        if promoted
        else f"candidate did not strictly beat baseline ({improvement:+.3f}); rejected"
    )
    return EvolutionResult(
        promoted=promoted,
        candidate=candidate,
        baseline_pass_rate=baseline.pass_rate,
        candidate_pass_rate=evolved.pass_rate,
        reason=reason,
    )
