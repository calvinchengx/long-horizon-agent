"""The verification contract — the only authority that can mark work "done".

This is the heart of long-horizon reliability: per-step errors compound, so progress is gated
by DETERMINISTIC checks (exit codes from tests/lint/build/typecheck), not by an LLM's say-so.
A checklist item flips to done ONLY when ``VerificationResult.all_green`` is True, and
``all_green`` is only ever True when at least one *gating* check actually ran and passed: a
verdict over zero gating checks is ``"unverified"``, never a vacuous pass.

Checks run inside the sandbox session (where the code lives), never on the orchestrating host.

Swapping the ``Verifier`` is how the engine adapts to other domains — but reliability is only
ever as good as the verifier the domain can provide (see docs/architecture.md, honesty policy).
"""

from __future__ import annotations

import re
from collections.abc import Iterable
from pathlib import PurePath
from typing import Literal, Protocol, runtime_checkable

from pydantic import BaseModel, Field, model_validator

from lha.contracts.sandbox import SandboxSession

Verdict = Literal["passed", "failed", "unverified"]

# Name of the harness-owned check that fails when pre-existing test/config files were tampered.
HARNESS_INTEGRITY_CHECK = "harness_integrity"


class Check(BaseModel):
    """A single deterministic check: an argv run in the sandbox session's work directory."""

    name: str
    command: list[str] = Field(min_length=1)
    # If True, a non-zero exit blocks progress. Advisory checks (e.g. an LLM-judge) set False.
    gating: bool = True
    # Per-check timeout override (seconds); ``None`` uses the verifier's default.
    timeout_s: int | None = None


class CheckResult(BaseModel):
    """The real outcome of running a ``Check``."""

    name: str
    passed: bool
    exit_code: int
    gating: bool = True
    duration_s: float = 0.0
    timed_out: bool = False
    # Clipped tail of the check's stdout/stderr, so the agent can see WHY a check failed.
    output_tail: str = ""
    # Pointer to the full log in the object store (claim-check), to keep results compact.
    output_ref: str | None = None


class VerificationResult(BaseModel):
    """Aggregate verdict over a set of checks.

    ``all_green`` and ``verdict`` are always DERIVED from ``results`` (any value passed in is
    overridden): ``all_green`` requires at least one gating result and every gating result to
    pass. With no gating results the verdict is ``"unverified"`` and ``all_green`` is False.
    """

    all_green: bool = False
    verdict: Verdict = "unverified"
    results: list[CheckResult] = Field(default_factory=list)

    @model_validator(mode="after")
    def _derive_verdict(self) -> VerificationResult:
        gating = [r for r in self.results if r.gating]
        if not gating:
            self.all_green = False
            self.verdict = "unverified"
        else:
            self.all_green = all(r.passed for r in gating)
            self.verdict = "passed" if self.all_green else "failed"
        return self

    @classmethod
    def from_results(cls, results: list[CheckResult]) -> VerificationResult:
        """Build a verdict; ``all_green`` requires >=1 gating check and all gating checks pass."""
        return cls(results=list(results))

    @property
    def unverified(self) -> bool:
        """True when no gating check ran — the item can NOT be considered done."""
        return self.verdict == "unverified"

    def with_results(self, extra: list[CheckResult]) -> VerificationResult:
        """Return a new verdict including ``extra`` results (e.g. the harness-integrity check)."""
        return VerificationResult.from_results([*self.results, *extra])

    def failure_report(self, *, per_check_chars: int = 1500) -> str:
        """A compact, model-facing explanation of why this verdict is not green."""
        if self.unverified:
            return (
                "UNVERIFIED: no gating checks ran, so the item cannot be marked done. "
                "Configure at least one gating check for this mission."
            )
        lines: list[str] = []
        for r in self.results:
            if r.passed or not r.gating:
                continue
            status = "timed out" if r.timed_out else f"exit {r.exit_code}"
            tail = r.output_tail[-per_check_chars:] if r.output_tail else "(no output)"
            lines.append(f"- {r.name} FAILED ({status}):\n{tail}")
        return "\n".join(lines) if lines else "all gating checks passed"


@runtime_checkable
class Verifier(Protocol):
    """Runs deterministic checks and returns ground-truth pass/fail. Impl: ``src/lha/verify/``."""

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        """Run ``checks`` inside ``session`` (its workdir) and return the aggregate verdict."""
        ...


# --- check naming (durable/CLI callers build checks from raw argv lists) ----------------------

_RUNNER_TOKENS = frozenset(
    {
        "uv",
        "uvx",
        "run",
        "exec",
        "poetry",
        "pipenv",
        "pdm",
        "hatch",
        "rye",
        "npx",
        "npm",
        "pnpm",
        "yarn",
        "bunx",
        "-m",
    }
)
_PYTHON_RE = re.compile(r"^python(\d+(\.\d+)?)?(\.exe)?$", re.IGNORECASE)
_UNSAFE_NAME_CHARS = re.compile(r"[^A-Za-z0-9_.-]+")


def derive_check_name(command: list[str]) -> str:
    """Derive a readable check name from argv, skipping runner prefixes (``uv run``, ``python -m``).

    ``["uv", "run", "pytest", "-q"]`` -> ``"pytest"``. Not unique on its own; see
    ``ensure_unique_check_names`` / ``checks_from_commands``.
    """
    interpreter = ""
    for token in command:
        if token == "-c":  # inline code (``python -c "..."``): name it after the interpreter
            return interpreter or "check"
        if not token or token.startswith("-"):
            continue
        base = PurePath(token).name
        if _PYTHON_RE.match(base):
            interpreter = "python"
            continue
        if token in _RUNNER_TOKENS or base in _RUNNER_TOKENS:
            continue
        name = _UNSAFE_NAME_CHARS.sub("_", base).strip("_.-")[:40]
        if name:
            return name
    return "check"


def ensure_unique_check_names(
    checks: Iterable[Check], *, reserved: Iterable[str] = (HARNESS_INTEGRITY_CHECK,)
) -> list[Check]:
    """Return ``checks`` with duplicate (or reserved) names suffixed ``-2``, ``-3``, ... in order."""
    taken = set(reserved)
    out: list[Check] = []
    for check in checks:
        name = check.name
        if name in taken:
            n = 2
            while f"{name}-{n}" in taken:
                n += 1
            name = f"{name}-{n}"
        taken.add(name)
        out.append(check if name == check.name else check.model_copy(update={"name": name}))
    return out


def checks_from_commands(commands: Iterable[list[str]], *, gating: bool = True) -> list[Check]:
    """Build uniquely-named ``Check``s from raw argv lists (empty argv lists are skipped)."""
    return ensure_unique_check_names(
        Check(name=derive_check_name(cmd), command=list(cmd), gating=gating)
        for cmd in commands
        if cmd
    )
