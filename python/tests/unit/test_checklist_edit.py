"""Operator checklist edits: the ``edit_checklist`` activity and ``lha mission-edit``
(``lha.state.checklist_edit`` itself is pinned by ``spec/state/checklist_edit.json``)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.contracts.state import Checklist, ChecklistItem
from lha.durable.activities import EDIT_EVENT, _edit_checklist
from lha.durable.signals import SIGNAL_CHECKLIST_EDIT
from lha.durable.types import EditInput
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from tests.unit.test_hitl_ladder import _fake_client, _Handle

runner = CliRunner()


async def _anchored(workdir: Path) -> GitMissionAnchor:
    anchor = GitMissionAnchor(workdir)
    items = [ChecklistItem(id="01", description="one"), ChecklistItem(id="02", description="two")]
    await anchor.initialize(title="t", description="d", items=Checklist(items=items))
    return anchor


def _commits(workdir: Path) -> list[str]:
    return git_ops.run_git(workdir, "log", "--format=%s").splitlines()


@pytest.mark.asyncio
async def test_edit_activity_commits_once_and_is_idempotent(tmp_path: Path) -> None:
    anchor = await _anchored(tmp_path)
    inp = EditInput(
        mission_id="m",
        workdir=str(tmp_path),
        cycle_id="e1",
        edits=[
            {"op": "add", "description": "three (witness: cmd:true)"},
            {"op": "remove", "id": "02"},
        ],
        by="calvin",
    )
    first = await _edit_checklist(inp)
    assert first.advanced and first.note == "checklist edited by calvin: added 03; removed 02"
    assert (first.items_done, first.items_total) == (0, 2)
    checklist = await anchor.read_checklist()
    assert [i.id for i in checklist.items] == ["01", "03"]
    assert checklist.items[1].witnesses == ["cmd:true"]
    assert _commits(tmp_path)[0] == "lha: checklist edited by calvin"
    events = [e for e in await anchor.read_events() if e.kind == EDIT_EVENT]
    assert len(events) == 1 and events[0].cycle_id == "e1"
    assert events[0].payload["edits"] == inp.edits and events[0].payload["by"] == "calvin"
    # A retry of the same activity (same id, same batch) applies nothing again.
    again = await _edit_checklist(inp)
    assert again.advanced and again.note == first.note
    assert _commits(tmp_path).count("lha: checklist edited by calvin") == 1
    # The same id with a different batch (a local edit numbered the same) is a new edit.
    other = await _edit_checklist(
        EditInput(
            mission_id="m",
            workdir=str(tmp_path),
            cycle_id="e1",
            edits=[{"op": "remove", "id": "03"}],
        )
    )
    assert other.advanced and other.note == "checklist edited by an operator: removed 03"
    assert _commits(tmp_path)[0] == "lha: checklist edited by an operator"


@pytest.mark.asyncio
async def test_edit_activity_refuses_without_committing(tmp_path: Path) -> None:
    await _anchored(tmp_path)
    before = _commits(tmp_path)
    result = await _edit_checklist(
        EditInput(
            mission_id="m",
            workdir=str(tmp_path),
            cycle_id="e1",
            edits=[{"op": "remove", "id": "9"}],
        )
    )
    assert not result.advanced
    assert result.note == "checklist edit refused: edit #1: unknown item '9'"
    assert _commits(tmp_path) == before


def test_mission_edit_signals_the_batch(monkeypatch: pytest.MonkeyPatch) -> None:
    handle = _Handle(None)
    _fake_client(monkeypatch, handle)
    result = runner.invoke(
        cli.app,
        [
            "mission-edit",
            "m1",
            "--remove",
            "03",
            "--add",
            "Ship it (witness: cmd:true)",
            "--describe",
            "02=Parse it",
            "--depends",
            "02=",
            "--reopen",
            "01",
            "--block",
            "04",
            "--unblock",
            "05",
            "--as",
            " calvin ",
        ],
    )
    assert result.exit_code == 0, result.output
    assert result.output == (
        "mission m1: 7 checklist edits queued (applied before the next cycle; "
        "'lha mission-status' shows the outcome)\n"
    )
    assert handle.signals == [
        (
            SIGNAL_CHECKLIST_EDIT,
            {
                "edits": [
                    {"op": "edit", "id": "02", "description": "Parse it"},
                    {"op": "edit", "id": "02", "depends_on": []},
                    {"op": "reopen", "id": "01"},
                    {"op": "unblock", "id": "05"},
                    {"op": "block", "id": "04"},
                    {"op": "remove", "id": "03"},
                    {"op": "add", "description": "Ship it (witness: cmd:true)"},
                ],
                "by": "calvin",
            },
        )
    ]


def test_mission_edit_reads_a_batch_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    handle = _Handle(None)
    _fake_client(monkeypatch, handle)
    batch = tmp_path / "edits.json"
    batch.write_text(json.dumps({"edits": [{"op": "remove", "id": "01"}]}), encoding="utf-8")
    result = runner.invoke(cli.app, ["mission-edit", "m1", "--edits", str(batch), "--add", "x"])
    assert result.exit_code == 0, result.output
    assert "1 checklist edit queued" not in result.output and "2 checklist edits" in result.output
    assert handle.signals[0][1]["edits"] == [
        {"op": "remove", "id": "01"},
        {"op": "add", "description": "x"},
    ]
    bad = tmp_path / "bad.json"
    bad.write_text("[1, 2]", encoding="utf-8")
    assert runner.invoke(cli.app, ["mission-edit", "m1", "--edits", str(bad)]).exit_code == 2
    assert (
        runner.invoke(cli.app, ["mission-edit", "m1", "--edits", str(tmp_path / "no")]).exit_code
        == 2
    )


def test_mission_edit_validates_before_connecting(monkeypatch: pytest.MonkeyPatch) -> None:
    handle = _Handle(None)
    _fake_client(monkeypatch, handle)
    cases: list[tuple[list[str], str]] = [
        (["mission-edit", "m1"], "nothing to do"),
        (["mission-edit"], "give MISSION_ID"),
        (["mission-edit", "m1", "--workdir", ".", "--add", "x"], "not both"),
        (["mission-edit", "m1", "--describe", "nope"], "--describe expects ID=VALUE"),
        (["mission-edit", "m1", "--depends", "=01"], "--depends expects ID=VALUE"),
        (["mission-edit", "m1", "--add", "x", "--as", "x" * 201], "--as is longer than 200"),
        (["mission-edit", "m1", *[a for _ in range(51) for a in ("--add", "x")]], "at most 50"),
    ]
    for args, message in cases:
        result = runner.invoke(cli.app, args)
        assert result.exit_code == 2 and message in result.output, (args, result.output)
    assert handle.signals == []


def test_mission_edit_workdir_applies_locally(tmp_path: Path) -> None:
    import asyncio

    asyncio.run(_anchored(tmp_path))
    ok = runner.invoke(
        cli.app, ["mission-edit", "--workdir", str(tmp_path), "--add", "three", "--as", "me"]
    )
    assert ok.exit_code == 0, ok.output
    assert ok.output == "checklist edited by me: added 03\n"
    refused = runner.invoke(cli.app, ["mission-edit", "--workdir", str(tmp_path), "--remove", "zz"])
    assert refused.exit_code == 1 and "unknown item 'zz'" in refused.output
    # Local edits are numbered after the committed ones (e1, e2, ...).
    second = runner.invoke(cli.app, ["mission-edit", "--workdir", str(tmp_path), "--remove", "03"])
    assert second.exit_code == 0, second.output
    events = asyncio.run(GitMissionAnchor(tmp_path).read_events())
    assert [e.cycle_id for e in events if e.kind == EDIT_EVENT] == ["e1", "e2"]


def test_mission_status_shows_pending_edits(monkeypatch: pytest.MonkeyPatch) -> None:
    class _Pending(_Handle):
        async def query(self, name: str, **kw: Any) -> Any:
            if name == "pending_edits":
                return 2
            return await super().query(name, **kw)

    _fake_client(monkeypatch, _Pending(None))
    status = runner.invoke(cli.app, ["mission-status", "m1"]).output
    assert "checklist edits pending: 2 (applied before the next cycle)" in status
