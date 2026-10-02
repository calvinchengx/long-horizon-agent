"""Exporting a mission's own judgments as labels (``lha labels export``).

A mission records three judgments that can fit the System One thresholds still to be measured
(``docs/25-system-one.md``, "Not built yet"): a human's approve or reject of a gated tool call,
the verifier's verdict on an attempt, and the reviewer's verdict on a verified diff. This module
turns the anchor's committed events and the store's ``hitl_gates`` rows into JSON Lines, one
object per judgment, redacted like every other record LHA writes. ``label_rows`` is pure and
pinned by ``spec/systemone/labels.json``; the CLI supplies the events, gates and diffs.
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass

from lha.contracts.state import EventRecord
from lha.obs.redact import redact_mapping
from lha.persistence.store import GATE_DEFAULTED, GATE_RESOLVED, GateRow

#: Bumped when a row's shape changes, so a training script can refuse rows it does not know.
LABEL_SCHEMA = 1

SOURCE_GATE = "gate"  # hitl_gates: a human (or the default) answered a gate
SOURCE_TOOL_APPROVAL = "tool_approval"  # the dispatcher's record of a gated tool call
SOURCE_VERIFIER = "verifier"  # a cycle's verification verdict
SOURCE_REVIEW = "review"  # the reviewer's verdict on a verified diff
SOURCES = (SOURCE_GATE, SOURCE_TOOL_APPROVAL, SOURCE_VERIFIER, SOURCE_REVIEW)

#: Anchor event kinds the rows come from.
RUN_EVENT = "orchestrate"
CYCLE_EVENT = "cycle"
TOOL_APPROVAL_EVENT = "tool_approval"
REVIEW_EVENT = "review"
REVIEW_SCREEN_EVENT = "review_screen"

#: A reviewed diff in a row is cut to this many characters (``--diffs``).
DIFF_CAP = 20_000
#: What a check contributes to a verifier row (no timings: they vary run to run).
_CHECK_FIELDS = ("name", "passed", "gating", "exit_code")
_VERIFIER_FIELDS = ("status", "verified", "tool_calls", "rolled_back", "split_into")
_APPROVAL_FIELDS = ("tool", "arguments", "reason", "fingerprint")
_REVIEW_FIELDS = ("base", "head", "blocking", "blocking_issues", "advisory")


@dataclass(frozen=True)
class LabelRow:
    """One judgment: what was judged (``input``) and the judgment (``label``), by whom."""

    source: str
    label: str
    by: str
    mission_id: str
    cycle_id: str
    item_id: str
    at: str
    input: dict[str, object]

    def to_json(self) -> dict[str, object]:
        return {
            "schema": LABEL_SCHEMA,
            "source": self.source,
            "label": self.label,
            "by": self.by,
            "mission_id": self.mission_id,
            "cycle_id": self.cycle_id,
            "item_id": self.item_id,
            "at": self.at,
            "input": self.input,
        }


def to_jsonl(rows: Iterable[LabelRow]) -> str:
    """The rows as JSON Lines (sorted keys, so both implementations write the same bytes)."""
    return "".join(
        json.dumps(row.to_json(), sort_keys=True, ensure_ascii=False, separators=(",", ":")) + "\n"
        for row in rows
    )


def mission_id_of(events: Sequence[EventRecord]) -> str:
    """The mission id the anchor's latest ``orchestrate`` event names (``""`` when none does)."""
    for event in reversed(events):
        if event.kind == RUN_EVENT:
            return str(event.payload.get("mission_id") or "")
    return ""


