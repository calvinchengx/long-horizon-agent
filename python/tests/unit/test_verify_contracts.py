"""Verification contract + verifier plane: no vacuous passes, sandboxed exec, output capture."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from lha.contracts.sandbox import ExecResult
from lha.contracts.verify import (
    Check,
    CheckResult,
    VerificationResult,
    checks_from_commands,
    derive_check_name,
    ensure_unique_check_names,
)
from lha.verify.flaky_quarantine import FlakeEvidenceError, FlakyQuarantine
from lha.verify.verifier import DeterministicVerifier, clip_output_tail, default_python_checks


class FakeSession:
    """Records exec calls; returns canned results by argv[0]. Never touches the host."""

    def __init__(self, results: dict[str, ExecResult], workdir: str = "/sandbox") -> None:
        self.workdir = workdir
        self.results = results
        self.calls: list[tuple[list[str], int]] = []

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        self.calls.append((argv, timeout_s))
        if argv[0] == "explode":
            raise RuntimeError("container gone")
        return self.results.get(argv[0], ExecResult(exit_code=0))

    async def write_file(self, relpath: str, content: str) -> None:
        return None

    async def read_file(self, relpath: str) -> str:
        return ""

    async def close(self) -> None:
        return None


# --- VerificationResult: zero gating checks is never green ----------------------------------
def test_empty_verification_is_unverified() -> None:
    assert VerificationResult(all_green=True).all_green is False
    assert VerificationResult(all_green=True).verdict == "unverified"
    assert VerificationResult.from_results([]).unverified


def test_only_advisory_results_are_unverified() -> None:
    result = VerificationResult.from_results(
        [CheckResult(name="judge", passed=True, exit_code=0, gating=False)]
    )
    assert not result.all_green and result.verdict == "unverified"


def test_from_results_respects_gating() -> None:
    result = VerificationResult.from_results(
        [
            CheckResult(name="pytest", passed=True, exit_code=0),
            CheckResult(name="judge", passed=False, exit_code=1, gating=False),
        ]
    )
    assert result.all_green and result.verdict == "passed"
    failing = result.with_results([CheckResult(name="x", passed=False, exit_code=2)])
    assert not failing.all_green and failing.verdict == "failed"
    assert "x FAILED (exit 2)" in failing.failure_report()


def test_check_requires_a_command() -> None:
    with pytest.raises(ValidationError):
        Check(name="empty", command=[])


# --- check naming (V5) ------------------------------------------------------------------------
def test_derive_check_name_skips_runners() -> None:
    assert derive_check_name(["uv", "run", "pytest", "-q"]) == "pytest"
    assert derive_check_name(["uv", "run", "--frozen", "ruff", "check", "."]) == "ruff"
    assert derive_check_name(["python3", "-m", "ty", "check"]) == "ty"
    assert derive_check_name(["/usr/bin/python3.12", "-c", "print(1)"]) == "python"
    assert derive_check_name(["npx", "tsc"]) == "tsc"


def test_checks_from_commands_are_unique() -> None:
    checks = checks_from_commands(
        [["uv", "run", "pytest", "tests/unit"], ["uv", "run", "pytest", "tests/int"], []]
    )
    assert [c.name for c in checks] == ["pytest", "pytest-2"]


def test_unique_names_reserve_harness_integrity() -> None:
    checks = ensure_unique_check_names([Check(name="harness_integrity", command=["x"])])
    assert checks[0].name == "harness_integrity-2"


def test_default_python_checks() -> None:
    assert [c.name for c in default_python_checks()] == ["ruff", "ty", "pytest"]
    assert all(c.gating for c in default_python_checks())


# --- DeterministicVerifier runs in the sandbox session (V4/V5) ------------------------------
@pytest.mark.asyncio
async def test_verifier_executes_through_session_and_captures_output() -> None:
    session = FakeSession(
        {
            "pytest": ExecResult(exit_code=1, stdout="..F\nFAILED test_x - assert 1 == 2"),
            "ruff": ExecResult(exit_code=0, stdout="All checks passed!"),
        }
    )
    verifier = DeterministicVerifier(default_timeout_s=77)
    result = await verifier.verify(
        session,
        [
            Check(name="ruff", command=["ruff", "check"]),
            Check(name="pytest", command=["pytest"], timeout_s=5),
        ],
    )
    assert [argv for argv, _ in session.calls] == [["ruff", "check"], ["pytest"]]
    assert [t for _, t in session.calls] == [77, 5]
    by_name = {r.name: r for r in result.results}
    assert "assert 1 == 2" in by_name["pytest"].output_tail
    assert by_name["pytest"].duration_s >= 0.0
    assert result.verdict == "failed"
    assert "assert 1 == 2" in result.failure_report()


@pytest.mark.asyncio
async def test_verifier_sandbox_error_is_a_failure() -> None:
    result = await DeterministicVerifier().verify(
        FakeSession({}), [Check(name="boom", command=["explode"])]
    )
    assert not result.all_green
    assert "container gone" in result.results[0].output_tail


@pytest.mark.asyncio
async def test_verifier_with_no_checks_is_unverified() -> None:
    result = await DeterministicVerifier().verify(FakeSession({}), [])
    assert result.verdict == "unverified" and not result.all_green


def test_clip_output_tail_keeps_the_end() -> None:
    clipped = clip_output_tail("a" * 5000 + "END", "ERR", limit=100)
    assert clipped.endswith("ERR") and "END" in clipped and len(clipped) < 150


# --- flaky quarantine needs evidence (V6) ----------------------------------------------------
def test_mark_flaky_requires_evidence() -> None:
    quarantine = FlakyQuarantine()
    with pytest.raises(FlakeEvidenceError):
        quarantine.mark_flaky("pytest")
    # Failing on different revisions is a regression, not a flake.
    quarantine.record("pytest", revision="r1", passed=True)
    quarantine.record("pytest", revision="r2", passed=False)
    quarantine.record("pytest", revision="r2", passed=False)
    with pytest.raises(FlakeEvidenceError):
        quarantine.mark_flaky("pytest")
    quarantine.record("pytest", revision="r2", passed=True)
    quarantine.mark_flaky("pytest")
    assert quarantine.is_flaky("pytest")
