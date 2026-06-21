"""``DeterministicVerifier`` — the only authority that can mark work "done".

Runs each ``Check`` through the sandbox session's ``exec`` (so checks run WHERE the code lives —
container/VM/local workdir — never on the orchestrating host with its inherited environment) and
aggregates exit codes into a ``VerificationResult``. This is exit-code ground truth, not an LLM's
opinion — the single most important lever on long-horizon reliability (per-step errors compound,
so every step is gated by a deterministic check). Advisory (non-gating) checks are recorded but
never block, and a run with zero gating checks is ``unverified`` (never a vacuous pass).
"""

from __future__ import annotations

import time

from lha.contracts.sandbox import SandboxSession
from lha.contracts.verify import (
    Check,
    CheckResult,
    VerificationResult,
    checks_from_commands,
    ensure_unique_check_names,
)

# Tail of each check's output kept on the CheckResult (what the agent sees on failure).
OUTPUT_TAIL_CHARS = 4000


def clip_output_tail(stdout: str, stderr: str, *, limit: int = OUTPUT_TAIL_CHARS) -> str:
    """Combine stdout/stderr and keep the last ``limit`` chars (the end is where failures are)."""
    parts = [p for p in (stdout.rstrip(), stderr.rstrip()) if p]
    combined = "\n--- stderr ---\n".join(parts) if len(parts) == 2 else "".join(parts)
    if len(combined) <= limit:
        return combined
    return "...[truncated]...\n" + combined[-limit:]


class DeterministicVerifier:
    """Implements the ``Verifier`` protocol by running real check commands in the sandbox."""

    def __init__(
        self, *, default_timeout_s: int = 1200, output_tail_chars: int = OUTPUT_TAIL_CHARS
    ) -> None:
        self._timeout = default_timeout_s
        self._tail = output_tail_chars

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        results: list[CheckResult] = []
        for check in ensure_unique_check_names(checks, reserved=()):
            results.append(await self._run_one(session, check))
        # The gate requires >=1 gating check and every GATING check to pass (see contract).
        return VerificationResult.from_results(results)

    async def _run_one(self, session: SandboxSession, check: Check) -> CheckResult:
        started = time.monotonic()
        try:
            outcome = await session.exec(check.command, timeout_s=check.timeout_s or self._timeout)
        except Exception as exc:  # a sandbox failure is a failed check, never a pass
            return CheckResult(
                name=check.name,
                passed=False,
                exit_code=-1,
                gating=check.gating,
                duration_s=time.monotonic() - started,
                output_tail=f"[verifier] could not execute check: {type(exc).__name__}: {exc}",
            )
        return CheckResult(
            name=check.name,
            passed=outcome.ok,
            exit_code=outcome.exit_code,
            gating=check.gating,
            duration_s=time.monotonic() - started,
            timed_out=outcome.timed_out,
            output_tail=clip_output_tail(outcome.stdout, outcome.stderr, limit=self._tail),
        )


DEFAULT_PYTHON_CHECK_COMMANDS: tuple[tuple[str, ...], ...] = (
    ("uv", "run", "ruff", "check", "."),
    ("uv", "run", "ty", "check"),
    ("uv", "run", "pytest", "-q"),
)


def default_python_checks() -> list[Check]:
    """The standard deterministic gate for a uv-managed Python repo: ruff, ty, pytest.

    This is the sensible default for run paths that were not given explicit checks.
    """
    return checks_from_commands([list(cmd) for cmd in DEFAULT_PYTHON_CHECK_COMMANDS])
