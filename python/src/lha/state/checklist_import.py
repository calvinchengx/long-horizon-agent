"""Seeding a mission with the operator's own checklist instead of the Planner's.

Two formats, chosen by file extension:

* ``.json`` — either a ``Checklist`` dump (``{"items": [...]}`` with ids) or a mission file::

      {"title": "...", "description": "...", "references": ["docs/api.md"],
       "items": [{"description": "...", "id": "01", "depends_on": [], "witnesses": ["go:TestX"],
                  "allow_harness_edits": false}]}

  Items without an ``id`` get zero-padded sequential ids (``01``, ``02``, ... by position).

* ``.md`` — a Markdown roadmap (the shape of fabric-emulator's ``docs/13-roadmap.md``): the first
  ``# `` heading is the title; paragraph text before the first ``## `` is the description; each
  ``## `` section is a phase; each ``- [ ] text`` line is an item (``- [x]`` too, only with
  ``include_done=True``, imported as ``todo`` so the harness re-verifies it). Lines indented under
  a checkbox continue its description; nested bullets that are not checkboxes are ignored.
  Witnesses are declared inline — ``(witness: go:TestX)`` or ``(witnesses: go:TestX, ci:e2e)`` —
  and removed from the description. Items in a phase depend on EVERY item of the nearest earlier
  phase that has items; items within one phase are independent.

Everything is validated up front (ids, dependencies, witness syntax) and reported as a
``ChecklistImportError`` naming the file and, for Markdown, the line.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from pathlib import Path

from pydantic import ValidationError

from lha.contracts.state import Checklist, ChecklistItem
from lha.verify.witnesses import validate_witness


class ChecklistImportError(ValueError):
    """The operator's checklist file is unreadable, malformed or structurally invalid."""


@dataclass(frozen=True)
class ImportedChecklist:
    """A parsed checklist file: the items plus the mission metadata it declared."""

    checklist: Checklist
    title: str = ""
    description: str = ""
    references: list[str] = field(default_factory=list)


def load_checklist(path: str | Path, *, include_done: bool = False) -> ImportedChecklist:
    """Load and validate an operator checklist (``.json`` or ``.md``)."""
    p = Path(path)
    try:
        text = p.read_text(encoding="utf-8")
    except OSError as exc:
        raise ChecklistImportError(f"{p}: cannot read checklist: {exc}") from exc
    suffix = p.suffix.lower()
    if suffix == ".json":
        imported, lines = _from_json(p, text), {}
    elif suffix in (".md", ".markdown"):
        imported, lines = _from_markdown(p, text, include_done=include_done)
    else:
        raise ChecklistImportError(
            f"{p}: unsupported checklist format {suffix!r} (use .json or .md)"
        )
    _validate(p, imported.checklist, lines)
    return imported


def _validate(path: Path, checklist: Checklist, lines: dict[str, int]) -> None:
    def where(item_id: str) -> str:
        return f"{path}:{lines[item_id]}" if item_id in lines else str(path)

    if not checklist.items:
        raise ChecklistImportError(f"{path}: checklist has no items")
    errors = checklist.dependency_errors()
    if errors:
        raise ChecklistImportError(f"{path}: invalid checklist: " + "; ".join(errors))
    for item in checklist.items:
        for witness in item.witnesses:
            try:
                validate_witness(witness)
            except ValueError as exc:
                raise ChecklistImportError(f"{where(item.id)}: item {item.id!r}: {exc}") from exc


# --- JSON -----------------------------------------------------------------------------------

_ITEM_FIELDS = frozenset(ChecklistItem.model_fields)


def _str_list(path: Path, value: object, what: str) -> list[str]:
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value):
        raise ChecklistImportError(f"{path}: {what} must be a list of strings")
    return list(value)


def _from_json(path: Path, text: str) -> ImportedChecklist:
    try:
        data = json.loads(text)
    except ValueError as exc:
        raise ChecklistImportError(f"{path}: invalid JSON: {exc}") from exc
    if isinstance(data, list):
        data = {"items": data}
    if not isinstance(data, dict) or not isinstance(data.get("items"), list):
        raise ChecklistImportError(f'{path}: expected an object with an "items" list')
    title = data.get("title", "")
    description = data.get("description", "")
    if not isinstance(title, str) or not isinstance(description, str):
        raise ChecklistImportError(f'{path}: "title" and "description" must be strings')
    references = _str_list(path, data.get("references", []), '"references"')
    raw_items: list[object] = data["items"]
    width = max(2, len(str(len(raw_items))))
    items: list[ChecklistItem] = []
    for n, raw in enumerate(raw_items, start=1):
        label = f"items[{n - 1}]"
        if not isinstance(raw, dict):
            raise ChecklistImportError(f"{path}: {label} must be an object")
        unknown = sorted(str(k) for k in raw if k not in _ITEM_FIELDS)
        if unknown:
            raise ChecklistImportError(f"{path}: {label} has unknown field(s): {unknown}")
        fields = dict(raw)
        fields.setdefault("id", f"{n:0{width}d}")
        try:
            item = ChecklistItem.model_validate(fields)
        except ValidationError as exc:
            raise ChecklistImportError(f"{path}: {label} is invalid: {exc}") from exc
        if not item.id.strip() or not item.description.strip():
            raise ChecklistImportError(f"{path}: {label} needs a non-empty id and description")
        items.append(item)
    return ImportedChecklist(
        checklist=Checklist(items=items),
        title=title.strip(),
        description=description.strip(),
        references=references,
    )


