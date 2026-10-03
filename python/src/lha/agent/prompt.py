"""Prompt construction for the agent loop.

Provider-agnostic: tools are described in text and the model is asked to reply with a single JSON
action (use a tool) or a done signal. This works uniformly across stub / Ollama / Claude without
depending on any one provider's native tool-calling schema (native ``tool_calls`` are also honored
when a backend returns them). The mission anchor — built from the IMMUTABLE mission spec, not the
progress narrative — is recited every cycle to fight drift.

Retrieved memory (``lha.memory.service``: past attempts, verified skills, repo/semantic hits) is
rendered by ``render_memory_block`` under a hard character budget and placed in the task message,
so in-session compaction (which keeps the task message verbatim) never drops it. No memory → the
prompt is byte-for-byte what it was without the memory plane.
"""

from __future__ import annotations

from lha.contracts.model import ModelMessage
from lha.contracts.state import ChecklistItem, DecisionRecord, SituationSnapshot
from lha.contracts.tools import ToolSpec
from lha.verify.witnesses import witness_command

ACTION_INSTRUCTIONS = (
    "You act by replying with EXACTLY ONE JSON object and nothing else.\n"
    'To use a tool: {"tool": "<name>", "arguments": { ... }}\n'
    'When the item is fully done: {"done": true, "summary": "<what you did>"}\n'
    "Take one action per reply. Prefer reading/searching before writing.\n"
    "Signalling done does NOT mark the item done: the harness then runs the deterministic "
    "checks, and only a green result counts. Do not edit files under .lha/ (harness-owned) and "
    "do not modify or delete existing tests or test configuration."
)

# The lead only: the loop verifies when it signals done and feeds a failure back while turns
# remain. Without this, leads kept running tests (often unrelated suites) until out of turns.
LEAD_DONE_INSTRUCTIONS = (
    "As soon as this item's acceptance checks pass, signal done instead of running more tests: "
    "if the harness's checks then fail, you get the failure report while turns remain."
)

# The claude_code lead engine: Claude Code calls real tools (LHA's over MCP), so no JSON protocol.
ENGINE_INSTRUCTIONS = (
    "Work this item in this session with the tools you have. When you believe it is done, call "
    "the `verify` tool: it runs the mission's deterministic checks and this item's witnesses "
    "exactly as the harness will after you stop. If it fails, fix the cause and verify again. "
    "Stop when verify passes, or when you cannot make further progress, and end with a short "
    "summary of what you changed.\n"
    "Only a green result from the harness counts. Do not commit, push, or switch branches: the "
    "harness commits verified work itself. Do not edit files under .lha/ (harness-owned) and do "
    "not modify or delete existing tests or test configuration."
)

CORRECTIVE_INSTRUCTIONS = (
    "Your previous reply was not a valid action ({reason}). Reply again with EXACTLY ONE JSON "
    'object and nothing else: {{"tool": "<name>", "arguments": {{...}}}} or '
    '{{"done": true, "summary": "..."}}. Keep it short.'
)

# Caller-supplied extra context (e.g. research briefs / progress) is clipped to its newest tail.
_EXTRA_CONTEXT_CAP = 8000
_LAST_FAILURE_CAP = 3000
# Absolute ceiling on the memory block, whatever budget the caller asks for.
MEMORY_HARD_CAP = 20_000
#: The cycle-start code map (``lha.agent.code_map``): its header and its size cap in the prompt.
CODE_MAP_HEADER = (
    "Code map for this item (from ripwire: ranked definitions, callers and tests; it can miss "
    "things, so read a file before you change it):"
)
CODE_MAP_HARD_CAP = 16_000
MEMORY_HEADER = (
    "Relevant memory (retrieved from earlier cycles, skills and the repository; it may be "
    "stale, so verify before relying on it):"
)
_MIN_LINE = 40  # don't bother adding a line clipped shorter than this
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


def _clip_line(line: str, room: int) -> str:
    if len(line) <= room:
        return line
    return line[: max(0, room - 15)] + " ...[clipped]"


def render_code_map(output: str) -> str:
    """The code-map section for a ripwire task bundle (``""`` when there is none)."""
    body = output.strip()
    if not body:
        return ""
    return _clip_line(f"{CODE_MAP_HEADER}\n{body}", CODE_MAP_HARD_CAP)


