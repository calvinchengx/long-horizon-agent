"""Agent-as-a-Judge — calibrated LLM scoring for fuzzy criteria and evolution gating.

Used (a) as an ADVISORY online judge over sampled traces, and (b) as the promotion gate for
offline-evolved prompts/skills (alongside the deterministic verifier + replay). Deliberately
independent of and unwritable-by the agents it judges. Returns a structured verdict; defaults to
fail on unparseable output (never silently 'passes').
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass

from lha.contracts.model import ModelMessage, ModelProvider


@dataclass
class Verdict:
    passed: bool
    score: float
    rationale: str


def extract_json_object(text: str) -> dict[str, object] | None:
    """The outermost ``{...}`` in ``text`` parsed as a JSON object, or ``None``."""
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end == -1 or end < start:
        return None
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return None
    return parsed if isinstance(parsed, dict) else None


class AgentAsJudge:
    """Scores a candidate against a rubric using an independent model."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def judge(self, *, candidate: str, rubric: str) -> Verdict:
        result = await self._model.complete(
            [
                ModelMessage(
                    role="system",
                    content=(
                        "You are an impartial judge. Reply ONLY with JSON: "
                        '{"pass": <bool>, "score": <0..1>, "rationale": "<short>"}.'
                    ),
                ),
                ModelMessage(
                    role="user", content=f"Rubric:\n{rubric}\n\nCandidate:\n{candidate[:8000]}"
                ),
            ]
        )
        return parse_verdict(result.text)


def parse_verdict(text: str) -> Verdict:
    """Strictly validate a judge reply; anything malformed is a FAIL (never a silent pass).

    ``pass`` must be a JSON boolean (the string ``"false"`` is invalid, not truthy); ``score`` must
    be a finite JSON number and is clamped to ``[0, 1]``.
    """
    obj = extract_json_object(text)
    if obj is None:
        return Verdict(passed=False, score=0.0, rationale="unparseable judge output")
    raw_pass = obj.get("pass")
    if not isinstance(raw_pass, bool):
        return Verdict(
            passed=False, score=0.0, rationale=f"invalid judge output: pass={raw_pass!r}"
        )
    raw_score = obj.get("score", 0.0)
    if isinstance(raw_score, bool) or not isinstance(raw_score, int | float):
        return Verdict(
            passed=False, score=0.0, rationale=f"invalid judge output: score={raw_score!r}"
        )
    score = float(raw_score)
    if not math.isfinite(score):
        return Verdict(passed=False, score=0.0, rationale="invalid judge output: non-finite score")
    return Verdict(
        passed=raw_pass,
        score=min(1.0, max(0.0, score)),
        rationale=str(obj.get("rationale", "")),
    )
