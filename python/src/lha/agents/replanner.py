"""The Replanner: split a blocked checklist item into smaller ones instead of deadlocking.

The Planner runs once, at intake. On a long mission some items turn out too coarse ("OneLake data
plane"): the lead fails verification ``max_consecutive_failures`` times in a row and the item
blocks, which — once its dependents are stuck behind it — ends the mission ``deadlocked``. The
Replanner asks the model to decompose the blocked item into 2-6 smaller, ordered steps, using the
harness's failure report as evidence; ``Checklist.split`` then replaces the item with children
``<id>.1 .. <id>.n``. The parent's witnesses move to the last child, so the original acceptance
still gates the result: splitting can make work tractable, never make it pass more easily.

Replanning is bounded by the caller (splits per mission, nesting depth); an unusable reply means
"no split" and the item stays blocked.
"""

from __future__ import annotations

from lha.agents.planner import parse_checklist
from lha.contracts.model import ModelMessage, ModelProvider
from lha.contracts.state import ChecklistItem

MAX_CHILDREN = 6
_FAILURE_CAP = 3000

_SYSTEM = (
    "You are the Planner. A checklist item failed deterministic verification repeatedly and was "
    "blocked. Break it into smaller steps that can each be completed and verified in one short "
    "work session, in dependency order. Each step must be self-contained (do not refer to "
    '"the item above"), concrete, and leave the code building and the tests passing. The last '
    "step must complete the original item."
)
_INSTRUCTIONS = (
    "Reply with ONLY a JSON array of 2-6 steps; each element: "
    '{"description": "<imperative step>"}. No prose, no code fences.'
)


class Replanner:
    """Turns a blocked item into ordered child drafts (``[]`` when no usable split is offered)."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def split(self, *, mission_text: str, item: ChecklistItem) -> list[ChecklistItem]:
        failure = item.last_failure[-_FAILURE_CAP:] or "(no failure report)"
        acceptance = (
            "Its acceptance checks (they will gate the LAST step): " + ", ".join(item.witnesses)
            if item.witnesses
            else "It has no item-specific acceptance checks."
        )
        messages = [
            ModelMessage(role="system", content=_SYSTEM),
            ModelMessage(
                role="user",
                content=(
                    f"{mission_text}\n\nBlocked item [{item.id}]: {item.description}\n"
                    f"{acceptance}\n\nIt failed {item.consecutive_failures} times in a row. "
                    f"Latest verification report:\n{failure}\n\n{_INSTRUCTIONS}"
                ),
            ),
        ]
        result = await self._model.complete(messages)
        drafts = [
            ChecklistItem(id=f"draft-{n}", description=d.description)
            for n, d in enumerate(parse_checklist(result.text), start=1)
            if d.description.strip()
        ]
        if len(drafts) < 2:
            return []
        return drafts[:MAX_CHILDREN]
