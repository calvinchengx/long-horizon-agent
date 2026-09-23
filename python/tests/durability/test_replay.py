"""Replay safety net: recorded MissionWorkflow histories must replay against the current code.

``histories/`` holds real histories recorded from this suite. If a workflow change would make an
in-flight mission's history non-deterministic, ``test_recorded_histories_still_replay`` fails
here, before a deploy could break a running mission. When a change is intentionally incompatible
(and shipped behind a new Build ID), re-record ONE history with::

    LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py -k <test>

Compatible changes do NOT re-record: new workflow behaviour is guarded by ``workflow.patched`` so
older histories keep replaying down their old code path. The committed histories:

* ``mission_three_items.json`` — a plain three-item mission;
* ``mission_deadlock_gate_legacy.json`` / ``mission_approval_gate_legacy.json`` — a deadlock gate
  answered "retry" and an irreversible-action gate answered "approve", recorded with the workflow
  code from BEFORE the escalation ladder (proving the ``lha-gate-escalation-v1`` guard: without it
  they fail replay with a nondeterminism error);
* ``mission_approval_ladder.json`` — a real mission whose queued ``git push`` is approved at a
  gate on the escalation ladder (the ``lha-gate-escalation-v1`` path);
* ``mission_row_gate_retry.json`` — a real mission that deadlocks, is retried by a human at the
  deadlock gate and completes, writing the ``missions`` row from the workflow
  (``record_mission_status``, the ``lha-mission-row-v1`` path). Every older history above
  predates that patch and replays without those activities.
"""

from __future__ import annotations

import base64
import json
import os
import sys
from pathlib import Path

import pytest
from temporalio.client import WorkflowHistory
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.durable.activities import declare_impossible, make_cycle_activity, notify_gate
from lha.durable.replay_test_harness import build_replayer, replay_histories
from lha.durable.signals import SIGNAL_HUMAN_DECISION
from lha.durable.workflows import MissionWorkflow
from tests.durability import test_durable_spine as spine
from tests.durability._support import SETTINGS, init_mission, working_model
from tests.durability.test_durable_spine import (
    ROW_ACTIVITY,
    _healthy,
    _snapshot_activity,
    _unblock_activity,
)
from tests.durability.test_human_gates import gated_argv, gated_model

HISTORIES = Path(__file__).parent / "histories"
RECORDED = HISTORIES / "mission_three_items.json"
RECORDED_LADDER = HISTORIES / "mission_approval_ladder.json"
RECORDED_ROW = HISTORIES / "mission_row_gate_retry.json"


def _sanitized(history_json: str, workdir: Path, *more: Path) -> str:
    """Replace machine-specific paths inside payloads so the committed history is portable.

    Replay checks the commands a workflow issues, not the values it passes to activities, so
    rewriting paths inside payloads does not change what the history proves.
    """
    swaps = {str(workdir): "/workspace/mission", sys.executable: "python3"}
    swaps.update({str(path): "/workspace" for path in more})
    data = json.loads(history_json)

    def scrub(node: object) -> None:
        if isinstance(node, dict):
            raw = node.get("data")
            if isinstance(raw, str) and "metadata" in node:
                text = base64.b64decode(raw).decode("utf-8", "surrogateescape")
                for old, new in swaps.items():
                    text = text.replace(json.dumps(old)[1:-1], json.dumps(new)[1:-1])
                node["data"] = base64.b64encode(text.encode("utf-8", "surrogateescape")).decode()
            for value in node.values():
                scrub(value)
        elif isinstance(node, list):
            for value in node:
                scrub(value)

    scrub(data)
    return json.dumps(data, indent=1)


async def _record(tmp_path: Path) -> WorkflowHistory:
    inp = await init_mission(tmp_path / "work", n=3)
    task_queue = "lha-replay"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue=task_queue,
            workflows=[MissionWorkflow],
            activities=[
                make_cycle_activity(settings=SETTINGS, model_factory=working_model),
                _healthy,
                _unblock_activity,
                _snapshot_activity,
                ROW_ACTIVITY,
            ],
        ),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=task_queue
        )
        result = await handle.result()
        assert result.completed
        return await handle.fetch_history()


