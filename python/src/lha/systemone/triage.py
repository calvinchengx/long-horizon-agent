"""Stall triage: after repeated failures, ask a System One model WHY an item keeps failing.

Without triage an item is retried until ``max_consecutive_failures`` and only then blocked (and
split by the replanner). Triage asks one ``Choice`` about the latest failure and the one before
it, and acts only on a confident answer:

- ``scope``: the item needs several distinct pieces of work, so split it now (if the replanner
  may) instead of spending more attempts on it whole;
- ``environment``: the failure is outside the code (a missing tool, network, permissions), so
  block it now, for a human, instead of retrying something another attempt cannot fix;
- ``defect`` (or any answer below the threshold): carry on exactly as without triage.

Both actions only STOP work on an item earlier; neither can mark anything done (the parent's
witnesses still gate the last child of a split). A failed call means ``continue``.

The question is worded after TypeSafe's guidance on Jev's weaknesses: neutral option names with
full descriptions, one judgment per question, and only the text the judgment needs.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

from pydantic import JsonValue

from lha.contracts.state import ChecklistItem
from lha.contracts.system_one import Choice, ChoiceAnswer, SystemOneError, SystemOneModel
from lha.obs.redact import redact_text

TriageAction = Literal["continue", "split", "block"]

QUESTION_ID = "cause"
#: Characters of each failure report sent (the end of a report carries the errors).
MAX_FAILURE_CHARS = 3000

CAUSES: dict[str, str] = {
    "defect": (
        "The code written for the task has a specific bug or omission that one more focused "
        "attempt can fix."
    ),
    "scope": (
        "The task needs several distinct pieces of work, for example new files, packages or "
        "components that do not exist yet, and the attempts only got part of the way."
    ),
    "environment": (
        "The failure comes from outside the code: a missing tool or dependency, no network "
        "access, permissions, disk or memory limits, or a broken check command."
    ),
}

QUESTION = Choice(
    instructions=(
        "The attempts at `task` failed their checks, most recently with `latest_failure` and "
        "before that with `previous_failure`. Which option best describes why they fail?"
    ),
    criteria=dict(CAUSES),
)


def _tail(text: str, limit: int) -> str:
    text = redact_text(text)
    return text if len(text) <= limit else "..." + text[-limit:]


def triage_state(
    item: ChecklistItem,
    latest_failure: str,
    previous_failure: str,
    *,
    max_chars: int = MAX_FAILURE_CHARS,
) -> dict[str, JsonValue]:
    """What the model sees: the task, its witnesses and the two failures (redacted, tails)."""
    return {
        "task": redact_text(item.description),
        "witnesses": list(item.witnesses),
        "latest_failure": _tail(latest_failure, max_chars),
        "previous_failure": _tail(previous_failure, max_chars),
    }


def triage_action(answer: ChoiceAnswer, *, threshold: float, can_split: bool) -> TriageAction:
    """The action for an answer: only a confident ``scope`` or ``environment`` changes anything."""
    if answer.confidence < threshold:
        return "continue"
    if answer.choice == "scope" and can_split:
        return "split"
    if answer.choice == "environment":
        return "block"
    return "continue"


@dataclass
class TriageVerdict:
    action: TriageAction
    cause: str = ""
    confidence: float = 0.0
    probabilities: dict[str, float] = field(default_factory=dict)
    model: str = ""
    error: str = ""

    def payload(self, item_id: str, threshold: float) -> dict[str, object]:
        """The ``system_one`` event recorded with the cycle."""
        return {
            "use": "stall_triage",
            "item_id": item_id,
            "model": self.model,
            "answer": self.cause,
            "confidence": self.confidence,
            "probabilities": dict(self.probabilities),
            "threshold": threshold,
            "action": self.action,
            "error": self.error,
        }


class StallTriage:
    """Asks ``model`` why an item keeps failing, once it has failed ``min_failures`` in a row."""

    def __init__(
        self, model: SystemOneModel, *, threshold: float = 0.9, min_failures: int = 2
    ) -> None:
        self.model = model
        self.threshold = threshold
        self.min_failures = min_failures

    def applies(self, item: ChecklistItem) -> bool:
        return item.status == "in_progress" and item.consecutive_failures >= self.min_failures

    async def assess(
        self, item: ChecklistItem, latest_failure: str, previous_failure: str, *, can_split: bool
    ) -> TriageVerdict:
        state = triage_state(item, latest_failure, previous_failure)
        try:
            result = await self.model.evaluate(state, {QUESTION_ID: QUESTION})
        except SystemOneError as exc:
            return TriageVerdict(action="continue", error=str(exc))
        answer = result.answers[QUESTION_ID]
        if not isinstance(answer, ChoiceAnswer):  # parse_response guarantees it; be explicit
            return TriageVerdict(action="continue", error="the answer is not a choice")
        return TriageVerdict(
            action=triage_action(answer, threshold=self.threshold, can_split=can_split),
            cause=answer.choice,
            confidence=answer.confidence,
            probabilities=dict(answer.probabilities),
            model=result.model,
        )
