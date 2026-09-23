"""Worker wiring + helpers to run the durable spine against a real Temporal server.

Tests construct their own worker against an in-process time-skipping test server (no Docker, no
API key); this module is the production entrypoint (``python -m lha.durable.worker``) and the
helpers shared by both.
"""

from __future__ import annotations

import asyncio

from temporalio.client import Client
from temporalio.worker import Worker

from lha.config import Settings, get_settings
from lha.durable.activities import (
    check_mission_health,
    declare_impossible,
    notify_gate,
    read_mission_snapshot,
    run_agent_cycle,
    unblock_items,
)
from lha.durable.agent_activities import run_subagent
from lha.durable.data_converter import build_data_converter
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


def build_worker(client: Client, task_queue: str) -> Worker:
    """Construct a Worker that hosts the mission + sub-agent workflows and their activities."""
    return Worker(
        client,
        task_queue=task_queue,
        workflows=[MissionWorkflow, SubAgentWorkflow],
        activities=[
            run_agent_cycle,
            check_mission_health,
            notify_gate,
            declare_impossible,
            unblock_items,
            read_mission_snapshot,
            run_subagent,
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
    worker = build_worker(client, settings.task_queue)
    async with worker:
        await asyncio.Event().wait()


if __name__ == "__main__":
    asyncio.run(run_worker())
