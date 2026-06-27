"""The Planner: mission intake (a task description → a verifiable checklist).

Asks the model to decompose the mission into small, ordered, independently-verifiable items as a
JSON array. Parsing is robust and never crashes: if the model returns nothing usable, it falls
back to a single item built from the description, so a mission can always start.
"""

from __future__ import annotations

import json
import re

from lha.agents.roles import ROLES
from lha.contracts.model import ModelMessage, ModelProvider
from lha.contracts.state import Checklist, ChecklistItem

_PLANNER_INSTRUCTIONS = (
    "Decompose the mission into 3-15 small, ordered, independently-verifiable steps.\n"
    "Reply with ONLY a JSON array; each element: "
    '{"description": "<imperative step>", "depends_on": [<1-based numbers of EARLIER steps>], '
    '"allow_harness_edits": <true only if the step must modify EXISTING tests or test config>}.\n'
    "No prose, no code fences."
)

_DIGITS_RE = re.compile(r"\d+")


def _normalize_dep(raw: object) -> int | None:
    """Map a model-written dependency ("1", "01", 1, "step 1", "#1") to a 1-based index."""
    if isinstance(raw, bool):
        return None
    if isinstance(raw, int):
        return raw
    if isinstance(raw, float) and raw.is_integer():
        return int(raw)
    if isinstance(raw, str):
        found = _DIGITS_RE.findall(raw)
        if len(found) == 1:
            return int(found[0])
    return None


def parse_checklist(text: str) -> list[ChecklistItem]:
    """Extract a checklist from model output; assign zero-padded ids; tolerate messy output.

    Dependencies are normalized to the assigned ids. Only references to EARLIER steps are kept
    (which also makes cycles impossible); unknown / forward / self references are dropped and
    recorded in the item's ``notes`` so a bad plan can never leave items unreachable.
    """
    start = text.find("[")
    end = text.rfind("]")
    if start == -1 or end == -1 or end <= start:
        return []
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return []
    if not isinstance(parsed, list):
        return []

    # (original 1-based position, description, raw deps, allow_harness_edits)
    entries: list[tuple[int, str, list[object], bool]] = []
    for position, entry in enumerate(parsed, start=1):
        if isinstance(entry, dict):
            description = str(entry.get("description") or entry.get("step") or "").strip()
            raw_deps = entry.get("depends_on", [])
            deps: list[object] = list(raw_deps) if isinstance(raw_deps, list) else [raw_deps]
            allow = entry.get("allow_harness_edits") is True
        else:
            description, deps, allow = str(entry).strip(), [], False
        if description:
            entries.append((position, description, deps, allow))

    # The model's indices refer to ITS list positions; map them to the ids we assign.
    width = max(2, len(str(len(entries))))
    id_for = {pos: f"{n:0{width}d}" for n, (pos, *_rest) in enumerate(entries, start=1)}
    items: list[ChecklistItem] = []
    for position, description, raw_deps, allow in entries:
        kept: list[str] = []
        dropped: list[str] = []
        for raw in raw_deps:
            index = _normalize_dep(raw)
            if index is not None and index < position and index in id_for:
                if id_for[index] not in kept:
                    kept.append(id_for[index])
            elif raw is not None and raw != "":
                dropped.append(str(raw))
        items.append(
            ChecklistItem(
                id=id_for[position],
                description=description,
                depends_on=kept,
                allow_harness_edits=allow,
                notes=f"planner dropped invalid depends_on: {dropped}" if dropped else "",
            )
        )
    return items


class Planner:
    """Turns a mission into a checklist using the model (with a safe fallback)."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def plan(self, *, title: str, description: str, acceptance: str = "") -> Checklist:
        messages = [
            ModelMessage(role="system", content=ROLES["planner"].system_prompt),
            ModelMessage(
                role="user",
                content=(
                    f"Mission: {title}\n\n{description}\n\n"
                    f"Definition of done: {acceptance or '(use your judgment)'}\n\n"
                    f"{_PLANNER_INSTRUCTIONS}"
                ),
            ),
        ]
        result = await self._model.complete(messages)
        items = parse_checklist(result.text)
        if not items:
            # Never fail to produce a plan; degrade to a single item.
            items = [ChecklistItem(id="01", description=description.strip() or title.strip())]
        return Checklist(items=items)
