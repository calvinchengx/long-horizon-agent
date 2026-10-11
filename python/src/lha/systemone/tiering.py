"""The System One difficulty question: is the item a small change or a large one?

An implementer is about to work one checklist item. This question asks a System One model how
much work the item is: a small, well-understood change, or one that needs exploration, several
files, new dependencies or packages, or design decisions. The answer is used only to make LHA
CHEAPER: a confident ``small`` routes the implementer to the next cheaper model tier (never a
stronger one), and nothing else changes -- the reviewer, lead and researcher tiers are untouched.

Only meaningful for the ``claude`` backend, where roles have tiers, and off by default
(``LHA_SYSTEM_ONE_TIERING``). Its threshold is unmeasured, so the ``system_one`` event records the
answer, its confidence and the threshold for a future gold set. A failed call changes nothing.

The question is worded after TypeSafe's guidance on Jev's weaknesses: neutral option names with
full descriptions, one judgment per question, and only the text the judgment needs (the task and
its witnesses).
"""

from __future__ import annotations

from pydantic import JsonValue

from lha.contracts.system_one import Choice
from lha.obs.redact import redact_text

QUESTION_ID = "difficulty"

CRITERIA: dict[str, str] = {
    "small": (
        "The task is a small, well-understood change: one file or a few lines, no new "
        "dependencies, packages or design decisions."
    ),
    "large": (
        "The task needs exploration, several files, new dependencies or packages, or design "
        "decisions."
    ),
}

QUESTION = Choice(
    instructions=(
        "The task `task` is about to be given to an implementer. Is it a small, well-understood "
        "change, or does it need exploration, several files, new dependencies or packages, or "
        "design decisions?"
    ),
    criteria=dict(CRITERIA),
)


def tiering_state(description: str, witnesses: list[str]) -> dict[str, JsonValue]:
    """What the model sees: the task (redacted) and the item's witnesses."""
    return {"task": redact_text(description), "witnesses": list(witnesses)}