def label_rows(
    events: Sequence[EventRecord],
    gates: Sequence[GateRow],
    *,
    mission_id: str = "",
    diffs: Callable[[str, str], str] | None = None,
) -> list[LabelRow]:
    """Label rows from an anchor's committed ``events`` and the store's ``gates``.

    Event rows come first, in log order; then the closed gates (``RESOLVED`` or ``DEFAULTED``),
    oldest opening first. A ``tool_approval`` event whose fingerprint a gate row also carries is
    the same decision recorded twice (the approver writes the gate, the dispatcher the event), so
    only the gate row is kept. ``mission_id`` defaults to the one the latest ``orchestrate`` event names.
    ``diffs(base, head)`` supplies a reviewed diff (cut to ``DIFF_CAP``); without it review rows
    carry only the commit range.
    """
    mission = mission_id or mission_id_of(events)
    closed = sorted(
        (g for g in gates if g.status in (GATE_RESOLVED, GATE_DEFAULTED)),
        key=lambda g: (g.opened_at, g.gate_id),
    )
    gated = {(g.request or {}).get("fingerprint", "") for g in closed} - {""}
    # The pre-review screen's findings for each reviewed (cycle, item): the review row carries
    # them, so a threshold can be fitted against the reviewer's verdict.
    screens = {
        (e.cycle_id, str(e.payload.get("item_id") or "")): _as_list(e.payload.get("findings"))
        for e in events
        if e.kind == REVIEW_SCREEN_EVENT
    }
    rows: list[LabelRow] = []
    for event in events:
        row = _event_row(event, mission, gated, diffs, screens)
        if row is not None:
            rows.append(row)
    for gate in closed:
        rows.append(_gate_row(gate))
    return rows


def _event_row(
    event: EventRecord,
    mission: str,
    gated: set[str],
    diffs: Callable[[str, str], str] | None,
    screens: dict[tuple[str, str], list[object]],
) -> LabelRow | None:
    payload = event.payload
    if event.kind == TOOL_APPROVAL_EVENT:
        if str(payload.get("fingerprint") or "") in gated:
            return None
        by = "default" if payload.get("defaulted") else str(payload.get("resolved_by") or "human")
        return _row(
            SOURCE_TOOL_APPROVAL,
            str(payload.get("decision") or ""),
            by,
            mission,
            event.cycle_id,
            "",
            _pick(payload, _APPROVAL_FIELDS),
        )
    if event.kind == CYCLE_EVENT and payload.get("verdict") is not None:
        data = _pick(payload, _VERIFIER_FIELDS)
        checks = payload.get("checks")
        if isinstance(checks, list):
            data["checks"] = [_pick(c, _CHECK_FIELDS) for c in checks if isinstance(c, dict)]
        return _row(
            SOURCE_VERIFIER,
            str(payload["verdict"]),
            "verifier",
            mission,
            event.cycle_id,
            str(payload.get("item_id") or ""),
            data,
        )
    if event.kind == REVIEW_EVENT and payload.get("verdict") is not None:
        data = _pick(payload, _REVIEW_FIELDS)
        key = (event.cycle_id, str(payload.get("item_id") or ""))
        if key in screens:
            data["screen_findings"] = screens[key]
        base, head = str(payload.get("base") or ""), str(payload.get("head") or "")
        if diffs is not None and base and head:
            data["diff"] = diffs(base, head)[:DIFF_CAP]
        return _row(
            SOURCE_REVIEW,
            str(payload["verdict"]),
            "reviewer",
            mission,
            event.cycle_id,
            str(payload.get("item_id") or ""),
            data,
        )
    return None


def _gate_row(gate: GateRow) -> LabelRow:
    by = "default" if gate.status == GATE_DEFAULTED else (gate.resolved_by or "human")
    data: dict[str, object] = {
        "kind": gate.kind,
        "question": gate.question,
        "options": list(gate.options),
        "risk": gate.risk,
        "request": dict(gate.request or {}),
    }
    return _row(
        SOURCE_GATE, gate.decision or "", by, gate.mission_id, "", "", data, gate.resolved_at
    )


def _row(
    source: str,
    label: str,
    by: str,
    mission: str,
    cycle_id: str,
    item_id: str,
    data: dict[str, object],
    at: str = "",
) -> LabelRow:
    return LabelRow(
        source=source,
        label=label,
        by=by,
        mission_id=mission,
        cycle_id=cycle_id,
        item_id=item_id,
        at=at,
        input=redact_mapping(data),
    )


def _pick(payload: dict[str, object], keys: Sequence[str]) -> dict[str, object]:
    return {key: payload[key] for key in keys if key in payload}


def _as_list(value: object) -> list[object]:
    return list(value) if isinstance(value, list) else []
