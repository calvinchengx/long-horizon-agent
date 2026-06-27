"""Mutation-testing wrapper (verifier-trust signal).

Coverage says lines RAN; mutation score says the tests would CATCH a bug in them. A high mutation
score is the strongest evidence the deterministic gate is trustworthy. Thin wrapper around a
mutation tool (e.g. ``mutmut``) that parses its summary.

Honesty rules: a missing tool, a timeout, or output with no parseable summary is an ERROR
(``MutationToolError``) — never a silent ``0.0`` score that looks like a measurement.

Supported summaries:
  * mutmut 2.x / 3.x status line: ``123/123  🎉 100 🫥 0  ⏰ 2  🤔 0  🙁 21  🔇 0``
    (🎉 killed, 🫥 no tests, ⏰ timeout, 🤔 suspicious, 🙁 survived, 🔇 skipped)
  * plain text ``N killed`` / ``N survived``
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from lha.contracts.sandbox import ExecResult, SandboxSession
from lha.execution.proc import run_proc

_KILLED_RE = re.compile(r"(\d+)\s+killed", re.IGNORECASE)
_SURVIVED_RE = re.compile(r"(\d+)\s+survived", re.IGNORECASE)
_EMOJI = {
    "killed": "\U0001f389",  # 🎉
    "no_tests": "\U0001fae5",  # 🫥
    "timeout": "⏰",  # ⏰
    "suspicious": "\U0001f914",  # 🤔
    "survived": "\U0001f641",  # 🙁
    "skipped": "\U0001f507",  # 🔇
}
_TOOL_MISSING_CODES = (126, 127)


class MutationToolError(RuntimeError):
    """The mutation tool could not run or produced no parseable summary."""


@dataclass
class MutationResult:
    killed: int
    survived: int
    raw_tail: str
    timeout: int = 0
    suspicious: int = 0
    no_tests: int = 0
    exit_code: int = 0

    @property
    def total(self) -> int:
        return self.killed + self.timeout + self.survived + self.suspicious + self.no_tests

    @property
    def score(self) -> float:
        """Fraction of mutants caught (killed or timed out) over all evaluated mutants."""
        total = self.total
        return (self.killed + self.timeout) / total if total else 0.0


def _last_count(text: str, emoji: str) -> int | None:
    matches = re.findall(re.escape(emoji) + r"\s*(\d+)", text)
    return int(matches[-1]) if matches else None


def parse_mutation_summary(text: str) -> MutationResult | None:
    """Parse a mutation summary from tool output; ``None`` if no summary is present."""
    counts = {key: _last_count(text, emoji) for key, emoji in _EMOJI.items()}
    if counts["killed"] is not None or counts["survived"] is not None:
        return MutationResult(
            killed=counts["killed"] or 0,
            survived=counts["survived"] or 0,
            timeout=counts["timeout"] or 0,
            suspicious=counts["suspicious"] or 0,
            no_tests=counts["no_tests"] or 0,
            raw_tail=text[-2000:],
        )
    killed = _KILLED_RE.findall(text)
    survived = _SURVIVED_RE.findall(text)
    if not killed and not survived:
        return None
    return MutationResult(
        killed=int(killed[-1]) if killed else 0,
        survived=int(survived[-1]) if survived else 0,
        raw_tail=text[-2000:],
    )


async def run_mutation_testing(
    workdir: str,
    *,
    command: list[str] | None = None,
    timeout_s: int = 1800,
    session: SandboxSession | None = None,
) -> MutationResult:
    """Run mutation testing (in ``session`` if given, else in ``workdir``) and parse the summary.

    Raises ``MutationToolError`` if the tool is missing, times out, or reports no summary.
    """
    cmd = command or ["uv", "run", "mutmut", "run"]
    result: ExecResult
    if session is not None:
        result = await session.exec(cmd, timeout_s=timeout_s)
    else:
        result = await run_proc(cmd, cwd=workdir, timeout_s=timeout_s)
    combined = f"{result.stdout}\n{result.stderr}"
    if result.timed_out:
        raise MutationToolError(f"mutation run timed out after {timeout_s}s")
    if result.exit_code in _TOOL_MISSING_CODES:
        raise MutationToolError(f"mutation tool not runnable ({cmd[0]}): {combined[-500:]}")
    parsed = parse_mutation_summary(combined)
    if parsed is None:
        # mutmut 2.x encodes mutant statuses as exit bit-flags, so a non-zero exit WITH a
        # summary is fine; a run without one (crash, usage error, empty suite) is not.
        raise MutationToolError(
            f"no mutation summary in output (exit {result.exit_code}): {combined[-500:]}"
        )
    parsed.exit_code = result.exit_code
    return parsed
