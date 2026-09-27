"""Run-path plumbing on top of ``MissionStore``: the mission row and the persistent cost ledger.

* ``LedgerSink`` is installed as ``CostMeter.on_record``: every metered model call's ledger entry
  is written to ``cost_ledger`` under a deterministic idempotency key (``key_prefix`` + a per-sink
  sequence number), so a replayed write is a no-op. ``backfill`` writes entries recorded before
  the sink existed (e.g. the Planner's call, made before the mission id was known).
* ``MissionTracker`` upserts the ``missions`` row as the run moves RUNNING → DONE / ABORTED /
  IMPOSSIBLE (``status_for_stop`` maps a local runner's stop reason to the same terminal status
  the durable workflow reports); the durable cycle activity also writes WAITING_ON_HUMAN when a
  cycle queued an approval (``durable.activities.cycle_status``), and the workflow writes the
  statuses only it decides (SLEEPING, DEGRADED_PARK, an open gate, every final status) through
  the ``record_mission_status`` activity.

Persistence is an observer of the run, never a reason to fail it: store errors are logged and
counted (``failures``), not raised into the agent loop.
"""

from __future__ import annotations

from lha.durable.signals import (
    STATUS_ABORTED,
    STATUS_DONE,
    STATUS_IMPOSSIBLE,
    STATUS_RUNNING,
)
from lha.governor.cost import CostEntry
from lha.governor.metering import CostMeter
from lha.obs.events import get_logger
from lha.persistence.store import MissionStore

_log = get_logger("lha.persistence")


def status_for_stop(stopped_reason: str) -> str:
    """Terminal ``missions.status`` for a local runner's ``stopped_reason``.

    Mirrors the durable workflow: complete → DONE, deadlocked → IMPOSSIBLE, budget / max cycles /
    loop / model unavailable / error → ABORTED.
    """
    if stopped_reason == "complete":
        return STATUS_DONE
    if stopped_reason.startswith("deadlocked"):
        return STATUS_IMPOSSIBLE
    return STATUS_ABORTED


class LedgerSink:
    """Persists each ``CostEntry`` of a metered run to the store's ``cost_ledger``."""

    def __init__(self, store: MissionStore, mission_id: str, *, key_prefix: str = "") -> None:
        self._store = store
        self.mission_id = mission_id
        self._prefix = key_prefix
        self._seq = 0
        self.written = 0
        self.failures = 0

    async def __call__(self, entry: CostEntry) -> None:
        call_key = f"{self._prefix}#{self._seq}"
        self._seq += 1
        try:
            if await self._store.record_cost(self.mission_id, entry, call_key=call_key):
                self.written += 1
        except Exception as exc:
            self.failures += 1
            _log.warning(
                "cost_ledger_write_failed",
                mission_id=self.mission_id,
                error=f"{type(exc).__name__}: {exc}",
            )

    async def backfill(self, entries: list[CostEntry]) -> None:
        for entry in entries:
            await self(entry)

    def attach(self, meter: CostMeter) -> None:
        meter.on_record = self


class MissionTracker:
    """Keeps one ``missions`` row current for a run."""

    def __init__(
        self,
        store: MissionStore,
        mission_id: str,
        *,
        title: str = "",
        description: str = "",
        workflow_id: str | None = None,
    ) -> None:
        self._store = store
        self.mission_id = mission_id
        self._title = title
        self._description = description
        self._workflow_id = workflow_id
        self.failures = 0

    async def set_status(
        self, status: str, *, head_sha: str | None = None, reopen: bool = False
    ) -> None:
        """Upsert the row (a terminal status stays unless ``reopen``: see ``upsert_mission``)."""
        try:
            await self._store.upsert_mission(
                mission_id=self.mission_id,
                title=self._title,
                description=self._description,
                status=status,
                head_sha=head_sha or None,
                workflow_id=self._workflow_id,
                reopen=reopen,
            )
        except Exception as exc:
            self.failures += 1
            _log.warning(
                "mission_upsert_failed",
                mission_id=self.mission_id,
                status=status,
                error=f"{type(exc).__name__}: {exc}",
            )

    async def running(self, *, head_sha: str | None = None) -> None:
        await self.set_status(STATUS_RUNNING, head_sha=head_sha)

    async def finish(self, stopped_reason: str, *, head_sha: str | None = None) -> str:
        status = status_for_stop(stopped_reason)
        await self.set_status(status, head_sha=head_sha)
        return status
