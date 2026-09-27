"""The ``POST /v1/systemone`` wire format: request bodies, strict answer parsing, confidence.

Pure functions, shared with the Go implementation through ``spec/systemone/wire.json``. Parsing
is strict because LHA acts on these numbers: an answer missing, of the wrong type, naming an
option that was not asked, or with probabilities that do not form a distribution is an error
(``SystemOneError``), never a default.
"""

from __future__ import annotations

import math

from pydantic import JsonValue

from lha.contracts.system_one import (
    Answer,
    Choice,
    ChoiceAnswer,
    Noul,
    NoulAnswer,
    Question,
    Score,
    ScoreAnswer,
    SystemOneError,
    SystemOneResult,
)

#: API limits (TypeSafe's published limits; Kev accepts more, so they are checked client-side).
MAX_CHOICE_OPTIONS = 255
MIN_SCORE_LEVELS = 2
MAX_SCORE_LEVELS = 10
#: How far a distribution's sum may be from 1 (servers round probabilities, Kev to 2 decimals).
SUM_TOLERANCE = 0.05


def question_body(question: Question) -> dict[str, JsonValue]:
    """One question as it goes on the wire (a ``noul`` without criteria omits the field)."""
    body: dict[str, JsonValue] = {"type": question.type, "instructions": question.instructions}
    if question.criteria is not None:
        body["criteria"] = question.criteria
    return body


def check_question(qid: str, question: Question) -> None:
    """Raise ``SystemOneError`` for a question the API would refuse (checked before sending)."""
    if isinstance(question, Choice) and not 1 <= len(question.criteria) <= MAX_CHOICE_OPTIONS:
        raise SystemOneError(f"question {qid!r}: a choice needs 1-{MAX_CHOICE_OPTIONS} options")
    if isinstance(question, Score) and not (
        MIN_SCORE_LEVELS <= len(question.criteria) <= MAX_SCORE_LEVELS
    ):
        raise SystemOneError(
            f"question {qid!r}: a score needs {MIN_SCORE_LEVELS}-{MAX_SCORE_LEVELS} levels"
        )


def request_body(
    model: str, state: JsonValue, questions: dict[str, Question]
) -> dict[str, JsonValue]:
    """The JSON body of one evaluation request."""
    if not questions:
        raise SystemOneError("an evaluation needs at least one question")
    for qid, question in questions.items():
        check_question(qid, question)
    return {
        "model": model,
        "state": state,
        "questions": {qid: question_body(q) for qid, q in questions.items()},
    }


def choice_confidence(probabilities: list[float]) -> float:
    """``(p_max - 1/K) / (1 - 1/K)``: 1 when one option has everything, 0 when uniform."""
    k = len(probabilities)
    if k <= 1:
        return 1.0
    return max(0.0, min(1.0, (max(probabilities) - 1 / k) / (1 - 1 / k)))


def score_confidence(probabilities: list[float]) -> float:
    """``max(0, 1 - E|level - mode| / D)``, with ``D`` the mean distance of a uniform
    distribution over the levels from its middle (2/3 for three levels)."""
    n = len(probabilities)
    if n <= 1:
        return 1.0
    mode = max(range(n), key=lambda i: probabilities[i])
    spread = sum(p * abs(i - mode) for i, p in enumerate(probabilities))
    middle = (n - 1) / 2
    uniform = sum(abs(i - middle) for i in range(n)) / n
    return max(0.0, 1 - spread / uniform)


def _number(value: object, what: str, *, low: float = 0.0, high: float = 1.0) -> float:
    if isinstance(value, bool) or not isinstance(value, int | float):
        raise SystemOneError(f"{what} is not a number: {value!r}")
    number = float(value)
    if not math.isfinite(number) or not low <= number <= high:
        raise SystemOneError(f"{what} is outside [{low:g}, {high:g}]: {value!r}")
    return number


def _distribution(value: object, keys: list[str], what: str) -> dict[str, float]:
    if not isinstance(value, dict):
        raise SystemOneError(f"{what} probabilities are not an object")
    if sorted(value) != sorted(keys):
        raise SystemOneError(f"{what} probabilities name {sorted(value)}, expected {sorted(keys)}")
    probs = {k: _number(value[k], f"{what} probability of {k!r}") for k in keys}
    if abs(sum(probs.values()) - 1.0) > SUM_TOLERANCE:
        raise SystemOneError(f"{what} probabilities sum to {sum(probs.values()):.4f}, not 1")
    return probs


def parse_answer(qid: str, question: Question, raw: object) -> Answer:
    """One answer, checked against the question that was asked."""
    what = f"answer {qid!r}"
    if not isinstance(raw, dict):
        raise SystemOneError(f"{what} is not an object")
    if raw.get("type") != question.type:
        raise SystemOneError(f"{what} has type {raw.get('type')!r}, expected {question.type!r}")
    if isinstance(question, Noul):
        return NoulAnswer(noul=_number(raw.get("noul"), f"{what} noul"))
    if isinstance(question, Choice):
        options = list(question.criteria)
        probs = _distribution(raw.get("probabilities"), options, what)
        choice = raw.get("choice")
        if not isinstance(choice, str) or choice not in question.criteria:
            raise SystemOneError(f"{what} chose {choice!r}, not one of the options")
        confidence = _number(raw.get("confidence"), f"{what} confidence")
        return ChoiceAnswer(choice=choice, probabilities=probs, confidence=confidence)
    levels = [str(i) for i in range(len(question.criteria))]
    probs = _distribution(raw.get("probabilities"), levels, what)
    score = _number(raw.get("score"), f"{what} score", high=float(len(levels) - 1))
    confidence = _number(raw.get("confidence"), f"{what} confidence")
    return ScoreAnswer(score=score, probabilities=probs, confidence=confidence)


def _tokens(usage: dict[str, object], key: str) -> int:
    value = usage.get(key, 0)
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise SystemOneError(f"usage {key} is not a non-negative integer: {value!r}")
    return value


def parse_response(data: object, questions: dict[str, Question]) -> SystemOneResult:
    """A response body, checked against the questions asked (extra answers are ignored)."""
    if not isinstance(data, dict):
        raise SystemOneError("the response is not an object")
    answers = data.get("answers")
    if not isinstance(answers, dict):
        raise SystemOneError("the response has no answers object")
    model = data.get("model", "")
    if not isinstance(model, str):
        raise SystemOneError(f"the response model is not a string: {model!r}")
    usage = data.get("usage")
    if usage is None:
        usage = {}
    if not isinstance(usage, dict):
        raise SystemOneError("the response usage is not an object")
    parsed: dict[str, Answer] = {}
    for qid, question in questions.items():
        if qid not in answers:
            raise SystemOneError(f"answer {qid!r} is missing")
        parsed[qid] = parse_answer(qid, question, answers[qid])
    return SystemOneResult(
        model=model,
        answers=parsed,
        input_tokens=_tokens(usage, "input_tokens"),
        output_tokens=_tokens(usage, "output_tokens"),
    )


def question_from_wire(raw: object) -> Question:
    """A question from its wire form (the inverse of ``question_body``)."""
    if not isinstance(raw, dict):
        raise SystemOneError("a question is not an object")
    kind = raw.get("type")
    if kind == "noul":
        return Noul.model_validate(raw)
    if kind == "choice":
        return Choice.model_validate(raw)
    if kind == "score":
        return Score.model_validate(raw)
    raise SystemOneError(f"unknown question type {kind!r}")