# --- Markdown -------------------------------------------------------------------------------

_TITLE_RE = re.compile(r"^#\s+(.+?)\s*#*\s*$")
_PHASE_RE = re.compile(r"^##\s+")
_HEADING_RE = re.compile(r"^#{1,6}\s")
_CHECKBOX_RE = re.compile(r"^(\s*)[-*+]\s+\[([ xX])\]\s+(.*)$")
_BULLET_RE = re.compile(r"^(\s*)(?:[-*+]|\d+[.)])\s")
_WITNESS_RE = re.compile(r"\(\s*witness(?:es)?\s*:\s*([^)]*)\)", re.IGNORECASE)
_SPACES_RE = re.compile(r"\s+")
_WITNESS_STRIP_RE = re.compile(r"\s*" + _WITNESS_RE.pattern, re.IGNORECASE)


@dataclass
class _Draft:
    line: int
    indent: int
    phase: int
    parts: list[str]
    include: bool


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" \t"))


def _from_markdown(
    path: Path, text: str, *, include_done: bool
) -> tuple[ImportedChecklist, dict[str, int]]:
    title = ""
    intro: list[str] = []
    phase = 0
    seen_phase = False
    drafts: list[_Draft] = []
    current: _Draft | None = None
    skip_indent: int | None = None  # inside an ignored nested (non-checkbox) bullet
    in_fence = False

    for lineno, raw in enumerate(text.splitlines(), start=1):
        line = raw.rstrip()
        stripped = line.strip()
        indent = _indent(line)
        if stripped.startswith(("```", "~~~")):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        if skip_indent is not None:
            if not stripped or indent > skip_indent:
                continue
            skip_indent = None
        if not stripped:
            if not seen_phase and current is None:
                intro.append("")
            continue
        if _PHASE_RE.match(line):
            phase, seen_phase, current = phase + 1, True, None
            continue
        if not title and not seen_phase and (m := _TITLE_RE.match(line)):
            title = m.group(1)
            continue
        if _HEADING_RE.match(line):
            current = None
            continue
        if box := _CHECKBOX_RE.match(line):
            current = _Draft(
                line=lineno,
                indent=len(box.group(1)),
                phase=phase,
                parts=[box.group(3)],
                include=box.group(2) == " " or include_done,
            )
            drafts.append(current)
            continue
        if current is not None and indent > current.indent:
            if _BULLET_RE.match(line):
                skip_indent = indent
            else:
                current.parts.append(stripped)
            continue
        current = None
        if _BULLET_RE.match(line):
            skip_indent = indent
        elif not seen_phase:
            intro.append(stripped)

    checklist, lines = _build_items(path, [d for d in drafts if d.include])
    description = "\n\n".join(" ".join(p) for p in _paragraphs(intro) if p)
    imported = ImportedChecklist(
        checklist=checklist,
        title=title or path.stem,
        description=description,
    )
    return imported, lines


def _paragraphs(lines: list[str]) -> list[list[str]]:
    out: list[list[str]] = [[]]
    for line in lines:
        if line:
            out[-1].append(line)
        elif out[-1]:
            out.append([])
    return out


def _build_items(path: Path, drafts: list[_Draft]) -> tuple[Checklist, dict[str, int]]:
    width = max(2, len(str(len(drafts))))
    items: list[ChecklistItem] = []
    lines: dict[str, int] = {}
    ids_by_phase: dict[int, list[str]] = {}
    previous: list[str] = []  # ids of the nearest earlier phase that has items
    current_phase: int | None = None
    for n, draft in enumerate(drafts, start=1):
        if draft.phase != current_phase:
            if current_phase is not None:
                previous = ids_by_phase[current_phase]
            current_phase = draft.phase
        text = " ".join(draft.parts)
        witnesses: list[str] = []
        for group in _WITNESS_RE.findall(text):
            witnesses += [w.strip() for w in group.split(",") if w.strip()]
        description = _SPACES_RE.sub(" ", _WITNESS_STRIP_RE.sub("", text)).strip()
        if not description:
            raise ChecklistImportError(f"{path}:{draft.line}: checklist item has no description")
        item_id = f"{n:0{width}d}"
        ids_by_phase.setdefault(draft.phase, []).append(item_id)
        lines[item_id] = draft.line
        items.append(
            ChecklistItem(
                id=item_id,
                description=description,
                depends_on=list(previous),
                witnesses=witnesses,
            )
        )
    return Checklist(items=items), lines


# --- fabric-emulator style witness manifests ------------------------------------------------


def witnesses_from_manifest(path: str | Path) -> dict[str, list[str]]:
    """Read a ``witnesses.json`` manifest (``{claim_id: {"witnesses": [...]}}``) into a dict.

    Keys starting with ``_`` (e.g. ``_gated``: witnesses that only pass in CI) are skipped.
    """
    p = Path(path)
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise ChecklistImportError(f"{p}: cannot read witness manifest: {exc}") from exc
    if not isinstance(data, dict):
        raise ChecklistImportError(f"{p}: witness manifest must be a JSON object")
    out: dict[str, list[str]] = {}
    for key, entry in data.items():
        if key.startswith("_"):
            continue
        witnesses = entry.get("witnesses") if isinstance(entry, dict) else None
        out[key] = _str_list(p, witnesses, f"{key!r}.witnesses")
    return out
