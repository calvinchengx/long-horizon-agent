"""Everything a run path persists, opened together: store + mission row + cost ledger + memory.

``open_run_services`` is what ``lha run-local`` / ``mission`` / ``orchestrate`` (via the local
runner and the Orchestrator) and the Temporal activities call::

    services = await open_run_services(settings, mission_id=..., workdir=..., meter=meter, ...)
    try:
        await services.tracker.running()
        loop = AgentLoop(..., memory=services.memory)
        ...
    finally:
        await services.finish(stopped_reason, head_sha=head)   # local runners only
        await services.close()

It opens the configured ``MissionStore`` (SQLite unless ``LHA_POSTGRES_DSN``), installs a
``LedgerSink`` on ``meter`` so EVERY metered call lands in ``cost_ledger`` (``backfill`` also
writes calls the meter recorded before the mission id existed, e.g. the Planner's), and opens the
memory plane when ``memory_enabled``.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from lha.config import Settings
from lha.contracts.model import ModelProvider
from lha.contracts.system_one import SystemOneModel
from lha.governor.metering import CostMeter
from lha.memory.service import MissionMemory, open_mission_memory
from lha.obs.events import TraceRecorder
from lha.persistence.event_log import MissionEventLog
from lha.persistence.store import MissionStore, open_store
from lha.persistence.tracking import LedgerSink, MissionTracker
from lha.systemone.build import close_system_one


@dataclass
class RunServices:
    store: MissionStore
    tracker: MissionTracker
    sink: LedgerSink
    memory: MissionMemory | None
    meter: CostMeter | None = None
    system_one: SystemOneModel | None = None
    # The shared event record (``mission_events``) the run's recorder writes to, if it has one.
    events: MissionEventLog | None = None

    async def finish(self, stopped_reason: str, *, head_sha: str | None = None) -> str:
        return await self.tracker.finish(stopped_reason, head_sha=head_sha)

    async def close(self) -> None:
        if self.meter is not None and self.meter.on_record is self.sink:
            self.meter.on_record = None
        if self.events is not None:
            await self.events.aclose()  # before the store closes; best effort, never raises
        try:
            if self.memory is not None:
                await self.memory.close()
        finally:
            try:
                await close_system_one(self.system_one)
            finally:
                await self.store.close()


async def open_run_services(
    settings: Settings,
    *,
    mission_id: str,
    workdir: str | Path,
    meter: CostMeter,
    title: str = "",
    description: str = "",
    model: ModelProvider | None = None,
    recorder: TraceRecorder | None = None,
    key_prefix: str = "",
    backfill: bool = True,
    workflow_id: str | None = None,
    system_one: SystemOneModel | None = None,
) -> RunServices:
    """Open the store, hook the meter to the persistent ledger, and open memory.

    ``system_one`` (``lha.systemone.build_system_one``, metered by ``meter``) serves memory
    reranking; the returned services own it and close it.
    """
    try:
        store = await open_store(settings, workdir=workdir)
    except BaseException:
        await close_system_one(system_one)
        raise
    try:
        sink = LedgerSink(store, mission_id, key_prefix=key_prefix)
        if backfill:
            await sink.backfill(list(meter.ledger.entries))
        sink.attach(meter)
        tracker = MissionTracker(
            store,
            mission_id,
            title=title,
            description=description,
            workflow_id=workflow_id,
            workdir=workdir,
        )
        memory = await open_mission_memory(
            settings,
            store=store,
            workdir=workdir,
            mission_id=mission_id,
            model=model,
            recorder=recorder,
            system_one=system_one,
        )
    except BaseException:
        await store.close()
        await close_system_one(system_one)
        raise
    events = None
    if recorder is not None:
        events = MissionEventLog(store)
        recorder.listeners.append(events.add)
        events.start()
    return RunServices(
        store=store,
        tracker=tracker,
        sink=sink,
        memory=memory,
        meter=meter,
        system_one=system_one,
        events=events,
    )
