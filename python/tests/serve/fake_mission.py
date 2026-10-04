"""A fake mission workflow for the UI API conformance runner (``test_serve_contract.py``).

It answers the mission workflow's queries (the wire contract, docs/19-wire-contract.md) with fixed
state, as an older worker would (it has no ``steer_notes`` query), clears its gate once a decision
arrives, and records every signal it receives, so the runner can check what a server sent without a
worker or a model. Run as ``python -m tests.serve.fake_mission <temporal address>``: it starts the
workflow as ``mission:mission_durable``, prints ``ready`` and serves until killed.
"""

from __future__ import annotations

import asyncio
import sys
from typing import Any

from temporalio import workflow
from temporalio.client import Client
from temporalio.common import WorkflowIDConflictPolicy
from temporalio.worker import Worker

TASK_QUEUE = "lha-serve-conformance"
WORKFLOW_ID = "mission:mission_durable"
SIGNALS_QUERY = "conformance_signals"

#: When the fake sleeps until (``resume_at``): 2026-10-04T00:05:00.500000+00:00.
RESUME_AT = 1791072300.5
OPEN_QUESTION = "Which database should the API use?"

#: The open gate the fake reports (``gate_v1``; ``lha.durable.types.GateView``).
GATE: dict[str, Any] = {
    "gate_id": "g9",
    "kind": "tool_call",
    "question": "Run `git push review HEAD`?",
    "options": ["approve", "reject"],
    "default_action": "reject",
    "opened_at": "2026-10-04T00:01:00+00:00",
    "deadline": "2026-10-05T00:01:00+00:00",
    "escalations_sent": 0,
    "next_escalation_at": "",
    "recommended": "",
    "request": {
        "tool": "run_command",
        "arguments": "{'argv': ['git', 'push', 'review', 'HEAD']}",
        "reason": "publishes commits",
        "fingerprint": "f9",
    },
}


@workflow.defn(name="LhaServeFakeMission")
class FakeMission:
    def __init__(self) -> None:
        self.signals: list[list[Any]] = []
        self.edits = 0
        self.gate_open = True

    @workflow.run
    async def run(self) -> None:
        await workflow.wait_condition(lambda: False)

    @workflow.signal(name="steer_v1")
    def steer(self, note: str) -> None:
        self.signals.append(["steer_v1", note])

    @workflow.signal(name="snooze_v1")
    def snooze(self, seconds: int) -> None:
        self.signals.append(["snooze_v1", seconds])

    @workflow.signal(name="checklist_edit_v1")
    def edit(self, batch: dict[str, Any]) -> None:
        self.signals.append(["checklist_edit_v1", batch])
        self.edits += 1

    @workflow.signal(name="human_decision_v1")
    def decide_v1(self, decision: str) -> None:
        self.signals.append(["human_decision_v1", decision])

    @workflow.signal(name="human_decision_v2")
    def decide(self, decision: dict[str, Any]) -> None:
        self.signals.append(["human_decision_v2", decision])
        self.gate_open = False

    @workflow.query(name="status_v1")
    def status(self) -> str:
        return "RUNNING"

    @workflow.query(name="cycles_done")
    def cycles(self) -> int:
        return 3

    @workflow.query(name="gate_v1")
    def gate(self) -> dict[str, Any] | None:
        return GATE if self.gate_open else None

    @workflow.query(name="gate_log_v1")
    def gate_log(self) -> list[str]:
        return []

    @workflow.query(name="open_question")
    def open_question(self) -> str | None:
        return OPEN_QUESTION

    @workflow.query(name="resume_at")
    def resume_at(self) -> float | None:
        return RESUME_AT

    @workflow.query(name="pending_edits")
    def pending_edits(self) -> int:
        return self.edits

    @workflow.query(name=SIGNALS_QUERY)
    def received(self) -> list[list[Any]]:
        return list(self.signals)


async def main(address: str) -> None:
    client = await Client.connect(address)
    async with Worker(client, task_queue=TASK_QUEUE, workflows=[FakeMission]):
        await client.start_workflow(
            FakeMission.run,
            id=WORKFLOW_ID,
            task_queue=TASK_QUEUE,
            id_conflict_policy=WorkflowIDConflictPolicy.TERMINATE_EXISTING,
        )
        print("ready", flush=True)
        await asyncio.Event().wait()


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1]))
