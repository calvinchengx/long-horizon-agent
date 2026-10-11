"""The model-backed pre-review screen: a System One model asked the pre-review question.

The deterministic screen (``lha.verify.review_screen``) reads a verified diff for the weakened-test
patterns it knows. This screen asks a System One model the pre-review question
(``lha.systemone.review``) on the same diff and, on a confident ``block``, adds one finding so the
diff gets the full review exactly as the deterministic findings do. It can only make LHA more
cautious: a ``block`` is one more review, never an approval, and it never skips the reviewer.

Off by default (``LHA_SYSTEM_ONE_REVIEW``): it needs a System One backend (like triage), and its
threshold is unmeasured until the ``--judge system_one`` scorecard says what it should be.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

from lha.contracts.system_one import ChoiceAnswer, SystemOneError, SystemOneModel
from lha.systemone.review import QUESTION, QUESTION_ID, review_state

#: The anchor event each model-backed screen is recorded as (the schema's ``use`` says which use).
SYSTEM_ONE_EVENT = "system_one"
#: The ``use`` this screen writes into its ``system_one`` event.
USE = "pre_review"

ScreenAction = Literal["continue", "block"]


@dataclass
class ScreenVerdict:
    """The model's answer and what it changed (``asked`` is False when the diff was empty)."""

    action: ScreenAction = "continue"
    answer: str = ""
    confidence: float = 0.0
    probabilities: dict[str, float] = field(default_factory=dict)
    model: str = ""
    error: str = ""
    asked: bool = True

    def findings(self) -> list[str]:
        """The finding a confident ``block`` adds (nothing otherwise)."""
        if self.action == "block":
            return [f"system_one: block (confidence {self.confidence:.2f})"]
        return []

    def payload(self, item_id: str, threshold: float) -> dict[str, object]:
        """The ``system_one`` event recorded with the review (the shape triage's event has)."""
        return {
            "use": USE,
            "item_id": item_id,
            "model": self.model,
            "answer": self.answer,
            "confidence": self.confidence,
            "probabilities": dict(self.probabilities),
            "threshold": threshold,
            "action": self.action,
            "error": self.error,
        }


class ReviewScreen:
    """Asks ``model`` the pre-review question on a verified diff, and blocks only when confident."""

    def __init__(self, model: SystemOneModel, *, threshold: float = 0.9) -> None:
        self.model = model
        self.threshold = threshold

    async def assess(self, diff: str, task: str) -> ScreenVerdict:
        """The model's answer for ``diff`` (no call on an empty diff; a failure changes nothing)."""
        if not diff:
            return ScreenVerdict(asked=False)
        state = review_state(task, diff)
        try:
            result = await self.model.evaluate(state, {QUESTION_ID: QUESTION})
        except SystemOneError as exc:
            return ScreenVerdict(error=str(exc))
        answer = result.answers[QUESTION_ID]
        if not isinstance(answer, ChoiceAnswer):  # parse_response guarantees it; be explicit
            return ScreenVerdict(error="the answer is not a choice")
        blocked = answer.choice == "block" and answer.confidence >= self.threshold
        return ScreenVerdict(
            action="block" if blocked else "continue",
            answer=answer.choice,
            confidence=answer.confidence,
            probabilities=dict(answer.probabilities),
            model=result.model,
        )

    async def findings(self, diff: str, task: str) -> list[str]:
        """The finding a confident ``block`` adds, or ``[]``."""
        return (await self.assess(diff, task)).findings()
