"""The independent Reviewer role wrapper.

It runs in FRESH context (no shared trace with the author) — the highest-leverage reliability
investment. It is a thin wrapper over ``SubAgent`` with the role's prompt and a scoped dispatcher.

The Reviewer returns a STRUCTURED verdict parsed from JSON (``verdict`` + ``blocking_issues`` +
``advisory``). Free-text substring matching is not used ("No blocking issues" is not a block). If
the verdict cannot be parsed, the result is conservative: blocking (an unreviewable change is not
approved), unless explicit ``BLOCK:`` lines were given, which are then the blocking issues.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from lha.agents.roles import ROLES
from lha.agents.subagent import SubAgent
from lha.contracts.model import ModelProvider
from lha.contracts.tools import ToolContext, ToolDispatcher

_DIFF_CAP = 8000

_APPROVE = frozenset({"approve", "approved", "pass", "lgtm", "accept", "ok"})
_BLOCK = frozenset({"block", "blocked", "blocking", "reject", "request_changes", "fail"})


def extract_json_object(text: str) -> dict[str, object] | None:
    """The outermost ``{...}`` in ``text`` parsed as a JSON object, or ``None``."""
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end == -1 or end < start:
        return None
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return None
    return parsed if isinstance(parsed, dict) else None


_REVIEW_OBJECTIVE = (
    "Review the diff against the acceptance criteria. Identify correctness, security, and scope "
    "issues. Investigate with the tools if needed. When finished, reply with EXACTLY one JSON "
    'object: {"done": true, "verdict": "approve" | "block", '
    '"blocking_issues": ["<issue that must be fixed before merge>", ...], '
    '"advisory": ["<non-blocking suggestion>", ...], "summary": "<one line>"}. '
    'Use "block" only if blocking_issues is non-empty.'
)


@dataclass
class ReviewResult:
    brief: str
    blocking: bool
    tool_calls: int
    verdict: str = "unparsed"  # "approve" | "block" | "unparsed"
    blocking_issues: list[str] = field(default_factory=list)
    advisory: list[str] = field(default_factory=list)

    def notes(self) -> str:
        """Human/agent-readable review notes (used when reopening an item)."""
        lines = [f"Review verdict: {self.verdict}"]
        lines += [f"- BLOCKING: {issue}" for issue in self.blocking_issues]
        lines += [f"- advisory: {note}" for note in self.advisory]
        if not self.blocking_issues and self.brief:
            lines.append(self.brief[:1000])
        return "\n".join(lines)


def _str_list(value: object) -> list[str]:
    if not isinstance(value, list):
        return []
    return [str(v).strip() for v in value if str(v).strip()]


def parse_review(text: str) -> tuple[str, bool, list[str], list[str]]:
    """Parse a review reply into ``(verdict, blocking, blocking_issues, advisory)``."""
    obj = extract_json_object(text)
    if obj is not None and ("verdict" in obj or "blocking_issues" in obj):
        issues = _str_list(obj.get("blocking_issues"))
        advisory = _str_list(obj.get("advisory"))
        raw = obj.get("verdict")
        verdict = raw.strip().lower() if isinstance(raw, str) else ""
        if verdict in _BLOCK:
            return "block", True, issues, advisory
        if verdict in _APPROVE:
            # An "approve" that still lists blocking issues is contradictory: trust the issues.
            return ("block", True, issues, advisory) if issues else ("approve", False, [], advisory)
        if isinstance(obj.get("blocking_issues"), list):
            return ("block", True, issues, advisory) if issues else ("approve", False, [], advisory)
        return "unparsed", True, issues, advisory

    # Fallback: explicit BLOCK: lines, else conservative (unparseable => not approved).
    marked = [
        line.strip()[len("BLOCK:") :].strip()
        for line in text.splitlines()
        if line.strip().upper().startswith("BLOCK:")
    ]
    return ("block" if marked else "unparsed"), True, marked, []


class Reviewer:
    """Adversarial, fresh-context diff review."""

    def __init__(self, model: ModelProvider, dispatcher: ToolDispatcher) -> None:
        self._agent = SubAgent(role=ROLES["reviewer"], model=model, dispatcher=dispatcher)

    async def review(self, *, diff: str, criteria: str, ctx: ToolContext) -> ReviewResult:
        result = await self._agent.run(
            objective=_REVIEW_OBJECTIVE,
            ctx=ctx,
            extra_context=f"Acceptance criteria: {criteria}\n\nDIFF:\n{diff[:_DIFF_CAP]}",
        )
        verdict, blocking, issues, advisory = parse_review(result.final_text or result.brief)
        return ReviewResult(
            brief=result.brief,
            blocking=blocking,
            tool_calls=result.tool_calls,
            verdict=verdict,
            blocking_issues=issues,
            advisory=advisory,
        )
