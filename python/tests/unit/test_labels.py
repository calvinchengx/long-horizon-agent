"""``lha labels export``: the mission's judgments as JSON Lines labels (docs/25-system-one.md)."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agents.reviewer import REVIEW_EVENT
from lha.config import Settings
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, EventRecord
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import GateEvent, GateRow
from lha.state.mission_anchor import GitMissionAnchor
from lha.systemone.labels import (
    DIFF_CAP,
    LABEL_SCHEMA,
    label_rows,
    mission_id_of,
    to_jsonl,
)

runner = CliRunner()


def _approval(fingerprint: str, decision: str, *, defaulted: bool = False) -> EventRecord:
    return EventRecord(
        kind="tool_approval",
        cycle_id="c1",
        payload={
            "tool": "run_command",
            "arguments": '{"argv": ["git", "push"], "token": "sk-ant-abcdefghijklmnop1234"}',
            "reason": "pushes to a remote",
            "fingerprint": fingerprint,
            "decision": decision,
            "approved": decision == "approve",
            "resolved_by": "timeout" if defaulted else "terminal:calvin",
            "defaulted": defaulted,
        },
    )


def _cycle(item: str, verdict: str, **extra: object) -> EventRecord:
    return EventRecord(
        kind="cycle",
        cycle_id="c2",
        payload={
            "item_id": item,
            "verified": verdict == "passed",
            "verdict": verdict,
            "status": "done" if verdict == "passed" else "in_progress",
            "tool_calls": 4,
            "split_into": [],
            "rolled_back": [],
            "checks": [
                {
                    "name": "pytest",
                    "passed": verdict == "passed",
                    "gating": True,
                    "exit_code": 0 if verdict == "passed" else 1,
                    "duration_s": 1.234,
                },
            ],
            **extra,
        },
    )


def _review(item: str, verdict: str) -> EventRecord:
    return EventRecord(
        kind=REVIEW_EVENT,
        cycle_id="c3",
        payload={
            "item_id": item,
            "verdict": verdict,
            "blocking": verdict == "block",
            "blocking_issues": ["deletes a test"] if verdict == "block" else [],
            "advisory": ["rename x"],
            "reopened": verdict == "block",
            "blocked": False,
            "base": "aaa111",
            "head": "bbb222",
        },
    )


def _gate(fingerprint: str, status: str, decision: str = "approve") -> GateRow:
    return GateRow(
        mission_id="m1",
        gate_id=f"m1:tool:{fingerprint}",
        kind="tool_call",
        status=status,
        question="Allow `git push`? Authorization: Bearer hunter2secret",
        options=["approve", "reject"],
        default_action="reject",
        risk="high",
        decision=decision if status != "OPEN" else None,
        resolved_by="terminal:calvin" if status == "RESOLVED" else "timeout",
        request={"fingerprint": fingerprint, "tool": "run_command", "argv": '["git", "push"]'},
        opened_at=f"2026-01-01T0{fingerprint[-1]}:00:00+00:00",
        resolved_at=f"2026-01-01T0{fingerprint[-1]}:05:00+00:00",
    )


def test_rows_come_from_events_then_closed_gates_and_dedupe_recorded_gates() -> None:
    events = [
        EventRecord(kind="orchestrate", payload={"mission_id": "m1", "run": 1}),
        _approval("fp1", "approve"),  # also a gate row: dropped here, kept as the gate
        _approval("fp9", "reject", defaulted=True),  # no gate: the store was not available
        _cycle("01", "failed"),
        EventRecord(kind="cycle", cycle_id="c9", payload={"outcome": "impossible"}),  # no verdict
        _review("01", "block"),
        EventRecord(kind="lease_granted", payload={"path": "x"}),
    ]
    gates = [_gate("fp1", "RESOLVED"), _gate("fp2", "DEFAULTED", "reject"), _gate("fp3", "OPEN")]
    rows = label_rows(events, gates)
    assert [(r.source, r.label, r.by) for r in rows] == [
        ("tool_approval", "reject", "default"),
        ("verifier", "failed", "verifier"),
        ("review", "block", "reviewer"),
        ("gate", "approve", "terminal:calvin"),
        ("gate", "reject", "default"),
    ]
    assert {r.mission_id for r in rows} == {"m1"}
    assert [r.cycle_id for r in rows] == ["c1", "c2", "c3", "", ""]
    assert [r.item_id for r in rows] == ["", "01", "01", "", ""]
    assert rows[3].at == "2026-01-01T01:05:00+00:00" and rows[0].at == ""


def test_inputs_are_redacted_and_keep_only_stable_fields() -> None:
    events = [_approval("fp9", "approve"), _cycle("01", "passed"), _review("01", "approve")]
    approval, verifier, review = label_rows(events, [_gate("fp1", "RESOLVED")])[:3]
    assert "sk-ant-" not in json.dumps(approval.input)
    assert set(approval.input) == {"tool", "arguments", "reason", "fingerprint"}
    assert verifier.input["checks"] == [
        {"name": "pytest", "passed": True, "gating": True, "exit_code": 0}
    ]
    assert "duration_s" not in json.dumps(verifier.input)
    assert set(verifier.input) == {
        "status",
        "verified",
        "tool_calls",
        "rolled_back",
        "split_into",
        "checks",
    }
    assert review.input == {
        "base": "aaa111",
        "head": "bbb222",
        "blocking": False,
        "blocking_issues": [],
        "advisory": ["rename x"],
    }
    gate = label_rows([], [_gate("fp1", "RESOLVED")])[0]
    assert "hunter2secret" not in gate.input["question"]  # type: ignore[operator]
    assert gate.input["request"] == {
        "fingerprint": "fp1",
        "tool": "run_command",
        "argv": '["git", "push"]',
    }


def test_review_rows_carry_the_diff_only_when_asked_and_capped() -> None:
    calls: list[tuple[str, str]] = []

    def diffs(base: str, head: str) -> str:
        calls.append((base, head))
        return "x" * (DIFF_CAP + 10)

    (row,) = label_rows([_review("01", "approve")], [], diffs=diffs)
    assert calls == [("aaa111", "bbb222")]
    assert len(row.input["diff"]) == DIFF_CAP  # type: ignore[arg-type]
    (bare,) = label_rows([_review("01", "approve")], [])
    assert "diff" not in bare.input


def test_mission_id_comes_from_the_latest_orchestrate_event_or_the_caller() -> None:
    events = [
        EventRecord(kind="orchestrate", payload={"mission_id": "m1"}),
        EventRecord(kind="orchestrate", payload={"mission_id": "m2", "resumed": True}),
        _cycle("01", "passed"),
    ]
    assert mission_id_of(events) == "m2"
    assert mission_id_of([_cycle("01", "passed")]) == ""
    assert label_rows(events, [])[0].mission_id == "m2"
    assert label_rows(events, [], mission_id="given")[0].mission_id == "given"


def test_jsonl_has_sorted_keys_and_the_schema_number() -> None:
    text = to_jsonl(label_rows([_cycle("01", "passed")], []))
    assert text.endswith("\n") and text.count("\n") == 1
    obj = json.loads(text)
    assert list(obj) == sorted(obj) and obj["schema"] == LABEL_SCHEMA
    assert obj["source"] == "verifier" and obj["label"] == "passed"


# --- the command -------------------------------------------------------------------------------
def _checklist() -> Checklist:
    return Checklist(items=[ChecklistItem(id="01", description="do it", checks=[])])


async def _seed(workdir: Path, sqlite: Path) -> None:
    anchor = GitMissionAnchor(workdir)
    await anchor.initialize(title="T", description="D", items=_checklist())
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=_checklist(),
            events=[
                EventRecord(kind="orchestrate", payload={"mission_id": "m1", "run": 1}),
                _approval("fp1", "approve"),
                _approval("fp9", "reject", defaulted=True),
                _cycle("01", "passed"),
                _review("01", "approve"),
            ],
        )
    )
    store = SqliteStore(sqlite)
    await store.open()
    try:
        for event, decision, by in (("opened", "", ""), ("resolved", "approve", "terminal:calvin")):
            await store.record_gate_event(
                GateEvent(
                    mission_id="m1",
                    gate_id="m1:tool:fp1",
                    kind="tool_call",
                    event=event,
                    at=f"2026-01-01T01:0{5 if decision else 0}:00+00:00",
                    question="Allow `git push`?",
                    options=["approve", "reject"],
                    default_action="reject",
                    decision=decision,
                    resolved_by=by,
                    risk="high",
                    request={"fingerprint": "fp1", "tool": "run_command"},
                )
            )
    finally:
        await store.close()


def test_cli_exports_the_anchor_and_the_missions_gates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    workdir, sqlite = tmp_path / "ws", tmp_path / "store" / "lha.sqlite3"
    workdir.mkdir()
    asyncio.run(_seed(workdir, sqlite))
    settings = Settings(_env_file=None, model_backend="stub", sqlite_path=str(sqlite))  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)

    result = runner.invoke(cli.app, ["labels", "export", "--workdir", str(workdir)])
    assert result.exit_code == 0, result.output
    rows = [json.loads(line) for line in result.stdout.splitlines()]
    assert [(r["source"], r["label"]) for r in rows] == [
        ("tool_approval", "reject"),
        ("verifier", "passed"),
        ("review", "approve"),
        ("gate", "approve"),
    ]
    assert all(r["mission_id"] == "m1" for r in rows)
    assert "diff" not in rows[2]["input"]
    assert "4 labels (1 gate, 1 tool_approval, 1 verifier, 1 review)" in result.stderr

    out = tmp_path / "labels.jsonl"
    result = runner.invoke(
        cli.app, ["labels", "export", "--workdir", str(workdir), "--out", str(out), "--diffs"]
    )
    assert result.exit_code == 0, result.output
    assert result.stdout == ""
    rows = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
    assert rows[2]["input"]["diff"].startswith("(no new commits)") or "diff" in rows[2]["input"]

    # A mission id alone exports the store's gates.
    result = runner.invoke(cli.app, ["labels", "export", "m1", "--workdir", str(tmp_path / "none")])
    assert result.exit_code == 0, result.output
    assert [json.loads(line)["source"] for line in result.stdout.splitlines()] == ["gate"]

    # Neither an anchor nor a mission id: a clean error.
    result = runner.invoke(cli.app, ["labels", "export", "--workdir", str(tmp_path / "none")])
    assert result.exit_code == 2 and "no mission anchor" in result.output