def render_memory_block(
    sections: list[tuple[str, list[str]]],
    *,
    budget_chars: int,
    weights: tuple[float, ...] | None = None,
) -> str:
    """Render retrieved memory as ``header + titled sections`` within ``budget_chars``.

    Each section gets a share of the budget proportional to its weight (default: equal); what a
    section leaves unused rolls over to the sections after it. Lines are kept in the given
    (relevance) order and clipped, never reordered. Empty sections are omitted; with nothing to
    show (or a budget too small for the header) the result is ``""``. Deterministic.
    """
    budget = min(max(0, budget_chars), MEMORY_HARD_CAP)
    live = [(title, lines) for title, lines in sections if lines]
    if not live or budget < len(MEMORY_HEADER) + _MIN_LINE:
        return ""
    raw = list(weights) if weights is not None else [1.0] * len(sections)
    if len(raw) != len(sections):
        raise ValueError("render_memory_block: one weight per section is required")
    live_weights = [w for (_, lines), w in zip(sections, raw, strict=True) if lines]
    remaining = budget - len(MEMORY_HEADER)
    out = [MEMORY_HEADER]
    for index, (title, lines) in enumerate(live):
        share_weight = sum(live_weights[index:]) or 1.0
        allowance = int(remaining * (live_weights[index] / share_weight))
        head = f"{title}:"
        if allowance < len(head) + 1 + _MIN_LINE:
            continue
        chunk = [head]
        used = len(head) + 1
        for line in lines:
            room = allowance - used - 1
            if room < _MIN_LINE:
                break
            clipped = _clip_line(line, room)
            chunk.append(clipped)
            used += len(clipped) + 1
        if len(chunk) > 1:
            out.append("\n".join(chunk))
            remaining -= used + 1
    return "\n".join(out) if len(out) > 1 else ""


def _witness_line(witness: str) -> str:
    """A witness in the prompt, with the command that runs it when it runs in the sandbox."""
    command = witness_command(witness)
    return f"- {witness} (run: {command})" if command else f"- {witness}"


def build_messages(
    *,
    anchor_text: str,
    snapshot: SituationSnapshot,
    item: ChecklistItem,
    specs: list[ToolSpec],
    mission_text: str = "",
    memory_text: str = "",
    engine: bool = False,
    code_map_text: str = "",
) -> list[ModelMessage]:
    """Build the initial messages for one cycle: anchor recitation + active item + tools.

    ``mission_text`` is the immutable mission recitation (always first); ``anchor_text`` is
    optional caller context (progress, research, reflections), included when it adds anything.
    ``memory_text`` is the already-budgeted memory block (``render_memory_block``); it is capped
    again at ``MEMORY_HARD_CAP`` here. ``code_map_text`` is the rendered code map
    (``render_code_map``), capped again at ``CODE_MAP_HARD_CAP``. ``engine``: the prompt for a ``claude_code`` lead session,
    whose tools are real tool definitions, so they are not listed and no JSON protocol is asked.
    """
    anchor = mission_text.strip() or anchor_text.strip()
    extra = anchor_text.strip() if mission_text.strip() else ""
    if extra and extra != anchor:
        if len(extra) > _EXTRA_CONTEXT_CAP:
            extra = "...[earlier context trimmed]...\n" + extra[-_EXTRA_CONTEXT_CAP:]
        anchor = f"{anchor}\n\nContext:\n{extra}"
    role = "You are the Lead Engineer. Make verified progress on ONE checklist item per cycle."
    if engine:
        system = f"{anchor}\n\n{role}\n\n{ENGINE_INSTRUCTIONS}"
    else:
        system = (
            f"{anchor}\n\n{role}\n\n"
            f"Available tools:\n{render_tools(specs)}\n\n"
            f"{ACTION_INSTRUCTIONS}\n{LEAD_DONE_INSTRUCTIONS}"
        )
    recent = "\n".join(snapshot.recent_commits[:10]) or "(none yet)"
    user = f"Active checklist item: [{item.id}] {item.description}\n\n"
    if snapshot.members:
        user += (
            "This workspace holds member repositories (git submodules), each a repository of "
            "its own with its own tests and tooling; paths below are relative to the workspace "
            f"root: {', '.join(snapshot.members)}\n\n"
        )
    if item.witnesses:
        listed = "\n".join(_witness_line(w) for w in item.witnesses)
        user += (
            "This item is done only when ALL of these acceptance checks (witnesses) pass, in "
            f"addition to the mission's checks:\n{listed}\n\n"
        )
    if item.last_failure:
        user += (
            f"Previous attempt #{item.attempts} FAILED verification:\n"
            f"{item.last_failure[-_LAST_FAILURE_CAP:]}\n\n"
        )
    memory = memory_text.strip()
    if memory:
        user += f"{_clip_line(memory, MEMORY_HARD_CAP)}\n\n"
    code_map = code_map_text.strip()
    if code_map:
        user += f"{_clip_line(code_map, CODE_MAP_HARD_CAP)}\n\n"
    decisions = render_decisions(snapshot.last_decisions)
    if decisions:
        user += f"{decisions}\n\n"
    finish = "call verify" if engine else "signal done"
    user += f"Recent commits:\n{recent}\n\nWork this item using the tools, then {finish}."
    return [
        ModelMessage(role="system", content=system),
        ModelMessage(role="user", content=user),
    ]
