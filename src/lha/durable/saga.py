"""A saga / compensation stack for multi-step side effects.

Every irreversible-ish forward step (git push, branch create, PR open, deploy) registers an
idempotent compensation BEFORE it runs. On failure the saga runs the compensations in LIFO order,
so partial work is rolled back cleanly. Truly irreversible actions are not pretend-compensated —
they route to a human gate instead (see ``lha.hitl``).
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass


@dataclass
class Compensation:
    name: str
    undo: Callable[[], Awaitable[None]]


class Saga:
    """Accumulates compensations and runs them LIFO on ``compensate``."""

    def __init__(self) -> None:
        self._compensations: list[Compensation] = []
        self.failures: list[str] = []

    def add(self, name: str, undo: Callable[[], Awaitable[None]]) -> None:
        self._compensations.append(Compensation(name=name, undo=undo))

    async def compensate(self) -> list[str]:
        """Run compensations newest-first; return the names that ran. Records any undo failures.

        Each compensation is popped off the stack before it runs, so it is attempted at most once:
        a second ``compensate()`` (e.g. a retry after a partial rollback) is a no-op rather than a
        double undo.
        """
        ran: list[str] = []
        while self._compensations:
            comp = self._compensations.pop()
            try:
                await comp.undo()
                ran.append(comp.name)
            except Exception as exc:
                self.failures.append(f"{comp.name}: {type(exc).__name__}: {exc}")
        return ran

    def __len__(self) -> int:
        return len(self._compensations)
