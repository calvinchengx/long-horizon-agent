"""The execution-sandbox contract.

All shell/code execution goes through a ``Sandbox`` so the same agent loop runs unchanged on a
local subprocess (dev/CI), a Docker container, or an E2B Firecracker microVM (prod). Sessions
support exec + file IO and can be *snapshotted* for hours/days-long persistence and recovery —
the snapshot id is what the durable workflow stores so a run can resume its workspace after a
crash or after parking on a durable sleep.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from pydantic import BaseModel


class ExecResult(BaseModel):
    """The real result of running a command in a sandbox session."""

    exit_code: int
    stdout: str = ""
    stderr: str = ""
    timed_out: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out


class Snapshot(BaseModel):
    """A handle to a persisted sandbox state (filesystem ± memory, depending on backend)."""

    snapshot_id: str
    kind: str


@runtime_checkable
class SandboxSession(Protocol):
    """A live workspace. ``workdir`` is the root the agent operates in."""

    workdir: str

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        """Run ``argv`` (no shell) and capture its real output + exit code."""
        ...

    async def write_file(self, relpath: str, content: str) -> None:
        """Write ``content`` to ``relpath`` (relative to ``workdir``), creating parents."""
        ...

    async def read_file(self, relpath: str) -> str:
        """Read ``relpath`` (relative to ``workdir``)."""
        ...

    async def close(self) -> None:
        """Release the session (no-op for local; tears down container/VM for others)."""
        ...


@runtime_checkable
class Sandbox(Protocol):
    """Factory for sandbox sessions. Implementations live in ``src/lha/execution/``."""

    name: str

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        """Open (or restore from ``snapshot_id``) a session rooted at ``workdir``."""
        ...

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        """Persist the session's state and return a restorable handle."""
        ...
