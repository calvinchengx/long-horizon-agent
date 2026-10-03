"""The mission's shared event record: a run's trace events persisted to ``mission_events``.

Every run path attaches one to its ``TraceRecorder`` (through ``open_run_services``), so any reader
(``lha serve``, ``lha mission-report``, SQL) can follow a run without its logs, whichever
implementation runs it (docs/27-mission-ui.md). Events are stamped when recorded and written in
order, in batches, about once a second while the run goes on and once more when it ends.

Best effort: a store failure is logged and that batch is dropped (counted in ``dropped``); it never
fails a run. At most ``MAX_PENDING`` events wait for a write; beyond that the oldest are dropped.
"""

from __future__ import annotations

import asyncio
import contextlib
from datetime import UTC, datetime

import structlog

from lha.obs.events import TraceEvent
from lha.persistence.store import MissionEvent, MissionStore

#: How often pending events are written while a run goes on.
FLUSH_EVERY_S = 1.0
#: The most events waiting for a write (a store that is down for long loses the oldest).
MAX_PENDING = 10_000


def _now() -> str:
    return datetime.now(UTC).isoformat(timespec="microseconds")


class MissionEventLog:
    """Batches a run's trace events into ``mission_events``; attach ``add`` as a recorder listener."""

    def __init__(
        self,
        store: MissionStore,
        *,
        flush_every_s: float = FLUSH_EVERY_S,
        max_pending: int = MAX_PENDING,
    ) -> None:
        self._store = store
        self._every = flush_every_s
        self._max = max_pending
        self._pending: list[MissionEvent] = []
        self._lock = asyncio.Lock()
        self._task: asyncio.Task[None] | None = None
        self.written = 0
        self.dropped = 0

    def add(self, event: TraceEvent) -> None:
        """Queue one recorded event (already redacted by the recorder), stamped now."""
        self._pending.append(
            MissionEvent(event.mission_id, event.cycle_id, event.kind, dict(event.data), ts=_now())
        )
        if len(self._pending) > self._max:
            excess = len(self._pending) - self._max
            del self._pending[:excess]
            self.dropped += excess

    async def flush(self) -> None:
        """Write everything pending, in order; a failure drops the batch and is logged."""
        async with self._lock:
            batch, self._pending = self._pending, []
            if not batch:
                return
            try:
                await self._store.append_mission_events(batch)
                self.written += len(batch)
            except Exception as exc:
                self.dropped += len(batch)
                structlog.get_logger("lha.persistence").warning(
                    "mission_events_write_failed",
                    events=len(batch),
                    error=f"{type(exc).__name__}: {exc}",
                )

    def start(self) -> None:
        """Flush every ``flush_every_s`` seconds in the background until ``aclose``."""
        if self._task is None:
            self._task = asyncio.create_task(self._run())

    async def _run(self) -> None:
        while True:
            await asyncio.sleep(self._every)
            await self.flush()

    async def aclose(self) -> None:
        """Stop the background flush and write what is left."""
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
            self._task = None
        await self.flush()
