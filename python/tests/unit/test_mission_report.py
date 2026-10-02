"""``lha mission-report`` (docs/17-cli.md#lha-mission-report)."""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.config import Settings
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, EventRecord
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import GateEvent
from lha.state.mission_anchor import GitMissionAnchor

runner = CliRunner()


async def _seed(workdir: Path, sqlite: Path) -> None:
    items = Checklist(
        items=[
            ChecklistItem(id="01", description="do it"),
            ChecklistItem(id="02", description="then this"),
        ]
    )
    anchor = GitMissionAnchor(workdir)
    await anchor.initialize(title="Report test", description="two items", items=items)
    items.items[0].status, items.items[0].attempts = "done", 1
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=items,
            events=[
                EventRecord(kind="orchestrate", payload={"mission_id": "m1", "run": 1}),
                EventRecord(
                    kind="cycle", cycle_id="c1", payload={"item_id": "01", "verdict": "passed"}
                ),
                EventRecord(
                    kind="review", cycle_id="c1", payload={"item_id": "01", "verdict": "approve"}
                ),
            ],
        )
    )
    store = SqliteStore(sqlite)
    await store.open()
    try:
        await store.upsert_mission(
            mission_id="m1", title="Report test", status="RUNNING", head_sha="abc"
        )
        await store.record_gate_event(
            GateEvent(
                mission_id="m1",
                gate_id="m1:tool:fp1",
                kind="tool_call",
                event="opened",
                at="2026-01-01T01:00:00+00:00",
                question="Allow `git push`?",
                options=["approve", "reject"],
                default_action="reject",
                risk="high",
                request={"fingerprint": "fp1", "tool": "run_command", "argv": '["git", "push"]'},
            )
        )
    finally:
        await store.close()


def test_cli_renders_the_anchor_and_the_store(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    workdir, sqlite = tmp_path / "ws", tmp_path / "store" / "lha.sqlite3"
    workdir.mkdir()
    asyncio.run(_seed(workdir, sqlite))
    settings = Settings(_env_file=None, model_backend="stub", sqlite_path=str(sqlite))  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)

    result = runner.invoke(cli.app, ["mission-report", "--workdir", str(workdir)])
    assert result.exit_code == 0, result.output
    out = result.stdout
    assert out.startswith("# Mission: Report test\ntwo items\nmission m1  status RUNNING  head ")
    assert "  commits 2\n" in out
    assert (
        "## Items (1/2 done)\n01  done        attempts  1  do it\n02  todo        attempts  0  then this\n"
        in out
    )
    assert (
        "## Cycles (1)\nverdicts: passed 1, failed 0, other 0\nreviews: approve 1, block 0, unparsed 0\n"
        in out
    )
    assert (
        "## Gates (1)\n2026-01-01T01:00:00  m1  m1:tool:fp1  tool_call OPEN      reminders 0  open, default reject at -\n"
        in out
    )
    assert out.endswith("## Cost\nno cost ledger rows\n")

    # Store only.
    result = runner.invoke(cli.app, ["mission-report", "m1", "--workdir", str(tmp_path / "none")])
    assert result.exit_code == 0, result.output
    assert result.stdout.startswith(
        "# Mission: Report test\nmission m1  status RUNNING  head -  commits 0\n\n## Items (0/0 done)\n(no items)\n"
    )

    # Neither.
    result = runner.invoke(cli.app, ["mission-report", "--workdir", str(tmp_path / "none")])
    assert result.exit_code == 2 and "no mission anchor" in result.output
