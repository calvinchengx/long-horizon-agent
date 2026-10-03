"""Gold evaluation sets (``eval/gold/*.jsonl``, ``lha eval``): label rows with the judgment a
correct judge would have given, and the scoring of a judge against them.

A gold row is a ``lha labels export`` row (``lha.systemone.labels``, schema 1) plus::

    "gold": {"label": "block", "by": "<who decided and from what evidence>", "note": "..."},
    "tags": ["conftest-injection", "bypass"]

``gold.label`` is the right answer for what was judged (the row's ``input``), not for the item:
a reviewer shown an empty diff should block, whatever the code looked like. ``check_gold``
refuses a set whose gold labels are outside a source's vocabulary or that records one judgment
twice. ``score`` runs a judge over the rows and counts agreement with the gold labels, with
precision and recall of each source's positive label (the one that refuses: ``reject``,
``failed``, ``block``). Two judges are built in and need no model: ``recorded`` (the label the
mission recorded, so the scorecard says how often LHA's own verifier, reviewer and gates were
right) and ``screen`` (the deterministic pre-review screen re-run on each review row's diff).
Both implementations parse, check, score and render identically (``spec/systemone/gold.json``).
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field

from lha.systemone.labels import (
    LABEL_SCHEMA,
    SOURCE_GATE,
    SOURCE_REVIEW,
    SOURCE_TOOL_APPROVAL,
    SOURCE_VERIFIER,
    SOURCES,
)
from lha.verify.review_screen import screen_diff

#: The labels a gold row may carry per source, and the one that refuses (the positive class).
VOCABULARY: dict[str, tuple[str, ...]] = {
    SOURCE_GATE: ("approve", "reject"),
    SOURCE_TOOL_APPROVAL: ("approve", "reject"),
    SOURCE_VERIFIER: ("passed", "failed"),
    SOURCE_REVIEW: ("approve", "block"),
}
POSITIVE: dict[str, str] = {
    SOURCE_GATE: "reject",
    SOURCE_TOOL_APPROVAL: "reject",
    SOURCE_VERIFIER: "failed",
    SOURCE_REVIEW: "block",
}
JUDGES = ("recorded", "screen")
_STRING_KEYS = ("label", "by", "mission_id", "cycle_id", "item_id", "at")


class GoldError(ValueError):
    """A gold file that cannot be read as gold rows (``<file>:<line>: <why>``)."""


@dataclass(frozen=True)
class GoldRow:
    """One judged input with the label it got and the label it should have got."""

    source: str
    label: str
    by: str
    mission_id: str
    cycle_id: str
    item_id: str
    at: str
    input: dict[str, object]
    gold: str
    gold_by: str
    note: str = ""
    tags: tuple[str, ...] = ()
    where: str = ""  # "<file>:<line>", for messages

    @property
    def place(self) -> str:
        """``mission cycle item`` (the non-empty parts) for a report line."""
        return " ".join(p for p in (self.mission_id, self.cycle_id, self.item_id) if p) or "?"

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
            "gold": {"label": self.gold, "by": self.gold_by, "note": self.note},
            "tags": list(self.tags),
        }


def to_jsonl(rows: Iterable[GoldRow]) -> str:
    """The rows as JSON Lines (sorted keys, so both implementations write the same bytes)."""
    return "".join(
        json.dumps(row.to_json(), sort_keys=True, ensure_ascii=False, separators=(",", ":")) + "\n"
        for row in rows
    )


def parse_gold(text: str, *, name: str = "gold") -> list[GoldRow]:
    """The gold rows in a JSON Lines ``text``; ``GoldError`` names the first bad line."""
    rows: list[GoldRow] = []
    for n, line in enumerate(text.splitlines(), start=1):
        if not line.strip():
            continue
        where = f"{name}:{n}"
        try:
            data = json.loads(line)
        except ValueError:
            data = None
        if not isinstance(data, dict):
            raise GoldError(f"{where}: line is not a JSON object")
        if data.get("schema") != LABEL_SCHEMA:
            raise GoldError(f"{where}: schema {data.get('schema')!r} is not {LABEL_SCHEMA}")
        source = data.get("source")
        if source not in SOURCES:
            raise GoldError(f"{where}: unknown source {source!r}")
        for key in _STRING_KEYS:
            if not isinstance(data.get(key, ""), str):
                raise GoldError(f"{where}: {key!r} must be a string")
        inp = data.get("input", {})
        if not isinstance(inp, dict):
            raise GoldError(f"{where}: 'input' must be an object")
        gold = data.get("gold")
        if (
            not isinstance(gold, dict)
            or not isinstance(gold.get("label"), str)
            or not gold["label"]
            or not isinstance(gold.get("by"), str)
            or not gold["by"]
            or not isinstance(gold.get("note", ""), str)
        ):
            raise GoldError(f"{where}: 'gold' must be an object with a non-empty 'label' and 'by'")
        tags = data.get("tags", [])
        if not isinstance(tags, list) or not all(isinstance(t, str) for t in tags):
            raise GoldError(f"{where}: 'tags' must be a list of strings")
        rows.append(
            GoldRow(
                source=str(source),
                label=str(data.get("label", "")),
                by=str(data.get("by", "")),
                mission_id=str(data.get("mission_id", "")),
                cycle_id=str(data.get("cycle_id", "")),
                item_id=str(data.get("item_id", "")),
                at=str(data.get("at", "")),
                input=dict(inp),
                gold=str(gold["label"]),
                gold_by=str(gold["by"]),
                note=str(gold.get("note", "")),
                tags=tuple(tags),
                where=where,
            )
        )
    return rows


def judgment_key(row: GoldRow) -> str:
    """What makes two rows the same judgment: source, mission, cycle, item, and for a gate or
    approval the request's fingerprint."""
    parts = [row.source, row.mission_id, row.cycle_id, row.item_id]
    if row.source in (SOURCE_GATE, SOURCE_TOOL_APPROVAL):
        request = row.input.get("request")
        fingerprint = request.get("fingerprint") if isinstance(request, dict) else None
        parts.append(str(fingerprint or row.input.get("fingerprint") or ""))
    return " ".join(parts)


