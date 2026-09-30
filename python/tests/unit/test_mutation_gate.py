"""The opt-in mutation gate: a green verdict must also survive LHA_MUTATION_CHECK, which sees the
changed files; it never runs on a red or unverified verdict and never makes an item green."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

from lha.agent.assembly import lead_verifier
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, TurnResult
from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check, CheckResult, VerificationResult
from lha.execution.sandbox_local import LocalSandboxSession
from lha.model.stub import StubModel
from lha.verify.flaky_quarantine import FlakyRetryVerifier
from lha.verify.mutation_gate import (
    CHANGED_FILES_ENV,
    MUTATION_CHECK_NAME,
    MutationGateVerifier,
    changed_files,
)


class _Fixed:
    """A verifier returning one verdict (``passed`` / ``failed`` / ``unverified``)."""

    def __init__(self, verdict: str) -> None:
        self._verdict = verdict

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        if self._verdict == "unverified":
            return VerificationResult.from_results([])
        ok = self._verdict == "passed"
        return VerificationResult.from_results(
            [CheckResult(name="tests", passed=ok, exit_code=0 if ok else 1)]
        )

    def drain_events(self) -> list[object]:
        return ["inner-event"]


def _git(cwd: Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=cwd, check=True, capture_output=True, text=True
    ).stdout


def _repo(tmp_path: Path) -> Path:
    repo = tmp_path / "ws"
    repo.mkdir()
    _git(repo, "init", "-q")
    _git(
        repo,
        "-c",
        "user.name=t",
        "-c",
        "user.email=t@t",
        "commit",
        "-q",
        "--allow-empty",
        "-m",
        "i",
    )
    return repo


def _gate(repo: Path, inner: str, command: str) -> MutationGateVerifier:
    return MutationGateVerifier(_Fixed(inner), command=command, workdir=repo, timeout_s=60)


# The command records what it saw, then exits with the given code.
def _recorder(out: Path, code: int) -> str:
    return f'printf "%s" "${CHANGED_FILES_ENV}" > {out}; echo "survived: parser.py:42 > to >="; exit {code}'


def test_changed_files_are_tracked_edits_and_untracked_files_outside_lha(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    (repo / "kept.py").write_text("a\n")
    (repo / ".gitignore").write_text("ignored.log\n")
    _git(repo, "add", "kept.py", ".gitignore")
    _git(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c")
    assert changed_files(repo) == []
    (repo / "kept.py").write_text("b\n")
    (repo / "new file.py").write_text("x\n")
    (repo / "ignored.log").write_text("x\n")
    (repo / ".lha").mkdir()
    (repo / ".lha" / "checklist.json").write_text("{}")
    assert changed_files(repo) == ["kept.py", "new file.py"]


@pytest.mark.asyncio
async def test_a_green_verdict_must_survive_the_mutation_check(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    (repo / "parser.py").write_text("x = 1\n")
    seen = tmp_path / "seen"
    session = LocalSandboxSession(str(repo))
    red = await _gate(repo, "passed", _recorder(seen, 1)).verify(session, [])
    assert red.verdict == "failed" and seen.read_text() == "parser.py"
    result = next(r for r in red.results if r.name == MUTATION_CHECK_NAME)
    assert (
        result.exit_code == 1 and result.gating and "survived: parser.py:42" in result.output_tail
    )
    assert "survived: parser.py:42" in red.failure_report()
    green = await _gate(repo, "passed", _recorder(seen, 0)).verify(session, [])
    assert green.verdict == "passed" and [r.name for r in green.results] == ["tests", "mutation"]


@pytest.mark.asyncio
@pytest.mark.parametrize("inner", ["failed", "unverified"])
async def test_it_never_runs_on_a_red_or_unverified_verdict(tmp_path: Path, inner: str) -> None:
    repo = _repo(tmp_path)
    (repo / "parser.py").write_text("x = 1\n")
    seen = tmp_path / "seen"
    result = await _gate(repo, inner, _recorder(seen, 0)).verify(LocalSandboxSession(str(repo)), [])
    assert result.verdict == inner and not seen.exists()


@pytest.mark.asyncio
async def test_it_does_not_run_without_changes(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    seen = tmp_path / "seen"
    result = await _gate(repo, "passed", _recorder(seen, 1)).verify(
        LocalSandboxSession(str(repo)), []
    )
    assert result.verdict == "passed" and not seen.exists()


@pytest.mark.asyncio
async def test_it_fails_closed_when_it_cannot_list_the_changes(tmp_path: Path) -> None:
    not_a_repo = tmp_path / "plain"
    not_a_repo.mkdir()
    result = await _gate(not_a_repo, "passed", "exit 0").verify(
        LocalSandboxSession(str(not_a_repo)), []
    )
    assert result.verdict == "failed"
    assert "cannot list the changed files" in result.results[-1].output_tail


@pytest.mark.asyncio
async def test_a_sandbox_failure_is_a_failed_check(tmp_path: Path) -> None:
    class _Broken(LocalSandboxSession):
        async def exec(self, argv: list[str], **kwargs: object) -> object:  # type: ignore[override]
            raise OSError("sandbox gone")

    repo = _repo(tmp_path)
    (repo / "parser.py").write_text("x = 1\n")
    result = await _gate(repo, "passed", "exit 0").verify(_Broken(str(repo)), [])
    assert result.verdict == "failed" and "OSError: sandbox gone" in result.results[-1].output_tail


def test_events_are_forwarded_and_lead_verifier_wraps_only_when_set(tmp_path: Path) -> None:
    assert _gate(tmp_path, "passed", "exit 0").drain_events() == ["inner-event"]
    assert isinstance(lead_verifier(str(tmp_path), Settings(_env_file=None)), FlakyRetryVerifier)  # type: ignore[call-arg]
    gated = lead_verifier(
        str(tmp_path),
        Settings(_env_file=None, mutation_check="mutmut run", mutation_timeout_s=30),  # type: ignore[call-arg]
    )
    assert isinstance(gated, MutationGateVerifier) and gated._timeout == 30
    assert gated.drain_events() == []  # FlakyRetryVerifier's (none yet)


def _mission_settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 2,
        "max_replans": 0,
        "memory_enabled": False,
    }
    base.update(overrides)
    return Settings(_env_file=None, **base)  # type: ignore[call-arg]


class _WritesThenDone(StubModel):
    """Writes parser.py, then (after the write's tool result) signals done, in every cycle and
    after every failed verification (a failed attempt is rolled back)."""

    def __init__(self) -> None:
        super().__init__(script=[])

    async def complete(self, messages: list[ModelMessage], **kwargs: object) -> TurnResult:
        if messages and messages[-1].role == "tool":
            return TurnResult(text='{"done": true, "summary": "ok"}')
        write = ToolCall(
            id="w", name="write_file", arguments={"path": "parser.py", "content": "x = 1\n"}
        )
        return TurnResult(tool_calls=[write], stop_reason="tool_use")


@pytest.mark.asyncio
@pytest.mark.parametrize(("code", "completed"), [(0, True), (1, False)])
async def test_a_local_mission_is_gated_by_the_mutation_check(
    tmp_path: Path, code: int, completed: bool
) -> None:
    seen = tmp_path / "seen"
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="write the parser")]),
        checks=[Check(name="tests", command=[sys.executable, "-c", "pass"])],
        settings=_mission_settings(mutation_check=_recorder(seen, code)),
        model=_WritesThenDone(),
    )
    assert summary.completed is completed
    assert seen.read_text() == "parser.py"
