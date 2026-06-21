"""Docker sandbox — real isolation behind the same ``Sandbox`` interface.

The host workdir is bind-mounted at ``/workspace`` (so the harness's git anchor/verifier see the
agent's edits), and commands run inside a locked-down container:

- ``network_mode="none"`` by default: this is where egress default-deny is actually *enforced* for
  ``run_command`` (``curl``/``pip install`` cannot reach anything). Opt in with ``network=True``.
- resource limits (``mem_limit``, ``pids_limit``, ``nano_cpus``), ``cap_drop=["ALL"]``,
  ``no-new-privileges``, a non-root user (the host uid:gid on POSIX so files stay owned by the
  operator, else ``nobody``), and a read-only root filesystem with tmpfs ``/tmp``;
- the harness-owned ``.git`` and ``.lha`` dirs are re-mounted read-only, so code in the container
  cannot plant git hooks or rewrite the mission state;
- every exec is wrapped in coreutils ``timeout`` (plus a host-side deadline) and its output is
  streamed into a bounded buffer;
- file IO paths are validated lexically and re-checked with ``realpath`` inside the container.

Snapshots commit the container to an image, whose id is stored in the durable workflow. Requires
the ``sandbox`` extra (``docker``); the client can be injected (unit tests use a fake).
"""

from __future__ import annotations

import asyncio
import io
import os
import posixpath
import tarfile
from pathlib import Path, PurePosixPath
from typing import Any

from lha.contracts.sandbox import ExecResult, Sandbox, SandboxSession, Snapshot
from lha.execution.paths import PROTECTED_DIRS, PathEscapeError, contained_posix
from lha.execution.proc import DEFAULT_MAX_OUTPUT_BYTES, BoundedBuffer, validate_timeout

_WORKDIR = "/workspace"
_CONTAINER_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
_TIMEOUT_EXIT = 124  # coreutils `timeout` exit status when the deadline hit
_KILLED_EXIT = 137  # 128 + SIGKILL (from `timeout -k`)
_HOST_GRACE_S = 30


def _default_user() -> str:
    if hasattr(os, "getuid") and hasattr(os, "getgid"):
        return f"{os.getuid()}:{os.getgid()}"
    return "65534:65534"


class DockerSandboxSession(SandboxSession):
    def __init__(
        self,
        container: Any,
        workdir: str = _WORKDIR,
        *,
        max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES,
    ) -> None:
        self._container = container
        self.workdir = workdir
        self._max_output = max_output_bytes

    def _container_path(self, relpath: str) -> str:
        """Lexically contain ``relpath``, then confirm with ``realpath`` inside the container."""
        target = contained_posix(self.workdir, relpath)
        result = self._container.exec_run(cmd=["realpath", "-m", "--", target])
        if result.exit_code == 0:
            real = bytes(result.output).decode("utf-8", errors="replace").strip()
            if real and real != self.workdir and not real.startswith(self.workdir + "/"):
                raise PathEscapeError(f"path escapes the workspace: {relpath!r}")
        return target

    def _env(self, env: dict[str, str] | None) -> dict[str, str]:
        base = {
            "PATH": _CONTAINER_PATH,
            "HOME": self.workdir,
            "LANG": "C.UTF-8",
            "LC_ALL": "C.UTF-8",
            "TMPDIR": "/tmp",
            "GIT_TERMINAL_PROMPT": "0",
        }
        return {**base, **(env or {})}

    async def exec(
        self,
        argv: list[str],
        *,
        timeout_s: int = 600,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> ExecResult:
        timeout = validate_timeout(timeout_s)
        workdir = self.workdir
        if cwd:
            rel = cwd
            if PurePosixPath(cwd).is_absolute():
                rel = posixpath.relpath(posixpath.normpath(cwd), self.workdir)
            workdir = contained_posix(self.workdir, rel)
        wrapped = ["timeout", "-k", "5", f"{timeout}s", *argv]

        def _run() -> ExecResult:
            out, err = BoundedBuffer(self._max_output), BoundedBuffer(self._max_output)
            exec_id = self._container.client.api.exec_create(
                self._container.id,
                cmd=wrapped,
                workdir=workdir,
                environment=self._env(env),
                stdout=True,
                stderr=True,
            )["Id"]
            for stdout_b, stderr_b in self._container.client.api.exec_start(
                exec_id, stream=True, demux=True
            ):
                if stdout_b:
                    out.write(stdout_b)
                if stderr_b:
                    err.write(stderr_b)
            info = self._container.client.api.exec_inspect(exec_id)
            code = int(info.get("ExitCode") or 0)
            timed_out = code in (_TIMEOUT_EXIT, _KILLED_EXIT)
            stderr = err.text() + (f"\n[timed out after {timeout}s]" if timed_out else "")
            return ExecResult(
                exit_code=-1 if timed_out else code,
                stdout=out.text(),
                stderr=stderr,
                timed_out=timed_out,
            )

        try:
            return await asyncio.wait_for(asyncio.to_thread(_run), timeout=timeout + _HOST_GRACE_S)
        except TimeoutError:
            return ExecResult(
                exit_code=-1, stderr=f"[timed out after {timeout}s (host deadline)]", timed_out=True
            )

    async def write_file(self, relpath: str, content: str) -> None:
        def _write() -> None:
            target = PurePosixPath(self._container_path(relpath))
            data = content.encode("utf-8")
            info = tarfile.TarInfo(name=target.name)
            info.size = len(data)
            info.mode = 0o644
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode="w") as tar:
                tar.addfile(info, io.BytesIO(data))
            self._container.exec_run(cmd=["mkdir", "-p", "--", str(target.parent)])
            self._container.put_archive(str(target.parent), stream.getvalue())

        await asyncio.to_thread(_write)

    async def read_file(self, relpath: str) -> str:
        def _read() -> str:
            target = self._container_path(relpath)
            result = self._container.exec_run(cmd=["cat", "--", target])
            if result.exit_code != 0:
                raise OSError(f"cannot read {relpath!r} in container")
            return bytes(result.output).decode("utf-8", errors="replace")

        return await asyncio.to_thread(_read)

    async def close(self) -> None:
        def _stop() -> None:
            self._container.stop(timeout=5)
            self._container.remove(force=True)

        await asyncio.to_thread(_stop)


