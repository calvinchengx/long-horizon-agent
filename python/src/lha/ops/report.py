"""``lha mission-report``: one page about a mission, from its anchor and the mission store.

Everything a mission leaves behind is spread over the anchor's committed files, the store's
rows and the git history. ``render_report`` joins what an operator asks for after the fact into
one deterministic text (the same bytes from both implementations, ``spec/state/report.json``):
the mission, every item with its status, the cycles' verdicts, reviews and screens, the human
gates, the spend, and the commits. It reads; it changes nothing.
"""

from __future__ import annotations

from collections import Counter
from collections.abc import Sequence
from dataclasses import dataclass

from lha.contracts.state import Checklist, EventRecord, MissionSpec
from lha.persistence.store import CostSummary, GateRow, MissionRow


def _usd(value: float | None) -> str:
    return "unknown" if value is None else f"${value:.4f}"


def format_gate_row(row: GateRow) -> list[str]:
    """Human-readable lines for one recorded gate (``lha gates``, the report)."""
    decided = (
        f"{row.decision} by {row.resolved_by or '-'} at {row.resolved_at[:19]}"
        if row.status in ("RESOLVED", "DEFAULTED")
        else f"open, default {row.default_action or '-'} at {row.deadline[:19] or '-'}"
    )
    lines = [
        f"{row.opened_at[:19]}  {row.mission_id}  {row.gate_id}  {row.kind:<9} "
        f"{row.status:<9} reminders {row.reminders}  {decided}",
        f"  question: {row.question}",
        f"  options: {' | '.join(row.options)}",
    ]
    if row.request:
        what = row.request.get("argv") or row.request.get("arguments") or ""
        lines.append(f"  request: {row.request.get('tool', '')} {what}".rstrip())
    return lines


def format_cost_total(summary: CostSummary) -> str:
    """The ``lha costs`` total line."""
    return (
        f"total: {summary.calls} calls  known {_usd(summary.known_usd)}  "
        f"unknown-cost calls {summary.unknown_cost_calls}  "
        f"tokens in {summary.input_tokens} out {summary.output_tokens}"
    )


@dataclass(frozen=True)
class ReportInput:
    """What the report is rendered from. ``None`` store parts render as absent."""

    mission_id: str  # "" when neither the anchor nor the caller names one
    spec: MissionSpec | None
    checklist: Checklist
    events: Sequence[EventRecord]
    row: MissionRow | None
    gates: Sequence[GateRow]
    cost: CostSummary | None
    commits: int
    head_sha: str


def _first_line(text: str, limit: int = 100) -> str:
    line = text.strip().splitlines()[0] if text.strip() else ""
    return line if len(line) <= limit else line[: limit - 3] + "..."


def render_report(inp: ReportInput) -> str:
    """The mission report (see the module docstring); ends with a newline."""
    out: list[str] = []
    title = inp.spec.title if inp.spec is not None else (inp.row.title if inp.row else "(unknown)")
    out.append(f"# Mission: {title}")
    description = (
        inp.spec.description if inp.spec is not None else (inp.row.description if inp.row else "")
    )
    if description.strip():
        out.append(description.strip())
    if inp.spec is not None and inp.spec.acceptance.strip():
        out.append(f"Definition of done: {inp.spec.acceptance.strip()}")
    status = inp.row.status if inp.row is not None else "-"
    out.append(
        f"mission {inp.mission_id or '-'}  status {status}  head {inp.head_sha[:12] or '-'}  "
        f"commits {inp.commits}"
    )

    items = inp.checklist.items
    done = inp.checklist.items_done
    state = ""
    if items and inp.checklist.is_complete:
        state = ", complete"
    elif items and inp.checklist.is_deadlocked:
        state = f", deadlocked: {inp.checklist.deadlock_reason()}"
    out += ["", f"## Items ({done}/{len(items)} done{state})"]
    if not items:
        out.append("(no items)")
    for item in items:
        out.append(f"{item.id}  {item.status:<11} attempts {item.attempts:>2}  {item.description}")
        if item.witnesses:
            out.append(f"      witnesses: {', '.join(item.witnesses)}")
        if item.last_failure.strip() and item.status != "done":
            out.append(f"      last failure: {_first_line(item.last_failure)}")

    cycles = [e for e in inp.events if e.kind == "cycle" and e.payload.get("verdict") is not None]
    verdicts = Counter(str(e.payload.get("verdict")) for e in cycles)
    out += ["", f"## Cycles ({len(cycles)})"]
    other = len(cycles) - verdicts["passed"] - verdicts["failed"]
    out.append(f"verdicts: passed {verdicts['passed']}, failed {verdicts['failed']}, other {other}")
    reviews = Counter(str(e.payload.get("verdict")) for e in inp.events if e.kind == "review")
    if reviews:
        other_reviews = sum(reviews.values()) - reviews["approve"] - reviews["block"]
        out.append(
            f"reviews: approve {reviews['approve']}, block {reviews['block']}, "
            f"unparsed {other_reviews}"
        )
    screens = [e for e in inp.events if e.kind == "review_screen"]
    if screens:
        flagged = sum(1 for e in screens if e.payload.get("findings"))
        out.append(f"screens: {flagged} of {len(screens)} diffs flagged")
    reflections = sum(1 for e in inp.events if e.kind == "reflection")
    if reflections:
        out.append(f"reflections: {reflections}")

    out += ["", f"## Gates ({len(inp.gates)})"]
    if not inp.mission_id:
        out.append("(mission id unknown: pass MISSION_ID to read the store)")
    elif not inp.gates:
        out.append("none recorded")
    for gate in inp.gates:
        out.extend(format_gate_row(gate))

    out += ["", "## Cost"]
    if not inp.mission_id:
        out.append("(mission id unknown: pass MISSION_ID to read the store)")
    elif inp.cost is None or not inp.cost.calls:
        out.append("no cost ledger rows")
    else:
        out.append(format_cost_total(inp.cost))
    return "\n".join(out) + "\n"
