"""E2B (Firecracker microVM) sandbox adapter — strongest isolation + pause/resume snapshots.

Behind the same ``Sandbox`` interface as Local/Docker, so the agent loop is unchanged. Pause
returns a snapshot id the durable workflow stores; resume recreates the exact workspace, enabling
hours/days-long persistence and fast recovery. Requires the ``sandbox`` extra (``e2b``) + an E2B
API key. Untested in CI (needs the E2B service); approximate against the e2b async API.
Paths are contained lexically (``lha.execution.paths``); the microVM itself is the isolation
boundary for symlinks and network.
"""

from __future__ import annotations

import shlex

from lha.contracts.sandbox import ExecResult, Sandbox, SandboxSession, Snapshot
from lha.execution.paths import contained_posix
from lha.execution.proc import DEFAULT_MAX_OUTPUT_BYTES, BoundedBuffer, validate_timeout

_WORKDIR = "/home/user/workspace"


def _clip(text: str) -> str:
    buf = BoundedBuffer(DEFAULT_MAX_OUTPUT_BYTES)
    buf.write(text.encode("utf-8", errors="replace"))
    return buf.text()


class E2BSandboxSession(SandboxSession):
    def __init__(self, sandbox: object, workdir: str = _WORKDIR) -> None:
        self._sbx = sandbox
        self.workdir = workdir

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        timeout = validate_timeout(timeout_s)
        command = " ".join(shlex.quote(token) for token in argv)
        workdir = contained_posix(self.workdir, cwd) if cwd else self.workdir
        envs = {"HOME": self.workdir, "LANG": "C.UTF-8", **(env or {})}
        result = await self._sbx.commands.run(  # type: ignore[attr-defined]
            command, cwd=workdir, timeout=timeout, envs=envs
        )
        return ExecResult(
            exit_code=int(getattr(result, "exit_code", 0) or 0),
            stdout=_clip(str(getattr(result, "stdout", "") or "")),
            stderr=_clip(str(getattr(result, "stderr", "") or "")),
        )

    async def write_file(self, relpath: str, content: str) -> None:
        await self._sbx.files.write(contained_posix(self.workdir, relpath), content)  # type: ignore[attr-defined]

    async def read_file(self, relpath: str) -> str:
        data = await self._sbx.files.read(contained_posix(self.workdir, relpath))  # type: ignore[attr-defined]
        return str(data)

    async def close(self) -> None:
        await self._sbx.kill()  # type: ignore[attr-defined]


class E2BSandbox(Sandbox):
    def __init__(self, template: str = "base") -> None:
        self.name = "e2b"
        self._template = template

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        from e2b_code_interpreter import AsyncSandbox

        if snapshot_id:
            sandbox = await AsyncSandbox.resume(snapshot_id)
        else:
            sandbox = await AsyncSandbox.create(self._template)
            await sandbox.commands.run(f"mkdir -p {_WORKDIR}")
        return E2BSandboxSession(sandbox)

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        assert isinstance(session, E2BSandboxSession)
        snapshot_id = await session._sbx.pause()  # type: ignore[attr-defined]
        return Snapshot(snapshot_id=str(snapshot_id), kind="e2b")
