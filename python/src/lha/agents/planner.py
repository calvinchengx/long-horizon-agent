"""The Planner: mission intake (a task description → a verifiable checklist + file ownership).

Asks the model to decompose the mission into small, ordered, independently-verifiable items as a
JSON array, each naming the files it will create or modify. Parsing is robust and never crashes:
if the model returns nothing usable, it falls back to a single item built from the description, so
a mission can always start.

Ownership (``assign_ownership``): each item's declared files go to that item's implementer
(``writer_for_item``) — all or nothing — so items with disjoint write-sets can run in parallel.
An item is kept SERIAL (no write-set; the lead does it) when it declares no files, touches a
shared file (manifests, lockfiles, ``__init__.py``, ...), or declares an invalid path. An item
that overlaps files an EARLIER item declared is serial too, and is made to depend on that item,
so the two never run at once and the earlier item's lease is released before the later one runs.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field

from lha.agents.roles import ROLES
from lha.contracts.model import ModelMessage, ModelProvider
from lha.contracts.state import Checklist, ChecklistItem
from lha.coordination.ownership import (
    FileOwnershipMap,
    InvalidPathError,
    _normalize,
    is_shared,
    writer_for_item,
)
from lha.verify.witnesses import validate_witness

_PLANNER_INSTRUCTIONS = (
    "Decompose the mission into 3-15 small, ordered, independently-verifiable steps.\n"
    "Reply with ONLY a JSON array; each element: "
    '{"description": "<imperative step>", "depends_on": [<1-based numbers of EARLIER steps>], '
    '"files": [<repo-relative paths this step creates or modifies>], '
    '"allow_harness_edits": <true only if the step must modify EXISTING tests or test config>, '
    '"witnesses": [<optional: checks proving THIS step is delivered, each "pytest:<node id>", '
    '"go:TestName" or "cmd:<shell command that exits 0>"; they gate the step on top of the '
    "mission checks>]}.\n"
    "Steps whose files do not overlap can be built in parallel, each by its own writer; list "
    "every file a step writes, and keep shared files (pyproject.toml, lockfiles, __init__.py, "
    "conftest.py, migrations) in as few steps as possible.\n"
    "No prose, no code fences."
)

_DIGITS_RE = re.compile(r"\d+")
# Harness-owned directories: never part of a plan's write-set.
_HARNESS_DIRS = (".lha", ".git")


@dataclass
class MissionPlan:
    """What the Planner produced: the checklist, the ownership map, and each item's files."""

    checklist: Checklist
    ownership: FileOwnershipMap
    files: dict[str, list[str]] = field(default_factory=dict)  # item id -> declared files


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


def _files_of(entry: dict[str, object]) -> list[str]:
    raw = entry.get("files", [])
    values = raw if isinstance(raw, list) else [raw]
    out: list[str] = []
    for value in values:
        if isinstance(value, str) and value.strip() and value.strip() not in out:
            out.append(value.strip())
    return out


def _witnesses_of(entry: dict[str, object]) -> tuple[list[str], list[str]]:
    """The entry's valid ``witnesses`` and the ones dropped (as ``"<witness>: <why>"``).

    A Planner witness may only ADD a gate the sandbox can run (``pytest:``, ``go:``, ``cmd:``):
    a ``trusted:`` check runs outside the sandbox and stays the operator's to name.
    """
    raw = entry.get("witnesses", [])
    values = raw if isinstance(raw, list) else [raw]
    kept: list[str] = []
    dropped: list[str] = []
    for value in values:
        if not isinstance(value, str) or not value.strip():
            continue
        witness = value.strip()
        scheme = witness.partition(":")[0]
        if scheme in ("trusted", "ci"):
            dropped.append(f"{witness}: only the operator may name a trusted check")
            continue
        try:
            validate_witness(witness)
        except ValueError as exc:
            dropped.append(f"{witness}: {exc}")
            continue
        if witness not in kept:
            kept.append(witness)
    return kept, dropped


