"""Verifier-trust bootstrap.

"Green" only means "correct" if the tests actually exercise the code. Before trusting the
deterministic gate on a brownfield repo (and before enabling parallel writers), measure coverage
(and, when available, mutation score). Until trust clears a threshold, the system should move
deliberately and force coverage of touched lines.

Measurement: run the suite under coverage; ONLY if it passes, read the total with
``coverage report --format=total`` (exact, handles branch coverage). If that command is
unavailable, fall back to parsing the ``TOTAL`` row of the test output. Failing tests or an
unreadable total mean "not trusted", with the reason recorded — never a fake ``0.0`` measurement.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from lha.contracts.sandbox import ExecResult, SandboxSession
from lha.execution.proc import run_proc

# The TOTAL row's LAST percentage (works with/without branch columns and fractional precision).
_TOTAL_RE = re.compile(r"^TOTAL\b.*?(\d+(?:\.\d+)?)%\s*$", re.MULTILINE)
_BARE_PCT_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*%?\s*$")


def parse_total_coverage(text: str) -> float | None:
    """Parse the total coverage fraction (0..1) from a coverage ``TOTAL`` row; None if absent."""
    matches = _TOTAL_RE.findall(text)
    return float(matches[-1]) / 100.0 if matches else None


def parse_coverage_pct(text: str) -> float:
    """Parse the total coverage fraction (0..1) from `pytest --cov` / `coverage` output (0.0 if
    absent — prefer ``parse_total_coverage`` to distinguish 'absent' from 'zero')."""
    pct = parse_total_coverage(text)
    return pct if pct is not None else 0.0


def parse_format_total(text: str) -> float | None:
    """Parse ``coverage report --format=total`` output (e.g. ``83`` or ``83.33``) as 0..1."""
    for line in reversed(text.strip().splitlines()):
        match = _BARE_PCT_RE.match(line)
        if match:
            return float(match.group(1)) / 100.0
    return None


@dataclass
class VerifierTrust:
    coverage_pct: float
    trusted: bool
    raw_tail: str
    tests_passed: bool = True
    measured: bool = True
    reason: str = ""


class TrustBootstrap:
    """Measures whether the deterministic gate is trustworthy enough to gate on."""

    def __init__(self, *, coverage_threshold: float = 0.6, timeout_s: int = 900) -> None:
        self._threshold = coverage_threshold
        self._timeout = timeout_s

    async def measure(
        self,
        workdir: str,
        *,
        command: list[str] | None = None,
        total_command: list[str] | None = None,
        session: SandboxSession | None = None,
    ) -> VerifierTrust:
        cmd = command or ["uv", "run", "pytest", "--cov", "--cov-report=term", "-q"]
        total_cmd = total_command or ["uv", "run", "coverage", "report", "--format=total"]

        async def _run(argv: list[str]) -> ExecResult:
            if session is not None:
                return await session.exec(argv, timeout_s=self._timeout)
            return await run_proc(argv, cwd=workdir, timeout_s=self._timeout)

        result = await _run(cmd)
        combined = f"{result.stdout}\n{result.stderr}"
        tail = combined[-2000:]
        if not result.ok:
            reason = (
                "tests timed out" if result.timed_out else f"tests failed (exit {result.exit_code})"
            )
            return VerifierTrust(
                coverage_pct=0.0,
                trusted=False,
                raw_tail=tail,
                tests_passed=False,
                measured=False,
                reason=f"{reason}; coverage not read",
            )

        pct: float | None = None
        total = await _run(total_cmd)
        if total.ok:
            pct = parse_format_total(total.stdout)
        if pct is None:
            pct = parse_total_coverage(combined)
        if pct is None:
            return VerifierTrust(
                coverage_pct=0.0,
                trusted=False,
                raw_tail=tail,
                measured=False,
                reason="could not read a coverage total",
            )
        trusted = pct >= self._threshold
        return VerifierTrust(
            coverage_pct=pct,
            trusted=trusted,
            raw_tail=tail,
            reason="" if trusted else f"coverage {pct:.1%} below threshold {self._threshold:.0%}",
        )