class DockerSandbox(Sandbox):
    """Opens hardened containers. ``network=False`` (default) means no network at all."""

    def __init__(
        self,
        image: str = "python:3.12-slim",
        *,
        client: Any | None = None,
        network: bool = False,
        mem_limit: str = "2g",
        pids_limit: int = 512,
        cpus: float = 2.0,
        user: str | None = None,
        read_only_root: bool = True,
        max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES,
    ) -> None:
        self.name = "docker"
        self._image = image
        if client is None:
            import docker

            client = docker.from_env()
        self._client = client
        self._network = network
        self._mem_limit = mem_limit
        self._pids_limit = pids_limit
        self._nano_cpus = int(cpus * 1_000_000_000)
        self._user = user or _default_user()
        self._read_only_root = read_only_root
        self._max_output = max_output_bytes

    def run_kwargs(self, workdir: str) -> dict[str, Any]:
        """The ``containers.run`` hardening options (exposed for inspection/tests)."""
        host = Path(workdir).resolve()
        volumes: dict[str, dict[str, str]] = {str(host): {"bind": _WORKDIR, "mode": "rw"}}
        for name in sorted(PROTECTED_DIRS):
            if (host / name).is_dir():
                volumes[str(host / name)] = {"bind": f"{_WORKDIR}/{name}", "mode": "ro"}
        return {
            "command": ["sleep", "infinity"],
            "detach": True,
            "tty": False,
            "working_dir": _WORKDIR,
            "user": self._user,
            "environment": {"HOME": _WORKDIR, "LANG": "C.UTF-8"},
            "volumes": volumes,
            "network_mode": "bridge" if self._network else "none",
            "mem_limit": self._mem_limit,
            "memswap_limit": self._mem_limit,
            "pids_limit": self._pids_limit,
            "nano_cpus": self._nano_cpus,
            "cap_drop": ["ALL"],
            "security_opt": ["no-new-privileges:true"],
            "read_only": self._read_only_root,
            "tmpfs": {"/tmp": "rw,nosuid,nodev,size=512m"},
        }

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        image = snapshot_id or self._image
        await asyncio.to_thread(lambda: Path(workdir).mkdir(parents=True, exist_ok=True))
        kwargs = self.run_kwargs(workdir)

        def _start() -> Any:
            return self._client.containers.run(image, **kwargs)

        container = await asyncio.to_thread(_start)
        return DockerSandboxSession(container, max_output_bytes=self._max_output)

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        assert isinstance(session, DockerSandboxSession)

        def _commit() -> str:
            image = session._container.commit()
            return str(image.id)

        snapshot_id = await asyncio.to_thread(_commit)
        return Snapshot(snapshot_id=snapshot_id, kind="docker")