def check_gold(rows: Sequence[GoldRow]) -> list[str]:
    """Why the set is not usable: gold labels outside the source's vocabulary, and the same
    judgment recorded twice (``[]`` when it is fine)."""
    errors: list[str] = []
    seen: dict[str, str] = {}
    for row in rows:
        vocab = VOCABULARY[row.source]
        if row.gold not in vocab:
            errors.append(
                f"{row.where}: gold label {row.gold!r} is not one of {', '.join(vocab)} "
                f"for source {row.source}"
            )
        key = judgment_key(row)
        if key in seen:
            errors.append(f"{row.where}: duplicate of {seen[key]} ({key})")
        else:
            seen[key] = row.where
    return errors


Judge = Callable[[GoldRow], str | None]


def judge_recorded(row: GoldRow) -> str | None:
    """The label the mission recorded (how right LHA's own judges were)."""
    return row.label


def judge_screen(row: GoldRow) -> str | None:
    """The pre-review screen re-run on a review row's diff; abstains elsewhere."""
    if row.source != SOURCE_REVIEW:
        return None
    diff = row.input.get("diff")
    if not isinstance(diff, str):
        return None
    return "block" if screen_diff(diff) else "approve"


def judge_named(name: str) -> Judge:
    if name == "recorded":
        return judge_recorded
    if name == "screen":
        return judge_screen
    raise ValueError(f"unknown judge {name!r}; expected one of {', '.join(JUDGES)}")


@dataclass
class Scorecard:
    """One source's agreement with the gold labels under a judge."""

    source: str
    rows: int = 0
    judged: int = 0
    agree: int = 0
    tp: int = 0
    fp: int = 0
    fn: int = 0
    tn: int = 0
    disagreements: list[str] = field(default_factory=list)


def score(rows: Sequence[GoldRow], judge: Judge) -> list[Scorecard]:
    """A scorecard per source present, in ``SOURCES`` order."""
    cards = {source: Scorecard(source) for source in SOURCES}
    for row in rows:
        card = cards[row.source]
        card.rows += 1
        judged = judge(row)
        if judged is None:
            continue
        card.judged += 1
        positive = POSITIVE[row.source]
        if judged == row.gold:
            card.agree += 1
        else:
            line = f"  {row.place}: judged {judged}, gold {row.gold}"
            if row.tags:
                line += f" [{','.join(row.tags)}]"
            if row.note:
                line += f" ({row.note})"
            card.disagreements.append(line)
        if judged == positive and row.gold == positive:
            card.tp += 1
        elif judged == positive:
            card.fp += 1
        elif row.gold == positive:
            card.fn += 1
        else:
            card.tn += 1
    return [cards[s] for s in SOURCES if cards[s].rows]


def _ratio(num: int, den: int) -> str:
    return f"{num / den:.2f}" if den else "n/a"


def render_scorecards(cards: Sequence[Scorecard], judge_name: str) -> str:
    """The report ``lha eval run`` prints."""
    lines = [f"judge: {judge_name}"]
    rows = judged = agree = 0
    for card in cards:
        rows, judged, agree = rows + card.rows, judged + card.judged, agree + card.agree
        if not card.judged:
            lines.append(f"{card.source}: {card.rows} rows, 0 judged (the judge abstains)")
            continue
        lines.append(
            f"{card.source}: {card.rows} rows, {card.judged} judged, {card.agree} agree "
            f"({_ratio(card.agree, card.judged)}); {POSITIVE[card.source]}: precision "
            f"{_ratio(card.tp, card.tp + card.fp)}, recall {_ratio(card.tp, card.tp + card.fn)} "
            f"(tp {card.tp}, fp {card.fp}, fn {card.fn}, tn {card.tn})"
        )
        lines.extend(card.disagreements)
    lines.append(f"total: {rows} rows, {judged} judged, {agree} agree ({_ratio(agree, judged)})")
    return "\n".join(lines) + "\n"


def count_by_source(rows: Sequence[GoldRow]) -> str:
    """``"<n> gold rows (<n> gate, <n> tool_approval, <n> verifier, <n> review)"``."""
    counts = ", ".join(f"{sum(1 for r in rows if r.source == s)} {s}" for s in SOURCES)
    return f"{len(rows)} gold rows ({counts})"
