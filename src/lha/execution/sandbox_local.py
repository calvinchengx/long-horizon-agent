"""``LocalSandbox`` — runs commands directly on the host in a working directory.

For development, CI, and the offline/$0 path. NOT isolated (no container/VM): commands run as the
host user with host network access, so it must only run trusted code. Production entry points
must go through ``lha.execution.factory.build_sandbox`` which refuses ``local`` unless the operator
explicitly opts in (``allow_unsafe_local``); the Docker and E2B sandboxes provide real isolation
behind the same ``Sandbox`` interface.

What it DOES enforce: file IO is confined to the workdir (``lha.execution.paths``: no absolute
paths, ``..`` or symlink escapes) and children get a minimal environment with no host secrets
(``lha.execution.proc``). What it does NOT enforce: network egress (a command can reach the
internet) and filesystem access by arbitrary commands — use the Docker sandbox for that.

Snapshots here are cheap: the current git HEAD of the workdir, which is the restore point (the
workdir is a git repo — the mission anchor lives in it).
"""

from __future__ import annotations

import asyncio
from pathlib import Path

from lha.contracts.sandbox import ExecResult, Sandbox, SandboxSession, Snapshot
from lha.execution.paths import (
    PathEscapeError,
    read_text_within,
    resolve_within,
    write_text_within,
)
from lha.execution.proc import run_proc
from lha.state import git_ops


class LocalSandboxSession(SandboxSession):
    """A host-local working directory."""

    def __init__(self, workdir: str) -> None:
        self.workdir = workdir

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        return await run_proc(
            argv, cwd=self._contained_cwd(cwd), timeout_s=timeout_s, env=env, home=self.workdir
        )

    def _contained_cwd(self, cwd: str | None) -> str:
        """``cwd`` may be relative to the workdir, or absolute but inside it; never outside."""
        if not cwd:
            return self.workdir
        root = Path(self.workdir).resolve()
        if Path(cwd).is_absolute():
            resolved = Path(cwd).resolve()
            if not resolved.is_relative_to(root):
                raise PathEscapeError(f"cwd escapes the workspace: {cwd!r}")
            return str(resolved)
        return str(resolve_within(root, cwd))

    async def write_file(self, relpath: str, content: str) -> None:
        await asyncio.to_thread(write_text_within, self.workdir, relpath, content)

    async def read_file(self, relpath: str) -> str:
        return await asyncio.to_thread(read_text_within, self.workdir, relpath)

    async def close(self) -> None:
        return None


class LocalSandbox(Sandbox):
    """Opens host-local sessions."""

    def __init__(self) -> None:
        self.name = "local"

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        await asyncio.to_thread(lambda: Path(workdir).mkdir(parents=True, exist_ok=True))
        # snapshot_id (a git sha) is honoured by the caller via git checkout; nothing to mount here.
        return LocalSandboxSession(workdir)

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        sha = await asyncio.to_thread(git_ops.head_sha, session.workdir)
        return Snapshot(snapshot_id=sha or "no-commit", kind="local")
