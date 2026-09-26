"""Worker wiring + helpers to run the durable spine against a real Temporal server.

Tests construct their own worker against an in-process time-skipping test server (no Docker, no
API key); this module is the production entrypoint (``python -m lha.durable.worker``) and the
helpers shared by both.

Cross-language guard: the Python and Go Temporal SDKs number timers and activities differently,
so a history recorded by one implementation's worker does not replay on the other's. Workers
therefore mark their identity (``lha-py:<pid>@<host>`` here, ``lha-go:...`` in Go) and, before
polling, refuse to start when the task queue is already polled by the other implementation
(``check_task_queue_pollers``). Temporal lists a poller for a few minutes after it stopped.
"""

from __future__ import annotations

import asyncio
import os
import socket

from temporalio.api.enums.v1 import TaskQueueType
from temporalio.api.taskqueue.v1 import TaskQueue
from temporalio.api.workflowservice.v1 import DescribeTaskQueueRequest
from temporalio.client import Client
from temporalio.worker import Worker

from lha.config import Settings, get_settings
from lha.durable.activities import (
    check_mission_health,
    declare_impossible,
    notify_gate,
    read_mission_snapshot,
    record_mission_status,
    run_agent_cycle,
    unblock_items,
)
from lha.durable.agent_activities import run_subagent
from lha.durable.data_converter import build_data_converter
from lha.durable.org_activities import (
    integrate_branch,
    plan_round,
    review_cycle,
    run_implementer,
)
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.types import MissionInput, MissionResult
from lha.durable.workflows import MissionWorkflow


async def connect_client(settings: Settings | None = None) -> Client:
    """Connect to Temporal with the ClaimCheck data converter installed."""
    settings = settings or get_settings()
    return await Client.connect(
        settings.temporal_address,
        namespace=settings.temporal_namespace,
        data_converter=build_data_converter(object_store_root=settings.object_store_root),
    )


#: Marks the workers of each implementation in Temporal's poller list.
IDENTITY_MARKER = "lha-py"
GO_IDENTITY_MARKER = "lha-go"


def worker_identity() -> str:
    """This process's worker identity: ``lha-py:<pid>@<host>`` (the SDK default is
    ``<pid>@<host>``)."""
    return f"{IDENTITY_MARKER}:{os.getpid()}@{socket.gethostname()}"


class MixedWorkersError(RuntimeError):
    """The task queue is already polled by the Go implementation's workers."""


async def check_task_queue_pollers(client: Client, task_queue: str) -> None:
    """Raise ``MixedWorkersError`` when a poller of ``task_queue`` (workflow or activity tasks)
    is a Go lha worker (fail closed: one mission's history must stay with one implementation)."""
    for kind in (
        TaskQueueType.TASK_QUEUE_TYPE_WORKFLOW,
        TaskQueueType.TASK_QUEUE_TYPE_ACTIVITY,
    ):
        resp = await client.workflow_service.describe_task_queue(
            DescribeTaskQueueRequest(
                namespace=client.namespace,
                task_queue=TaskQueue(name=task_queue),
                task_queue_type=kind,
            )
        )
        for identity in sorted(p.identity for p in resp.pollers):
            if GO_IDENTITY_MARKER in identity:
                raise MixedWorkersError(
                    f"task queue {task_queue!r} is already polled by a Go lha worker ({identity}). "
                    "A Go and a Python worker cannot serve the same missions: their Temporal SDKs "
                    "number timers and activities differently, so a history recorded by one does "
                    "not replay on the other. Stop the Go workers (Temporal lists a poller for a "
                    "few minutes after it stops), or start this worker on another queue, e.g. "
                    f"LHA_TASK_QUEUE={task_queue}-py (and start its missions with the same "
                    "LHA_TASK_QUEUE)"
                )


def build_worker(client: Client, task_queue: str) -> Worker:
    """Construct a Worker that hosts the mission + sub-agent workflows and their activities."""
    return Worker(
        client,
        task_queue=task_queue,
        identity=worker_identity(),
        workflows=[MissionWorkflow, SubAgentWorkflow],
        activities=[
            run_agent_cycle,
            check_mission_health,
            notify_gate,
            declare_impossible,
            unblock_items,
            read_mission_snapshot,
            record_mission_status,
            run_subagent,
            plan_round,
            run_implementer,
            integrate_branch,
            review_cycle,
        ],
    )


async def start_mission(client: Client, inp: MissionInput, task_queue: str) -> MissionResult:
    """Start (or attach to) a mission workflow keyed by mission id and await its result.

    The workflow id IS the mission id, so re-invoking is idempotent (Temporal returns the running
    handle rather than starting a duplicate) — the basis for fleet-scale, one workflow per mission.
    """
    handle = await client.start_workflow(
        MissionWorkflow.run,
        inp,
        id=f"mission:{inp.mission_id}",
        task_queue=task_queue,
    )
    return await handle.result()


async def run_worker() -> None:
    """Connect to the configured Temporal server and serve missions until cancelled."""
    settings = get_settings()
    client = await connect_client(settings)
    await check_task_queue_pollers(client, settings.task_queue)
    worker = build_worker(client, settings.task_queue)
    async with worker:
        await asyncio.Event().wait()


if __name__ == "__main__":
    from lha.obs.otel import configure_tracing

    configure_tracing(component="worker")
    asyncio.run(run_worker())
