"""Operator edits to a mission's checklist (``lha mission-edit``, the ``checklist_edit_v1``
signal): add, remove, edit, reopen, block and unblock items, applied as one atomic batch.

An edit batch is a list of objects, each with an ``op``::

    {"op": "add", "description": "...", "id"?: "..", "witnesses"?: [..], "depends_on"?: [..],
     "after"?: "<id>", "allow_harness_edits"?: bool, "notes"?: "..."}
    {"op": "remove", "id": ".."}            # an open item (never a done or split one)
    {"op": "edit", "id": "..", "description"?: .., "witnesses"?: .., "depends_on"?: ..,
     "notes"?: .., "allow_harness_edits"?: ..}   # an open item
    {"op": "reopen", "id": ".."}            # done -> todo (its verification is forgotten)
    {"op": "block", "id": ".."}             # an open item -> blocked (a human parked it)
    {"op": "unblock", "id": ".."}           # blocked -> todo

A description may carry witnesses the Markdown roadmap way: ``"Do X (witness: cmd:true)"``.
The batch is validated as a whole (ids, dependencies, witness syntax, at least one item left);
one bad edit refuses the whole batch and the checklist is unchanged. Statuses other than the
ones named are never touched: the running cycle's ``in_progress`` item stays in progress.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping, Sequence

from lha.contracts.state import Checklist, ChecklistItem, EventRecord
from lha.state.checklist_import import split_witnesses
from lha.verify.witnesses import validate_witness

#: Edits per batch (one signal, one ``lha mission-edit`` call).
MAX_EDIT_OPS = 50

_OPS = ("add", "remove", "edit", "reopen", "block", "unblock")
_FIELDS: dict[str, frozenset[str]] = {
    "add": frozenset(
        {
            "op",
            "id",
            "description",
            "witnesses",
            "depends_on",
            "after",
            "allow_harness_edits",
            "notes",
        }
    ),
    "remove": frozenset({"op", "id"}),
    "edit": frozenset(
        {"op", "id", "description", "witnesses", "depends_on", "allow_harness_edits", "notes"}
    ),
    "reopen": frozenset({"op", "id"}),
    "block": frozenset({"op", "id"}),
    "unblock": frozenset({"op", "id"}),
}
_EDIT_ORDER = ("description", "witnesses", "depends_on", "notes", "allow_harness_edits")


class ChecklistEditError(ValueError):
    """The batch is malformed or would leave the checklist invalid; nothing was changed."""


def apply_edits(
    checklist: Checklist,
    edits: Sequence[object],
    *,
    by: str = "",
    reserved: Iterable[str] = (),
) -> list[str]:
    """Apply ``edits`` to ``checklist`` in place; return one summary line per edit.

    Raises ``ChecklistEditError`` (and leaves ``checklist`` untouched) when any edit is invalid.
    ``reserved`` ids (every item a cycle ever worked on, from the anchor's events) and the ids
    the checklist starts with are never handed to an added item (see ``next_item_id``).
    """
    if not isinstance(edits, Sequence) or isinstance(edits, str | bytes) or not edits:
        raise ChecklistEditError("edits must be a non-empty list of objects")
    if len(edits) > MAX_EDIT_OPS:
        raise ChecklistEditError(f"too many edits ({len(edits)} > {MAX_EDIT_OPS})")
    draft = checklist.model_copy(deep=True)
    taken = frozenset({*reserved, *(item.id for item in checklist.items)})
    lines: list[str] = []
    for n, edit in enumerate(edits, start=1):
        lines.append(_apply_one(draft, n, edit, by, taken))
    if not draft.items:
        raise ChecklistEditError("the checklist would have no items")
    errors = draft.dependency_errors()
    if errors:
        raise ChecklistEditError("invalid checklist: " + "; ".join(errors))
    for item in draft.items:
        for witness in item.witnesses:
            try:
                validate_witness(witness)
            except ValueError as exc:
                raise ChecklistEditError(f"item {item.id!r}: {exc}") from exc
    checklist.items = draft.items
    return lines


def _apply_one(checklist: Checklist, n: int, edit: object, by: str, taken: frozenset[str]) -> str:
    if not isinstance(edit, Mapping):
        raise ChecklistEditError(f"edit #{n}: must be an object")
    op = edit.get("op")
    if op is None:
        raise ChecklistEditError(f"edit #{n}: missing 'op'")
    if not isinstance(op, str) or op not in _OPS:
        raise ChecklistEditError(f"edit #{n}: unknown op {op!r}")
    unknown = sorted(str(k) for k in edit if k not in _FIELDS[op])
    if unknown:
        raise ChecklistEditError(f"edit #{n}: unknown field(s) {unknown}")
    if op == "add":
        return _add(checklist, n, edit, taken)
    item_id = _str(n, edit, "id")
    if item_id is None or not item_id.strip():
        raise ChecklistEditError(f"edit #{n}: 'id' is required")
    item = checklist.get(item_id)
    if item is None:
        raise ChecklistEditError(f"edit #{n}: unknown item {item_id!r}")
    if item.status == "split":
        raise ChecklistEditError(f"edit #{n}: item {item_id!r} was split")
    if op == "remove":
        if item.status == "done":
            raise ChecklistEditError(f"edit #{n}: cannot remove done item {item_id!r}")
        checklist.items = [i for i in checklist.items if i.id != item_id]
        return f"removed {item_id}"
    if op == "edit":
        return _edit(n, edit, item)
    if op == "reopen":
        if item.status != "done":
            raise ChecklistEditError(f"edit #{n}: item {item_id!r} is not done")
        item.status = "todo"
        item.verified_by = []
        item.consecutive_failures = 0
        item.last_failure = ""
        return f"reopened {item_id}"
    if op == "block":
        if item.status == "done":
            raise ChecklistEditError(f"edit #{n}: item {item_id!r} is done")
        if item.status == "blocked":
            raise ChecklistEditError(f"edit #{n}: item {item_id!r} is already blocked")
        item.status = "blocked"
        item.last_failure = f"blocked by {by}" if by else "blocked by an operator"
        return f"blocked {item_id}"
    if item.status != "blocked":
        raise ChecklistEditError(f"edit #{n}: item {item_id!r} is not blocked")
    checklist.unblock(item_id)
    return f"unblocked {item_id}"


def _add(checklist: Checklist, n: int, edit: Mapping[str, object], taken: frozenset[str]) -> str:
    text = _str(n, edit, "description")
    description, witnesses = split_witnesses(text or "")
    if not description:
        raise ChecklistEditError(f"edit #{n}: description must not be empty")
    witnesses += _str_list(n, edit, "witnesses") or []
    item_id = _str(n, edit, "id")
    if item_id is not None:
        item_id = item_id.strip()
        if not item_id:
            raise ChecklistEditError(f"edit #{n}: 'id' must not be empty")
        if checklist.get(item_id) is not None:
            raise ChecklistEditError(f"edit #{n}: item {item_id!r} already exists")
    else:
        item_id = next_item_id(checklist, taken)
    item = ChecklistItem(
        id=item_id,
        description=description,
        witnesses=witnesses,
        depends_on=_str_list(n, edit, "depends_on") or [],
        allow_harness_edits=_bool(n, edit, "allow_harness_edits") or False,
        notes=_str(n, edit, "notes") or "",
    )
    after = _str(n, edit, "after")
    if after:
        anchor = checklist.get(after)
        if anchor is None:
            raise ChecklistEditError(f"edit #{n}: unknown item {after!r} in 'after'")
        at = checklist.items.index(anchor) + 1
        checklist.items = [*checklist.items[:at], item, *checklist.items[at:]]
    else:
        checklist.items = [*checklist.items, item]
    return f"added {item_id}"


def _edit(n: int, edit: Mapping[str, object], item: ChecklistItem) -> str:
    if item.status == "done":
        raise ChecklistEditError(f"edit #{n}: item {item.id!r} is done; reopen it first")
    changed = [f for f in _EDIT_ORDER if f in edit]
    if not changed:
        raise ChecklistEditError(f"edit #{n}: nothing to change on {item.id!r}")
    if "description" in edit:
        description, extra = split_witnesses(_str(n, edit, "description") or "")
        if not description:
            raise ChecklistEditError(f"edit #{n}: description must not be empty")
        item.description = description
        if extra:
            item.witnesses = [*item.witnesses, *extra]
    if "witnesses" in edit:
        item.witnesses = _str_list(n, edit, "witnesses") or []
    if "depends_on" in edit:
        item.depends_on = _str_list(n, edit, "depends_on") or []
    if "notes" in edit:
        item.notes = _str(n, edit, "notes") or ""
    if "allow_harness_edits" in edit:
        item.allow_harness_edits = _bool(n, edit, "allow_harness_edits") or False
    return f"edited {item.id}: {', '.join(changed)}"


def worked_item_ids(events: Iterable[EventRecord]) -> set[str]:
    """Every item id the anchor's events name (``item_id``): reserved for ``next_item_id``."""
    ids: set[str] = set()
    for event in events:
        item_id = event.payload.get("item_id")
        if isinstance(item_id, str) and item_id:
            ids.add(item_id)
    return ids


def next_item_id(checklist: Checklist, reserved: Iterable[str] = ()) -> str:
    """One past the largest numeric id among the items and ``reserved``, zero-padded to the
    import's width: ``04`` after ``01``..``03``, and still ``04`` when ``03`` was removed (its id
    stays reserved), so the anchor's history never names two items the same. Ids that are not
    plain digits (``02.1``, ``02a``) do not count."""
    reserved = set(reserved)
    numeric = [i for i in (*(item.id for item in checklist.items), *reserved) if i.isdigit()]
    n = max((int(i) for i in numeric), default=0) + 1
    width = max(2, *(len(i) for i in numeric), len(str(n)))
    while checklist.get(f"{n:0{width}d}") is not None or f"{n:0{width}d}" in reserved:
        n += 1
    return f"{n:0{width}d}"


def _str(n: int, edit: Mapping[str, object], key: str) -> str | None:
    value = edit.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise ChecklistEditError(f"edit #{n}: {key!r} must be a string")
    return value


def _str_list(n: int, edit: Mapping[str, object], key: str) -> list[str] | None:
    value = edit.get(key)
    if value is None:
        return None
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value):
        raise ChecklistEditError(f"edit #{n}: {key!r} must be a list of strings")
    return [v.strip() for v in value if v.strip()]


def _bool(n: int, edit: Mapping[str, object], key: str) -> bool | None:
    value = edit.get(key)
    if value is None:
        return None
    if not isinstance(value, bool):
        raise ChecklistEditError(f"edit #{n}: {key!r} must be a boolean")
    return value
