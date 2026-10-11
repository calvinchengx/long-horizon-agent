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
from lha.systemone.commands import QUESTION as COMMAND_QUESTION
from lha.systemone.commands import QUESTION_ID as COMMAND_QUESTION_ID
from lha.systemone.commands import command_state
from lha.systemone.review import QUESTION, QUESTION_ID, review_state

#: The anchor event each model-backed screen is recorded as (the schema's ``use`` says which use).
SYSTEM_ONE_EVENT = "system_one"
#: The ``use`` this screen writes into its ``system_one`` event.
USE = "pre_review"
#: The ``use`` the command second opinion writes into its ``system_one`` event.
COMMAND_USE = "command_second_opinion"

ScreenAction = Literal["continue", "block"]
CommandAction = Literal["continue", "gate"]


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


@dataclass
class CommandVerdict:
    """The model's answer about one command and what it changed."""

    action: CommandAction = "continue"
    answer: str = ""
    confidence: float = 0.0
    probabilities: dict[str, float] = field(default_factory=dict)
    model: str = ""
    error: str = ""

    def reason(self) -> str | None:
        """The gate reason a confident ``dangerous`` gives (``None`` otherwise)."""
        if self.action == "gate":
            return f"the command looks dangerous (system one, confidence {self.confidence:.2f})"
        return None

    def payload(self, threshold: float) -> dict[str, object]:
        """The ``system_one`` event recorded with the call (the shape the other screens record)."""
        return {
            "use": COMMAND_USE,
            "model": self.model,
            "answer": self.answer,
            "confidence": self.confidence,
            "probabilities": dict(self.probabilities),
            "threshold": threshold,
            "action": self.action,
            "error": self.error,
        }


class CommandSecondOpinion:
    """Asks ``model`` whether a command the classifier allowed is dangerous, and gates only then.

    The dispatcher calls this ONLY for a ``run_command`` the classifier allowed. A confident
    ``dangerous`` returns a reason string, which the dispatcher uses as the gate reason: the call
    goes to the human gate exactly as an irreversible command is, or is denied when there is no
    gate. It can only add gating, never allow a denied command and never change the classifier.
    """

    def __init__(self, model: SystemOneModel, *, threshold: float = 0.9) -> None:
        self.model = model
        self.threshold = threshold

    async def assess(self, command: list[str]) -> CommandVerdict:
        """The model's answer for ``command`` (a failure changes nothing)."""
        state = command_state(command)
        try:
            result = await self.model.evaluate(state, {COMMAND_QUESTION_ID: COMMAND_QUESTION})
        except SystemOneError as exc:
            return CommandVerdict(error=str(exc))
        answer = result.answers[COMMAND_QUESTION_ID]
        if not isinstance(answer, ChoiceAnswer):  # parse_response guarantees it; be explicit
            return CommandVerdict(error="the answer is not a choice")
        gated = answer.choice == "dangerous" and answer.confidence >= self.threshold
        return CommandVerdict(
            action="gate" if gated else "continue",
            answer=answer.choice,
            confidence=answer.confidence,
            probabilities=dict(answer.probabilities),
            model=result.model,
        )

    async def reason(self, command: list[str]) -> str | None:
        """The gate reason a confident ``dangerous`` gives, or ``None``."""
        return (await self.assess(command)).reason()
