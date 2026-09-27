"""An offline System One model for tests: scripted answers, or a failure, for every call."""

from __future__ import annotations

from collections.abc import Callable

from pydantic import JsonValue

from lha.contracts.system_one import (
    Answer,
    Choice,
    ChoiceAnswer,
    Noul,
    NoulAnswer,
    Question,
    ScoreAnswer,
    SystemOneError,
    SystemOneResult,
)
from lha.systemone.wire import choice_confidence, score_confidence

Responder = Callable[[JsonValue, dict[str, Question]], dict[str, Answer]]


def choice_answer(probabilities: dict[str, float]) -> ChoiceAnswer:
    """A ``ChoiceAnswer`` for a distribution (the most likely option, standard confidence)."""
    return ChoiceAnswer(
        choice=max(probabilities, key=lambda k: probabilities[k]),
        probabilities=dict(probabilities),
        confidence=choice_confidence(list(probabilities.values())),
    )


def score_answer(probabilities: list[float]) -> ScoreAnswer:
    return ScoreAnswer(
        score=sum(i * p for i, p in enumerate(probabilities)),
        probabilities={str(i): p for i, p in enumerate(probabilities)},
        confidence=score_confidence(probabilities),
    )


def _neutral(question: Question) -> Answer:
    if isinstance(question, Noul):
        return NoulAnswer(noul=0.5)
    if isinstance(question, Choice):
        n = len(question.criteria)
        return choice_answer(dict.fromkeys(question.criteria, 1 / n))
    n = len(question.criteria)
    return score_answer([1 / n] * n)


class StubSystemOne:
    """Answers with ``respond(state, questions)`` (default: uniform, i.e. zero confidence)."""

    name = "systemone:stub"

    def __init__(self, respond: Responder | None = None, *, error: str = "") -> None:
        self._respond = respond
        self._error = error
        self.calls: list[tuple[JsonValue, dict[str, Question]]] = []

    async def evaluate(self, state: JsonValue, questions: dict[str, Question]) -> SystemOneResult:
        self.calls.append((state, questions))
        if self._error:
            raise SystemOneError(self._error)
        answers = self._respond(state, questions) if self._respond else {}
        return SystemOneResult(
            model="stub-1",
            answers={qid: answers.get(qid) or _neutral(q) for qid, q in questions.items()},
        )
