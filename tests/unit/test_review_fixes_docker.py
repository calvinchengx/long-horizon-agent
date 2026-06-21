"""Regression tests for the Docker exit-code review findings."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from lha.execution import sandbox_docker
from lha.execution.sandbox_docker import DockerSandboxSession

# --- 8. docker exit codes --------------------------------------------------------------------


@dataclass
class _API:
    inspections: list[dict[str, Any]]
    calls: int = 0

    def exec_create(self, container_id: str, **_: Any) -> dict[str, str]:
        return {"Id": "e1"}

    def exec_start(self, exec_id: str, *, stream: bool, demux: bool) -> Any:
        yield (b"out", None)

    def exec_inspect(self, exec_id: str) -> dict[str, Any]:
        info = self.inspections[min(self.calls, len(self.inspections) - 1)]
        self.calls += 1
        return info


@dataclass
class _Container:
    api: _API
    id: str = "c1"
    runs: list[Any] = field(default_factory=list)

    @property
    def client(self) -> Any:
        return self


def _session(*inspections: dict[str, Any]) -> DockerSandboxSession:
    return DockerSandboxSession(_Container(api=_API(list(inspections))))


@pytest.fixture
def _fast_clock(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(sandbox_docker, "_sleep", lambda _s: None)


@pytest.mark.asyncio
@pytest.mark.usefixtures("_fast_clock")
async def test_unknown_exit_code_is_not_success() -> None:
    result = await _session({"Running": False, "ExitCode": None}).exec(["true"], timeout_s=5)
    assert result.exit_code == -1 and not result.ok
    assert "exit status unknown" in result.stderr


@pytest.mark.asyncio
@pytest.mark.usefixtures("_fast_clock")
async def test_waits_for_process_to_finish_after_stream_closes() -> None:
    session = _session(
        {"Running": True, "ExitCode": None},
        {"Running": True, "ExitCode": None},
        {"Running": False, "ExitCode": 3},
    )
    result = await session.exec(["prog"], timeout_s=5)
    assert result.exit_code == 3 and not result.timed_out


@pytest.mark.asyncio
@pytest.mark.usefixtures("_fast_clock")
async def test_exit_137_before_deadline_is_a_kill_not_a_timeout() -> None:
    result = await _session({"Running": False, "ExitCode": 137}).exec(["prog"], timeout_s=60)
    assert not result.timed_out and result.exit_code == 137
    assert "SIGKILL" in result.stderr


@pytest.mark.asyncio
@pytest.mark.usefixtures("_fast_clock")
async def test_program_exiting_124_before_deadline_is_not_a_timeout() -> None:
    result = await _session({"Running": False, "ExitCode": 124}).exec(["prog"], timeout_s=60)
    assert not result.timed_out and result.exit_code == 124


@pytest.mark.asyncio
async def test_exit_124_after_deadline_is_a_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    ticks = iter([0.0, 10.0])
    monkeypatch.setattr(sandbox_docker, "_clock", lambda: next(ticks))
    result = await _session({"Running": False, "ExitCode": 124}).exec(["prog"], timeout_s=2)
    assert result.timed_out and result.exit_code == -1
