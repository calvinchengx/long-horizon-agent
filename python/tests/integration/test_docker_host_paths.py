"""Host-side readers see the Docker workspace at its HOST path, not at /workspace.

Inside the container the workspace is mounted at ``/workspace``, which is ``session.workdir``.
Code that reads the workspace from the host (harness integrity, ``list_files``, ``grep``, the
dispatcher's path checks, a Claude Code session's cwd) must use ``host_root(session)`` instead;
before that, under Docker, integrity snapshots were empty (tampering went unnoticed), the two
listing tools returned nothing, and a Claude Code lead could not even start.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ToolCall, TurnResult
from lha.contracts.sandbox import host_root
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.tools import ToolContext
from lha.contracts.verify import checks_from_commands
from lha.execution.tools.fs import GrepTool, ListFilesTool
from lha.model.stub import StubModel
from tests.integration.conftest import requires_docker
from tests.unit.test_claude_code import FAKE_CLAUDE

pytestmark = [pytest.mark.integration, requires_docker]
pytest.importorskip("docker")

IMAGE = "python:3.12-slim"


def _settings(**overrides: object) -> Settings:
    values: dict[str, object] = {
        "sandbox": "docker",
        "sandbox_image": IMAGE,
        "model_backend": "stub",
        "max_cycles": 1,
        "max_replans": 0,
        "budget_usd_ceiling": 5.0,
    }
    values.update(overrides)
    return Settings(_env_file=None, **values)  # type: ignore[call-arg]


def _checklist() -> Checklist:
    return Checklist(items=[ChecklistItem(id="01", description="Create hello.txt")])


async def test_list_files_and_grep_read_the_host_checkout(tmp_path: Path) -> None:
    from lha.execution.sandbox_docker import DockerSandbox

    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    (tmp_path / "pkg").mkdir()
    (tmp_path / "pkg" / "clock.go").write_text("package pkg\nfunc NowTime() {}\n")
    session = await DockerSandbox(image=IMAGE).open(workdir=str(tmp_path))
    try:
        assert session.workdir == "/workspace"
        assert host_root(session) == str(tmp_path.resolve())
        ctx = ToolContext(mission_id="m", session=session)
        listed = await ListFilesTool().run({}, ctx)
        assert listed.ok and "pkg/clock.go" in listed.content
        found = await GrepTool().run({"pattern": "NowTime"}, ctx)
        assert found.ok and "pkg/clock.go:2" in found.content
    finally:
        await session.close()


@pytest.mark.asyncio
async def test_harness_tampering_is_caught_in_docker(tmp_path: Path) -> None:
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "Makefile").write_text("check:\n\ttrue\n")
    subprocess.run(["git", "init", "-q", str(ws)], check=True)
    script = [
        TurnResult(
            tool_calls=[
                ToolCall(id="", name="write_file", arguments={"path": "hello.txt", "content": "x"}),
                ToolCall(
                    id="", name="write_file", arguments={"path": "Makefile", "content": "check:\n"}
                ),
            ],
            stop_reason="tool_use",
        ),
        TurnResult(text='{"done": true, "summary": "done"}', stop_reason="end_turn"),
    ]
    summary = await run_mission_local(
        workdir=str(ws),
        title="T",
        description="D",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_settings(harness_paths="Makefile", max_turns_per_cycle=2),
        model=StubModel(script=script),
    )
    assert not summary.completed
    checklist = json.loads((ws / ".lha" / "checklist.json").read_text())
    assert "harness_integrity" in checklist["items"][0]["last_failure"]
    assert (ws / "Makefile").read_text() == "check:\n\ttrue\n"  # the edit was reverted


@pytest.mark.asyncio
async def test_claude_code_lead_runs_on_the_host_checkout_with_a_docker_sandbox(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = tmp_path / "fake-claude"
    fake.write_text(f"#!{sys.executable}\n{FAKE_CLAUDE}")
    fake.chmod(0o755)
    monkeypatch.setenv("FAKE_CLAUDE_LOG", str(tmp_path / "claude.log"))
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "mcp")
    calls = [["write_file", {"path": "hello.txt", "content": "hi\n"}], ["verify", {}]]
    monkeypatch.setenv("FAKE_CLAUDE_CALLS", json.dumps(calls))
    ws = tmp_path / "ws"
    ws.mkdir()

    summary = await run_mission_local(
        workdir=str(ws),
        title="T",
        description="D",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_settings(
            lead_engine="claude_code", claude_code_bin=str(fake), claude_code_max_budget_usd=1.0
        ),
    )
    assert summary.completed, summary.stopped_reason
    (session,) = [json.loads(line) for line in (tmp_path / "claude.log").read_text().splitlines()]
    assert Path(session["cwd"]).resolve() == ws.resolve()
    assert (ws / "hello.txt").read_text() == "hi\n"  # written in the container, seen on the host
