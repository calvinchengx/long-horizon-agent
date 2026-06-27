"""Docker sandbox — real isolation behind the same ``Sandbox`` interface.

The host workdir is bind-mounted at ``/workspace`` (so the harness's git anchor/verifier see the
agent's edits), and commands run inside a locked-down container:

- ``network_mode="none"`` by default: this is where egress default-deny is actually *enforced* for
  ``run_command`` (``curl``/``pip install`` cannot reach anything). Opt in with ``network=True``
  (full network), or — better — give ``egress_hosts`` (an operator allow-list such as
  ``["pypi.org", "files.pythonhosted.org"]``): each session then gets its own ``--internal``
  network with no route out, plus a proxy container (``egress_proxy.py``) that is the only way
  out and forwards only to the allow-listed hosts. The sandbox gets ``HTTP(S)_PROXY`` pointing at
  it; code that ignores the proxy has no route at all.
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
import contextlib
import io
import os
import posixpath
import secrets
import tarfile
import time
from collections.abc import Callable, Sequence
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
_EXIT_POLL_S = 0.1  # exec_inspect poll interval while the process is still running
DEFAULT_IMAGE = "python:3.12-slim"
DEFAULT_PROXY_IMAGE = "python:3.12-alpine"
PROXY_PORT = 3128
_PROXY_SOURCE = Path(__file__).with_name("egress_proxy.py")
_PROXY_READY = "lha-egress-proxy listening"  # egress_proxy.READY_MESSAGE
_PROXY_READY_TIMEOUT_S = 60.0
_RESOURCE_PREFIX = "lha-egress-"
# Indirection so tests can drive the exec deadline without touching the event loop's clock.
_clock = time.monotonic
_sleep = time.sleep


def _default_user() -> str:
    if hasattr(os, "getuid") and hasattr(os, "getgid"):
        return f"{os.getuid()}:{os.getgid()}"
    return "65534:65534"


def _validate_egress_hosts(hosts: Sequence[str]) -> None:
    """Fail fast (at construction, not inside the proxy container) on a malformed allow-list."""
    from lha.execution.egress_proxy import parse_allow_list

    parse_allow_list(hosts)


class DockerSandboxSession(SandboxSession):
    def __init__(
        self,
        container: Any,
        workdir: str = _WORKDIR,
        *,
        max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES,
        on_close: Callable[[], None] | None = None,
        host_workdir: str | None = None,
    ) -> None:
        self._container = container
        self.workdir = workdir
        # The host directory bind-mounted at ``workdir`` (see ``contracts.sandbox.host_root``).
        self.host_workdir = host_workdir
        self._max_output = max_output_bytes
        self._on_close = on_close  # tears down per-session egress resources (proxy + network)

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
            started = _clock()
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
            # The output stream can end before the process does (it closed stdout/stderr), so
            # wait for the exec to actually finish; an unknown exit code is never "success".
            info = self._container.client.api.exec_inspect(exec_id)
            while info.get("Running") and _clock() - started < timeout + _HOST_GRACE_S:
                _sleep(_EXIT_POLL_S)
                info = self._container.client.api.exec_inspect(exec_id)
            raw_code = info.get("ExitCode")
            if info.get("Running") or raw_code is None:
                return ExecResult(
                    exit_code=-1,
                    stdout=out.text(),
                    stderr=err.text() + "\n[exit status unknown: process did not report one]",
                )
            code = int(raw_code)
            # 124/137 only mean "our deadline hit" if the deadline actually passed; otherwise
            # they are the program's own status (137 is typically an OOM kill in the container).
            deadline_hit = _clock() - started >= timeout
            timed_out = deadline_hit and code in (_TIMEOUT_EXIT, _KILLED_EXIT)
            note = ""
            if timed_out:
                note = f"\n[timed out after {timeout}s]"
            elif code == _KILLED_EXIT:
                note = "\n[killed by SIGKILL (exit 137), e.g. out of memory]"
            stderr = err.text() + note
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
            try:
                self._container.stop(timeout=5)
                self._container.remove(force=True)
            finally:
                if self._on_close is not None:
                    on_close, self._on_close = self._on_close, None
                    on_close()

        await asyncio.to_thread(_stop)


class _EgressGate:
    """The per-session egress resources: an ``--internal`` network and the proxy container.

    ``teardown`` is best-effort and idempotent: it force-removes the proxy container and the
    network, swallowing errors, so a failed ``open`` or ``close`` never leaks them.
    """

    def __init__(self, client: Any, token: str) -> None:
        self._client = client
        self.network_name = f"{_RESOURCE_PREFIX}{token}"
        self.proxy_name = f"{_RESOURCE_PREFIX}proxy-{token}"
        self.network: Any | None = None
        self.proxy: Any | None = None

    @property
    def proxy_url(self) -> str:
        return f"http://{self.proxy_name}:{PROXY_PORT}"

    def proxy_env(self) -> dict[str, str]:
        url = self.proxy_url
        no_proxy = "localhost,127.0.0.1"
        return {
            "HTTP_PROXY": url,
            "HTTPS_PROXY": url,
            "http_proxy": url,
            "https_proxy": url,
            "NO_PROXY": no_proxy,
            "no_proxy": no_proxy,
        }

    def create(self, *, proxy_image: str, egress_hosts: Sequence[str]) -> None:
        self.network = self._client.networks.create(
            self.network_name, driver="bridge", internal=True, labels={"lha.egress": "network"}
        )
        self.proxy = self._client.containers.run(
            proxy_image,
            command=["python", "-c", _PROXY_SOURCE.read_text(encoding="utf-8")],
            name=self.proxy_name,
            detach=True,
            # Default bridge = the way out; the internal network is joined below.
            network="bridge",
            environment={
                "LHA_PROXY_ALLOW": ",".join(egress_hosts),
                "LHA_PROXY_PORT": str(PROXY_PORT),
                "PYTHONDONTWRITEBYTECODE": "1",
                "PYTHONUNBUFFERED": "1",
            },
            labels={"lha.egress": "proxy"},
            user="65534:65534",
            cap_drop=["ALL"],
            security_opt=["no-new-privileges:true"],
            read_only=True,
            tmpfs={"/tmp": "rw,nosuid,nodev,size=16m"},
            mem_limit="256m",
            pids_limit=128,
        )
        self.network.connect(self.proxy)
        self._wait_ready()

    def _wait_ready(self) -> None:
        assert self.proxy is not None
        deadline = _clock() + _PROXY_READY_TIMEOUT_S
        while True:
            logs = bytes(self.proxy.logs()).decode("utf-8", errors="replace")
            if _PROXY_READY in logs:
                return
            self.proxy.reload()
            if self.proxy.status in ("exited", "dead"):
                raise RuntimeError(f"egress proxy exited during startup:\n{logs[-2000:]}")
            if _clock() >= deadline:
                raise RuntimeError(f"egress proxy not ready after {_PROXY_READY_TIMEOUT_S}s")
            _sleep(_EXIT_POLL_S)

    def teardown(self) -> None:
        proxy, self.proxy = self.proxy, None
        network, self.network = self.network, None
        if proxy is None:  # creation may have failed after the container existed: look it up
            with contextlib.suppress(Exception):
                proxy = self._client.containers.get(self.proxy_name)
        if proxy is not None:
            with contextlib.suppress(Exception):
                proxy.remove(force=True)
        if network is None:
            with contextlib.suppress(Exception):
                network = self._client.networks.get(self.network_name)
        if network is not None:
            with contextlib.suppress(Exception):
                network.remove()


class DockerSandbox(Sandbox):
    """Opens hardened containers.

    ``network=False`` and no ``egress_hosts`` (the default) means no network at all.
    ``egress_hosts`` routes egress through a per-session allow-list proxy (see the module doc);
    ``network=True`` is the unrestricted default bridge. The two are mutually exclusive.
    """

    def __init__(
        self,
        image: str = DEFAULT_IMAGE,
        *,
        client: Any | None = None,
        network: bool = False,
        egress_hosts: Sequence[str] = (),
        proxy_image: str = DEFAULT_PROXY_IMAGE,
        mem_limit: str = "2g",
        pids_limit: int = 512,
        cpus: float = 2.0,
        tmp_size: str = "1g",
        user: str | None = None,
        read_only_root: bool = True,
        max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES,
    ) -> None:
        self.name = "docker"
        hosts = tuple(h.strip() for h in egress_hosts if h.strip())
        if network and hosts:
            raise ValueError(
                "network=True (unrestricted network) and egress_hosts (allow-list proxy) are "
                "mutually exclusive"
            )
        if hosts:
            _validate_egress_hosts(hosts)
        self._image = image
        self._egress_hosts = hosts
        self._proxy_image = proxy_image
        if client is None:
            import docker

            client = docker.from_env()
        self._client = client
        self._network = network
        self._mem_limit = mem_limit
        self._pids_limit = pids_limit
        self._nano_cpus = int(cpus * 1_000_000_000)
        self._tmp_size = tmp_size
        self._user = user or _default_user()
        self._read_only_root = read_only_root
        self._max_output = max_output_bytes

    @property
    def image(self) -> str:
        return self._image

    @property
    def egress_hosts(self) -> tuple[str, ...]:
        return self._egress_hosts

    def run_kwargs(
        self,
        workdir: str,
        *,
        egress_network: str | None = None,
        proxy_env: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        """The ``containers.run`` hardening options (exposed for inspection/tests).

        With ``egress_network`` the container joins ONLY that (internal) network instead of
        ``network_mode="none"``/``"bridge"``, and ``proxy_env`` is added to its environment.
        """
        host = Path(workdir).resolve()
        volumes: dict[str, dict[str, str]] = {str(host): {"bind": _WORKDIR, "mode": "rw"}}
        for name in sorted(PROTECTED_DIRS):
            if (host / name).is_dir():
                volumes[str(host / name)] = {"bind": f"{_WORKDIR}/{name}", "mode": "ro"}
        networking: dict[str, Any]
        if egress_network is not None:
            networking = {"network": egress_network}
        else:
            networking = {"network_mode": "bridge" if self._network else "none"}
        return {
            "command": ["sleep", "infinity"],
            "detach": True,
            "tty": False,
            "working_dir": _WORKDIR,
            "user": self._user,
            "environment": {"HOME": _WORKDIR, "LANG": "C.UTF-8", **(proxy_env or {})},
            "volumes": volumes,
            **networking,
            "mem_limit": self._mem_limit,
            "memswap_limit": self._mem_limit,
            "pids_limit": self._pids_limit,
            "nano_cpus": self._nano_cpus,
            "cap_drop": ["ALL"],
            "security_opt": ["no-new-privileges:true"],
            "read_only": self._read_only_root,
            # ``exec``: Docker mounts tmpfs noexec by default, which breaks every toolchain that
            # builds then runs a binary there (``go test``). Code already runs from /workspace,
            # so this adds nothing an agent could not do anyway. Go/uv/pnpm caches live here too.
            # tmpfs pages count against ``mem_limit``: size the two together.
            "tmpfs": {"/tmp": f"rw,exec,nosuid,nodev,size={self._tmp_size}"},
        }

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        image = snapshot_id or self._image
        await asyncio.to_thread(lambda: Path(workdir).mkdir(parents=True, exist_ok=True))
        host = str(Path(workdir).resolve())
        if not self._egress_hosts:
            kwargs = self.run_kwargs(workdir)

            def _start() -> Any:
                return self._client.containers.run(image, **kwargs)

            container = await asyncio.to_thread(_start)
            return DockerSandboxSession(
                container, max_output_bytes=self._max_output, host_workdir=host
            )

        gate = _EgressGate(self._client, secrets.token_hex(6))

        def _start_gated() -> Any:
            try:
                gate.create(proxy_image=self._proxy_image, egress_hosts=self._egress_hosts)
                kwargs = self.run_kwargs(
                    workdir, egress_network=gate.network_name, proxy_env=gate.proxy_env()
                )
                return self._client.containers.run(image, **kwargs)
            except BaseException:
                gate.teardown()
                raise

        container = await asyncio.to_thread(_start_gated)
        return DockerSandboxSession(
            container, max_output_bytes=self._max_output, on_close=gate.teardown, host_workdir=host
        )

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        assert isinstance(session, DockerSandboxSession)

        def _commit() -> str:
            image = session._container.commit()
            return str(image.id)

        snapshot_id = await asyncio.to_thread(_commit)
        return Snapshot(snapshot_id=snapshot_id, kind="docker")
