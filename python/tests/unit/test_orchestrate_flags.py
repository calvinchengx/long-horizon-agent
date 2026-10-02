"""``lha orchestrate --research N --review/--no-review`` reach the Orchestrator."""

from __future__ import annotations

from pathlib import Path
from typing import Any, ClassVar

import pytest
from typer.testing import CliRunner

import lha.agents.orchestrator as orchestrator_module
import lha.cli.main as cli
from lha.agent.runner import MissionSummary
from lha.config import Settings

runner = CliRunner()


class _FakeOrchestrator:
    seen: ClassVar[list[dict[str, Any]]] = []

    def __init__(self, settings: Settings, **kwargs: Any) -> None:
        _FakeOrchestrator.seen.append(kwargs)

    async def run_mission(self, **kwargs: Any) -> MissionSummary:
        return MissionSummary(
            mission_id="m",
            completed=True,
            cycles=1,
            items_done=1,
            items_total=1,
            total_usd=0.0,
            head_sha="abc",
            stopped_reason="complete",
            trace_jsonl="",
        )


def test_orchestrate_passes_research_and_review(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    settings = Settings(
        _env_file=None, model_backend="stub", sandbox="local", allow_unsafe_local=True
    )  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    monkeypatch.setattr(orchestrator_module, "Orchestrator", _FakeOrchestrator)
    roadmap = tmp_path / "roadmap.md"
    roadmap.write_text("# T\n\n## Build\n\n- [ ] say hello\n", encoding="utf-8")
    base = [
        "orchestrate",
        "--checklist",
        str(roadmap),
        "--sandbox",
        "local",
        "--unsafe-local",
        "--no-default-checks",
        "--check",
        "true",
    ]
    _FakeOrchestrator.seen.clear()
    assert runner.invoke(cli.app, [*base, "--workdir", str(tmp_path / "a")]).exit_code == 0
    assert (
        runner.invoke(
            cli.app, [*base, "--workdir", str(tmp_path / "b"), "--no-review", "--research", "0"]
        ).exit_code
        == 0
    )
    assert [(k["research_per_item"], k["do_review"]) for k in _FakeOrchestrator.seen] == [
        (2, True),
        (0, False),
    ]
    assert (
        runner.invoke(
            cli.app, [*base, "--workdir", str(tmp_path / "c"), "--research", "5"]
        ).exit_code
        == 2
    )