@pytest.mark.asyncio
async def test_fresh_history_replays(tmp_path: Path) -> None:
    history = await _record(tmp_path)
    out = tmp_path / "histories"
    out.mkdir()
    (out / "fresh.json").write_text(history.to_json(), encoding="utf-8")
    if os.environ.get("LHA_RECORD_HISTORY") == "1":
        HISTORIES.mkdir(exist_ok=True)
        RECORDED.write_text(_sanitized(history.to_json(), tmp_path / "work"), encoding="utf-8")
    assert await replay_histories(str(out), object_store_root=tmp_path / "objects") == 1


async def _record_approval_ladder(tmp_path: Path) -> WorkflowHistory:
    """A real 2-item mission: cycle 1 queues a flagged ``git push``; a human approves it at the
    gate (the decision is sent early and held); cycle 2 runs it and completes."""
    work = tmp_path / "work"
    inp = await init_mission(work, n=2)
    task_queue = "lha-replay-ladder"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue=task_queue,
            workflows=[MissionWorkflow],
            activities=[
                make_cycle_activity(
                    settings=SETTINGS, model_factory=gated_model(gated_argv(tmp_path / "ran.log"))
                ),
                notify_gate,
                declare_impossible,
                _healthy,
                _unblock_activity,
                _snapshot_activity,
                ROW_ACTIVITY,
            ],
        ),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=task_queue
        )
        await handle.signal(SIGNAL_HUMAN_DECISION, "approve")
        result = await handle.result()
        assert result.completed and result.cycles == 2
        return await handle.fetch_history()


@pytest.mark.asyncio
async def test_fresh_approval_ladder_history_replays(tmp_path: Path) -> None:
    history = await _record_approval_ladder(tmp_path)
    out = tmp_path / "histories"
    out.mkdir()
    (out / "fresh.json").write_text(history.to_json(), encoding="utf-8")
    if os.environ.get("LHA_RECORD_HISTORY") == "1":
        HISTORIES.mkdir(exist_ok=True)
        RECORDED_LADDER.write_text(
            _sanitized(history.to_json(), tmp_path / "work", tmp_path), encoding="utf-8"
        )
    assert await replay_histories(str(out), object_store_root=tmp_path / "objects") == 1


async def _record_mission_row(tmp_path: Path) -> WorkflowHistory:
    """A real 1-item mission: three failed cycles block the item, the deadlock gate opens (the row
    reads WAITING_ON_HUMAN), a human's early "retry" unblocks it, it completes (row DONE)."""
    spine._gate["idle_cycles"] = 3
    work = tmp_path / "work"
    inp = await init_mission(work, n=1, deadlock_gate_seconds=3600)
    task_queue = "lha-replay-row"
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        Worker(
            env.client,
            task_queue=task_queue,
            workflows=[MissionWorkflow],
            activities=[
                make_cycle_activity(settings=SETTINGS, model_factory=spine._fails_three_times),
                notify_gate,
                declare_impossible,
                _healthy,
                _unblock_activity,
                _snapshot_activity,
                ROW_ACTIVITY,
            ],
        ),
    ):
        handle = await env.client.start_workflow(
            MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=task_queue
        )
        await handle.signal(SIGNAL_HUMAN_DECISION, "retry")
        result = await handle.result()
        assert result.completed and result.cycles == 4
        return await handle.fetch_history()


@pytest.mark.asyncio
async def test_fresh_mission_row_history_replays(tmp_path: Path) -> None:
    history = await _record_mission_row(tmp_path)
    out = tmp_path / "histories"
    out.mkdir()
    (out / "fresh.json").write_text(history.to_json(), encoding="utf-8")
    if os.environ.get("LHA_RECORD_HISTORY") == "1":
        HISTORIES.mkdir(exist_ok=True)
        RECORDED_ROW.write_text(
            _sanitized(history.to_json(), tmp_path / "work", tmp_path), encoding="utf-8"
        )
    assert await replay_histories(str(out), object_store_root=tmp_path / "objects") == 1


