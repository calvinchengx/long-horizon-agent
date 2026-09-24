"""Exclusive per-checkout locks (``flock`` on a file under ``.git/``).

Everything that changes a mission checkout's index or ``HEAD`` takes the same lock
(``CYCLE_LOCK``): a durable cycle attempt, a wave's integration, a durable review and a lease
decision. Two such writers (e.g. a zombie activity attempt and its retry, or two implementers
asking for leases at once) can then never interleave git operations on one checkout. Lock files
live in the repository's ``.git`` directory, so they are never tracked and never cleaned.
"""

from __future__ import annotations

import asyncio
import contextlib
import fcntl
import os
from collections.abc import AsyncIterator, Callable
from pathlib import Path

from lha.state import git_ops

CYCLE_LOCK = "lha-cycle.lock"
LOCK_WAIT_S = 300.0
_POLL_S = 0.5


class WorkdirBusyError(RuntimeError):
    """Another holder kept the checkout's lock for longer than the wait allowed."""


@contextlib.asynccontextmanager
async def workdir_flock(
    workdir: str | Path,
    *,
    name: str = CYCLE_LOCK,
    wait_s: float = LOCK_WAIT_S,
    on_wait: Callable[[], None] | None = None,
) -> AsyncIterator[None]:
    """Hold the exclusive lock ``.git/<name>`` of ``workdir``'s repository.

    Polls without blocking the event loop; ``on_wait`` runs on every poll (e.g. an activity
    heartbeat). Raises ``WorkdirBusyError`` after ``wait_s`` seconds.
    """
    path = await asyncio.to_thread(git_ops.git_dir, workdir) / name
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o644)
    try:
        waited = 0.0
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if waited >= wait_s:
                    raise WorkdirBusyError(
                        f"workdir {workdir} is locked by another writer ({name})"
                    ) from None
                if on_wait is not None:
                    on_wait()
                await asyncio.sleep(_POLL_S)
                waited += _POLL_S
        try:
            yield
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        os.close(fd)
