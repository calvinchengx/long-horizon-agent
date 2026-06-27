"""In-memory episodic log (the $0/offline episodic tier).

The git anchor's ``events.ndjson`` is the durable episodic record; this is a fast in-process view
for a single mission session (append + tail), used to feed consolidation and reflection.
"""

from __future__ import annotations

from lha.contracts.state import EventRecord


class InMemoryEpisodicLog:
    def __init__(self) -> None:
        self._events: list[EventRecord] = []

    async def append(self, event: EventRecord) -> None:
        self._events.append(event)

    async def tail(self, n: int = 50) -> list[EventRecord]:
        return self._events[-n:]

    def __len__(self) -> int:
        return len(self._events)
