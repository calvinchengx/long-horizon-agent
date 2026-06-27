"""Replay safety net: recorded MissionWorkflow histories must replay against the current code.

``histories/`` holds real histories recorded from this suite. If a workflow change would make an
in-flight mission's history non-deterministic, ``test_recorded_histories_still_replay`` fails
here, before a deploy could break a running mission. When a change is intentionally incompatible
(and shipped behind a new Build ID), re-record with::

    LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py
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

from lha.durable.activities import make_cycle_activity
from lha.durable.replay_test_harness import build_replayer, replay_histories
from lha.durable.workflows import MissionWorkflow
from tests.durability._support import SETTINGS, init_mission, working_model
from tests.durability.test_durable_spine import (
    _healthy,
    _snapshot_activity,
    _unblock_activity,
)

HISTORIES = Path(__file__).parent / "histories"
RECORDED = HISTORIES / "mission_three_items.json"


def _sanitized(history_json: str, workdir: Path) -> str:
    """Replace machine-specific paths inside payloads so the committed history is portable.

    Replay checks the commands a workflow issues, not the values it passes to activities, so
    rewriting paths inside payloads does not change what the history proves.
    """
    swaps = {str(workdir): "/workspace/mission", sys.executable: "python3"}
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


@pytest.mark.asyncio
async def test_recorded_histories_still_replay(tmp_path: Path) -> None:
    assert RECORDED.exists(), "record one with LHA_RECORD_HISTORY=1 (see module docstring)"
    replayed = await replay_histories(str(HISTORIES), object_store_root=tmp_path / "objects")
    assert replayed >= 1


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