@pytest.mark.asyncio
async def test_recorded_histories_still_replay(tmp_path: Path) -> None:
    assert RECORDED.exists(), "record one with LHA_RECORD_HISTORY=1 (see module docstring)"
    names = {p.name for p in HISTORIES.glob("*.json")}
    assert {
        "mission_three_items.json",
        "mission_deadlock_gate_legacy.json",
        "mission_approval_gate_legacy.json",
        "mission_approval_ladder.json",
        "mission_row_gate_retry.json",
    } <= names
    replayed = await replay_histories(str(HISTORIES), object_store_root=tmp_path / "objects")
    assert replayed == len(names)


def _events(path: Path) -> list[dict[str, object]]:
    return json.loads(path.read_text(encoding="utf-8"))["events"]


def _scheduled(path: Path) -> list[str]:
    return [
        e["activityTaskScheduledEventAttributes"]["activityType"]["name"]  # type: ignore[index]
        for e in _events(path)
        if "activityTaskScheduledEventAttributes" in e
    ]


def _patches(path: Path) -> list[str]:
    out = []
    for e in _events(path):
        attrs = e.get("markerRecordedEventAttributes")
        if isinstance(attrs, dict) and attrs.get("markerName") == "core_patch":
            for payload in attrs["details"]["patch-data"]["payloads"]:
                out.append(json.loads(base64.b64decode(payload["data"]))["id"])
    return out


def test_committed_histories_cover_what_they_claim() -> None:
    """The legacy histories predate the ladder; the ladder history really went through it; only
    the mission-row history went through ``record_mission_status``."""
    legacy_deadlock = HISTORIES / "mission_deadlock_gate_legacy.json"
    legacy_approval = HISTORIES / "mission_approval_gate_legacy.json"
    assert _patches(legacy_deadlock) == [] and "unblock_items" in _scheduled(legacy_deadlock)
    assert _patches(legacy_approval) == [] and "notify_gate" not in _scheduled(legacy_approval)
    assert _patches(RECORDED_LADDER) == ["lha-gate-escalation-v1"]
    assert _scheduled(RECORDED_LADDER) == [
        "run_agent_cycle",
        "notify_gate",
        "notify_gate",
        "run_agent_cycle",
    ]
    for older in (RECORDED, legacy_deadlock, legacy_approval, RECORDED_LADDER):
        assert "lha-mission-row-v1" not in _patches(older)
        assert "record_mission_status" not in _scheduled(older)
    assert _patches(RECORDED_ROW) == ["lha-gate-escalation-v1", "lha-mission-row-v1"]
    assert _scheduled(RECORDED_ROW) == [
        "run_agent_cycle",
        "run_agent_cycle",
        "run_agent_cycle",
        "record_mission_status",  # WAITING_ON_HUMAN: the deadlock gate opened
        "notify_gate",
        "notify_gate",
        "unblock_items",
        "run_agent_cycle",
        "record_mission_status",  # DONE
    ]


@pytest.mark.asyncio
async def test_replay_detects_a_changed_workflow(tmp_path: Path) -> None:
    """A history whose recorded commands no longer match the code must fail replay."""
    data = json.loads(RECORDED.read_text(encoding="utf-8"))
    renamed = 0
    for event in data["events"]:
        attrs = event.get("activityTaskScheduledEventAttributes")
        if attrs and attrs["activityType"]["name"] == "run_agent_cycle":
            attrs["activityType"]["name"] = "some_other_activity"
            renamed += 1
    assert renamed, "the recorded history should schedule run_agent_cycle"
    history = WorkflowHistory.from_json("tampered", json.dumps(data))
    replayer = build_replayer(object_store_root=tmp_path / "objects")
    with pytest.raises(Exception, match=r"(?i)nondetermin"):
        await replayer.replay_workflow(history)
