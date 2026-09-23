"""Prompt construction for the agent loop.

Provider-agnostic: tools are described in text and the model is asked to reply with a single JSON
action (use a tool) or a done signal. This works uniformly across stub / Ollama / Claude without
depending on any one provider's native tool-calling schema (native ``tool_calls`` are also honored
when a backend returns them). The mission anchor — built from the IMMUTABLE mission spec, not the
progress narrative — is recited every cycle to fight drift.
"""

from __future__ import annotations

from lha.contracts.model import ModelMessage
from lha.contracts.state import ChecklistItem, DecisionRecord, SituationSnapshot
from lha.contracts.tools import ToolSpec

ACTION_INSTRUCTIONS = (
    "You act by replying with EXACTLY ONE JSON object and nothing else.\n"
    'To use a tool: {"tool": "<name>", "arguments": { ... }}\n'
    'When the item is fully done: {"done": true, "summary": "<what you did>"}\n'
    "Take one action per reply. Prefer reading/searching before writing.\n"
    "Signalling done does NOT mark the item done: the harness then runs the deterministic "
    "checks, and only a green result counts. Do not edit files under .lha/ (harness-owned) and "
    "do not modify or delete existing tests or test configuration."
)

CORRECTIVE_INSTRUCTIONS = (
    "Your previous reply was not a valid action ({reason}). Reply again with EXACTLY ONE JSON "
    'object and nothing else: {{"tool": "<name>", "arguments": {{...}}}} or '
    '{{"done": true, "summary": "..."}}. Keep it short.'
)

# Caller-supplied extra context (e.g. research briefs / progress) is clipped to its newest tail.
_EXTRA_CONTEXT_CAP = 8000
_LAST_FAILURE_CAP = 3000
_DECISION_LINE_CAP = 600


def render_tools(specs: list[ToolSpec]) -> str:
    lines: list[str] = []
    for spec in specs:
        props = spec.parameters.get("properties", {}) if isinstance(spec.parameters, dict) else {}
        lines.append(f"- {spec.name}: {spec.description} (args: {props})")
    return "\n".join(lines) if lines else "(no tools available)"


def render_decisions(decisions: list[DecisionRecord]) -> str:
    """The recorded design decisions as a prompt section ('' when there are none)."""
    if not decisions:
        return ""
    lines = [
        "Design decisions already recorded (stay consistent with them; if one must change, "
        "record the new decision with record_decision):"
    ]
    for record in decisions:
        cycle = f"[{record.cycle_id}] " if record.cycle_id else ""
        line = f"- {cycle}{record.decision} — {record.rationale}"
        if record.alternatives_rejected:
            line += f" (rejected: {record.alternatives_rejected})"
        if record.affected:
            line += f" (affects: {', '.join(record.affected)})"
        lines.append(line[:_DECISION_LINE_CAP])
    return "\n".join(lines)


def corrective_message(reason: str) -> ModelMessage:
    """The user turn sent after an unparseable / truncated reply."""
    return ModelMessage(role="user", content=CORRECTIVE_INSTRUCTIONS.format(reason=reason))


def build_messages(
    *,
    anchor_text: str,
    snapshot: SituationSnapshot,
    item: ChecklistItem,
    specs: list[ToolSpec],
    mission_text: str = "",
) -> list[ModelMessage]:
    """Build the initial messages for one cycle: anchor recitation + active item + tools.

    ``mission_text`` is the immutable mission recitation (always first); ``anchor_text`` is
    optional caller context (progress, research, reflections), included when it adds anything.
    """
    anchor = mission_text.strip() or anchor_text.strip()
    extra = anchor_text.strip() if mission_text.strip() else ""
    if extra and extra != anchor:
        if len(extra) > _EXTRA_CONTEXT_CAP:
            extra = "...[earlier context trimmed]...\n" + extra[-_EXTRA_CONTEXT_CAP:]
        anchor = f"{anchor}\n\nContext:\n{extra}"
    system = (
        f"{anchor}\n\n"
        f"You are the Lead Engineer. Make verified progress on ONE checklist item per cycle.\n\n"
        f"Available tools:\n{render_tools(specs)}\n\n"
        f"{ACTION_INSTRUCTIONS}"
    )
    recent = "\n".join(snapshot.recent_commits[:10]) or "(none yet)"
    user = f"Active checklist item: [{item.id}] {item.description}\n\n"
    if item.witnesses:
        listed = "\n".join(f"- {w}" for w in item.witnesses)
        user += (
            "This item is done only when ALL of these acceptance checks (witnesses) pass, in "
            f"addition to the mission's checks:\n{listed}\n\n"
        )
    if item.last_failure:
        user += (
            f"Previous attempt #{item.attempts} FAILED verification:\n"
            f"{item.last_failure[-_LAST_FAILURE_CAP:]}\n\n"
        )
    decisions = render_decisions(snapshot.last_decisions)
    if decisions:
        user += f"{decisions}\n\n"
    user += f"Recent commits:\n{recent}\n\nWork this item using the tools, then signal done."
    return [
        ModelMessage(role="system", content=system),
        ModelMessage(role="user", content=user),
    ]