def parse_plan(text: str) -> tuple[list[ChecklistItem], dict[str, list[str]]]:
    """Extract checklist items and each item's declared ``files`` from model output.

    Ids are zero-padded; dependencies are normalized to the assigned ids. Only references to
    EARLIER steps are kept (which also makes cycles impossible); unknown / forward / self
    references are dropped and recorded in the item's ``notes`` so a bad plan can never leave
    items unreachable. The files are returned as written (``assign_ownership`` validates them).
    """
    start = text.find("[")
    end = text.rfind("]")
    if start == -1 or end == -1 or end <= start:
        return [], {}
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return [], {}
    if not isinstance(parsed, list):
        return [], {}

    # (original 1-based position, description, raw deps, allow_harness_edits, files, witnesses
    # kept, witnesses dropped)
    entries: list[tuple[int, str, list[object], bool, list[str], list[str], list[str]]] = []
    for position, entry in enumerate(parsed, start=1):
        if isinstance(entry, dict):
            description = str(entry.get("description") or entry.get("step") or "").strip()
            raw_deps = entry.get("depends_on", [])
            deps: list[object] = list(raw_deps) if isinstance(raw_deps, list) else [raw_deps]
            allow = entry.get("allow_harness_edits") is True
            files = _files_of(entry)
            witnesses, bad_witnesses = _witnesses_of(entry)
        else:
            description, deps, allow, files = str(entry).strip(), [], False, []
            witnesses, bad_witnesses = [], []
        if description:
            entries.append((position, description, deps, allow, files, witnesses, bad_witnesses))

    # The model's indices refer to ITS list positions; map them to the ids we assign.
    width = max(2, len(str(len(entries))))
    id_for = {pos: f"{n:0{width}d}" for n, (pos, *_rest) in enumerate(entries, start=1)}
    items: list[ChecklistItem] = []
    files_by_item: dict[str, list[str]] = {}
    for position, description, raw_deps, allow, files, witnesses, bad_witnesses in entries:
        kept: list[str] = []
        dropped: list[str] = []
        for raw in raw_deps:
            index = _normalize_dep(raw)
            if index is not None and index < position and index in id_for:
                if id_for[index] not in kept:
                    kept.append(id_for[index])
            elif raw is not None and raw != "":
                dropped.append(str(raw))
        notes: list[str] = []
        if dropped:
            notes.append(f"planner dropped invalid depends_on: {dropped}")
        if bad_witnesses:
            notes.append(f"planner dropped invalid witnesses: {bad_witnesses}")
        items.append(
            ChecklistItem(
                id=id_for[position],
                description=description,
                depends_on=kept,
                allow_harness_edits=allow,
                witnesses=witnesses,
                notes="; ".join(notes),
            )
        )
        if files:
            files_by_item[id_for[position]] = files
    return items, files_by_item


def parse_checklist(text: str) -> list[ChecklistItem]:
    """Extract a checklist from model output (``parse_plan`` without the files)."""
    return parse_plan(text)[0]


def _add_note(item: ChecklistItem, note: str) -> None:
    item.notes = f"{item.notes}; {note}" if item.notes else note


def assign_ownership(
    items: list[ChecklistItem], files_by_item: dict[str, list[str]]
) -> FileOwnershipMap:
    """Give each item's declared files to its implementer, or keep the item serial.

    Mutates ``items`` in place: serial items get a ``notes`` entry saying why, and an item that
    overlaps an earlier item's files gains a ``depends_on`` edge to it (earlier items only, so no
    cycle can appear). See the module docstring for the rules.
    """
    ownership = FileOwnershipMap()
    claimed: dict[str, str] = {}  # case-folded path -> id of the first item that declared it
    for item in items:
        declared = files_by_item.get(item.id, [])
        if not declared:
            continue
        keys: list[str] = []
        invalid: list[str] = []
        for path in declared:
            try:
                norm = _normalize(path)
            except InvalidPathError:
                invalid.append(path)
                continue
            if norm.split("/", 1)[0] in _HARNESS_DIRS:
                invalid.append(path)
                continue
            if norm.casefold() not in keys:
                keys.append(norm.casefold())
        shared = [k for k in keys if is_shared(k)]
        overlaps = sorted({claimed[k] for k in keys if k in claimed})
        for key in keys:
            claimed.setdefault(key, item.id)
        if invalid:
            _add_note(item, f"serial: invalid file paths {invalid}")
        elif shared:
            _add_note(item, f"serial: touches shared files {shared}")
        elif overlaps:
            for other in overlaps:
                if other not in item.depends_on:
                    item.depends_on.append(other)
            _add_note(item, f"serial after {', '.join(overlaps)}: overlapping files")
        else:
            writer = writer_for_item(item.id)
            for key in keys:
                ownership.assign(key, writer)
    return ownership


class Planner:
    """Turns a mission into a checklist (and file ownership) using the model, with a fallback."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def plan_mission(
        self, *, title: str, description: str, acceptance: str = ""
    ) -> MissionPlan:
        """Plan the checklist and assign single-writer file ownership from the declared files."""
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
        items, files = parse_plan(result.text)
        if not items:
            # Never fail to produce a plan; degrade to a single (serial) item.
            items = [ChecklistItem(id="01", description=description.strip() or title.strip())]
            files = {}
        ownership = assign_ownership(items, files)
        return MissionPlan(checklist=Checklist(items=items), ownership=ownership, files=files)

    async def plan(self, *, title: str, description: str, acceptance: str = "") -> Checklist:
        """The checklist alone (``plan_mission`` without the ownership map)."""
        plan = await self.plan_mission(title=title, description=description, acceptance=acceptance)
        return plan.checklist
