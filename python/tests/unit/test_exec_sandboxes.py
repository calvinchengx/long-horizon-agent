"""S1/S4/S6: sandbox factory + Docker hardening (with a fake docker client; no daemon needed)."""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from lha.execution import sandbox_docker
from lha.execution.factory import UnsafeSandboxError, build_sandbox, open_sandbox
from lha.execution.paths import PathEscapeError
from lha.execution.sandbox_docker import DockerSandbox, DockerSandboxSession
from lha.execution.sandbox_local import LocalSandbox


def test_factory_refuses_local_without_opt_in() -> None:
    with pytest.raises(UnsafeSandboxError):
        build_sandbox("local")
    assert isinstance(build_sandbox("local", allow_unsafe_local=True), LocalSandbox)
    with pytest.raises(ValueError):
        build_sandbox("chroot")


@pytest.mark.asyncio
async def test_open_sandbox_local_opt_in(tmp_path: Path) -> None:
    with pytest.raises(UnsafeSandboxError):
        await open_sandbox("local", workdir=str(tmp_path))
    session = await open_sandbox("local", workdir=str(tmp_path), allow_unsafe_local=True)
    assert session.workdir == str(tmp_path)


@dataclass
class _ExecRun:
    exit_code: int
    output: bytes


@dataclass
class _FakeAPI:
    chunks: list[tuple[bytes | None, bytes | None]] = field(default_factory=list)
    exit_code: int = 0
    created: list[dict[str, Any]] = field(default_factory=list)

    def exec_create(self, container_id: str, **kwargs: Any) -> dict[str, str]:
        self.created.append(kwargs)
        return {"Id": "exec1"}

    def exec_start(self, exec_id: str, *, stream: bool, demux: bool) -> Iterator[Any]:
        assert stream and demux
        yield from self.chunks

    def exec_inspect(self, exec_id: str) -> dict[str, int]:
        return {"ExitCode": self.exit_code}


@dataclass
class _FakeContainer:
    api: _FakeAPI = field(default_factory=_FakeAPI)
    realpath: str | None = None
    id: str = "c1"
    runs: list[list[str]] = field(default_factory=list)
    archives: list[str] = field(default_factory=list)

    @property
    def client(self) -> Any:
        return self

    def exec_run(self, cmd: list[str], **_: Any) -> _ExecRun:
        self.runs.append(cmd)
        if cmd[0] == "realpath":
            return _ExecRun(0, (self.realpath or cmd[-1]).encode() + b"\n")
        return _ExecRun(0, b"content")

    def put_archive(self, path: str, data: bytes) -> None:
        self.archives.append(path)


@dataclass
class _FakeContainers:
    started: list[tuple[str, dict[str, Any]]] = field(default_factory=list)

    def run(self, image: str, **kwargs: Any) -> _FakeContainer:
        self.started.append((image, kwargs))
        return _FakeContainer()


@dataclass
class _FakeClient:
    containers: _FakeContainers = field(default_factory=_FakeContainers)


@pytest.mark.asyncio
async def test_docker_container_is_hardened(tmp_path: Path) -> None:
    (tmp_path / ".git").mkdir()
    (tmp_path / ".lha").mkdir()
    client = _FakeClient()
    sandbox = DockerSandbox(client=client)
    await sandbox.open(workdir=str(tmp_path))

    _, kwargs = client.containers.started[0]
    assert kwargs["network_mode"] == "none"
    assert kwargs["cap_drop"] == ["ALL"]
    assert "no-new-privileges:true" in kwargs["security_opt"]
    assert kwargs["read_only"] is True
    assert kwargs["mem_limit"] and kwargs["pids_limit"] and kwargs["nano_cpus"]
    assert kwargs["user"] and not kwargs["user"].startswith("0:")
    assert "/tmp" in kwargs["tmpfs"]
    volumes = kwargs["volumes"]
    assert volumes[str(tmp_path.resolve())] == {"bind": "/workspace", "mode": "rw"}
    assert volumes[str(tmp_path.resolve() / ".git")]["mode"] == "ro"
    assert volumes[str(tmp_path.resolve() / ".lha")]["mode"] == "ro"

    networked = DockerSandbox(client=_FakeClient(), network=True)
    assert networked.run_kwargs(str(tmp_path))["network_mode"] == "bridge"


@pytest.mark.asyncio
async def test_docker_exec_wraps_timeout_env_and_bounds_output() -> None:
    container = _FakeContainer()
    container.api.chunks = [(b"a" * 5000, None), (None, b"err"), (b"END", None)]
    session = DockerSandboxSession(container, max_output_bytes=1000)
    result = await session.exec(["pytest", "-q"], timeout_s=30, env={"FOO": "1"})

    created = container.api.created[0]
    assert created["cmd"][:4] == ["timeout", "-k", "5", "30s"]
    assert created["cmd"][4:] == ["pytest", "-q"]
    assert created["environment"]["HOME"] == "/workspace"
    assert created["environment"]["FOO"] == "1"
    assert result.ok
    assert len(result.stdout) < 1200 and result.stdout.endswith("END")
    assert result.stderr == "err"


@pytest.mark.asyncio
async def test_docker_exec_reports_timeout_and_validates(monkeypatch: pytest.MonkeyPatch) -> None:
    ticks = iter([0.0, 5.0, 5.0, 5.0])  # the deadline (1s) has passed when the exit is read
    monkeypatch.setattr(sandbox_docker, "_clock", lambda: next(ticks))
    container = _FakeContainer()
    container.api.exit_code = 124
    session = DockerSandboxSession(container)
    result = await session.exec(["sleep", "999"], timeout_s=1)
    assert result.timed_out and not result.ok
    with pytest.raises(ValueError):
        await session.exec(["true"], timeout_s=True)
    with pytest.raises(PathEscapeError):
        await session.exec(["true"], cwd="/etc")


@pytest.mark.asyncio
async def test_docker_file_io_is_contained() -> None:
    container = _FakeContainer()
    session = DockerSandboxSession(container)
    with pytest.raises(PathEscapeError):
        await session.read_file("../etc/passwd")
    with pytest.raises(PathEscapeError):
        await session.write_file("/etc/cron.d/x", "boom")

    await session.write_file("src/a.py", "x = 1")
    assert container.archives == ["/workspace/src"]

    container.realpath = "/etc/shadow"  # a symlink inside the workspace pointing out
    with pytest.raises(PathEscapeError):
        await session.read_file("innocent_link")


# --- egress allow-list (proxy + internal network), with a fake docker client ----------------


@dataclass
class _FakeNetwork:
    name: str
    kwargs: dict[str, Any]
    connected: list[str] = field(default_factory=list)
    removed: bool = False

    def connect(self, container: Any) -> None:
        self.connected.append(container.name)

    def remove(self) -> None:
        self.removed = True


@dataclass
class _FakeProxy:
    name: str
    log: bytes = b"INFO lha-egress-proxy listening on 0.0.0.0:3128\n"
    status: str = "running"
    removed: bool = False

    def logs(self) -> bytes:
        return self.log

    def reload(self) -> None:
        pass

    def remove(self, *, force: bool) -> None:
        self.removed = True


@dataclass
class _FakeNetworks:
    created: list[_FakeNetwork] = field(default_factory=list)

    def create(self, name: str, **kwargs: Any) -> _FakeNetwork:
        network = _FakeNetwork(name, kwargs)
        self.created.append(network)
        return network

    def get(self, name: str) -> _FakeNetwork:
        return next(n for n in self.created if n.name == name)


@dataclass
class _EgressContainers:
    started: list[tuple[str, dict[str, Any]]] = field(default_factory=list)
    proxies: list[_FakeProxy] = field(default_factory=list)
    sandboxes: list[_FakeContainer] = field(default_factory=list)
    proxy_log: bytes = _FakeProxy("").log
    proxy_status: str = "running"
    fail_sandbox: bool = False

    def run(self, image: str, **kwargs: Any) -> Any:
        self.started.append((image, kwargs))
        if "name" in kwargs:  # the proxy container
            proxy = _FakeProxy(kwargs["name"], log=self.proxy_log, status=self.proxy_status)
            self.proxies.append(proxy)
            return proxy
        if self.fail_sandbox:
            raise RuntimeError("image not found")
        container = _StoppableContainer()
        self.sandboxes.append(container)
        return container

    def get(self, name: str) -> _FakeProxy:
        return next(p for p in self.proxies if p.name == name)


@dataclass
class _StoppableContainer(_FakeContainer):
    removed: bool = False

    def stop(self, *, timeout: int) -> None:
        pass

    def remove(self, *, force: bool) -> None:
        self.removed = True


@dataclass
class _EgressClient:
    containers: _EgressContainers = field(default_factory=_EgressContainers)
    networks: _FakeNetworks = field(default_factory=_FakeNetworks)


@pytest.mark.asyncio
async def test_egress_hosts_route_the_sandbox_through_an_allow_list_proxy(tmp_path: Path) -> None:
    client = _EgressClient()
    sandbox = DockerSandbox("my/sandbox:1", client=client, egress_hosts=["pypi.org", ".golang.org"])
    session = await sandbox.open(workdir=str(tmp_path))

    [network] = client.networks.created
    assert network.name.startswith("lha-egress-") and network.kwargs["internal"] is True
    (proxy_image, proxy_kwargs), (image, kwargs) = client.containers.started
    [proxy] = client.containers.proxies

    # The proxy: the stdlib-only proxy source, the allow-list, on the bridge AND the internal net.
    assert proxy_image == "python:3.12-alpine"
    assert proxy_kwargs["command"][:2] == ["python", "-c"]
    assert "class EgressProxy" in proxy_kwargs["command"][2]
    assert proxy_kwargs["environment"]["LHA_PROXY_ALLOW"] == "pypi.org,.golang.org"
    assert proxy_kwargs["network"] == "bridge" and network.connected == [proxy.name]
    assert proxy_kwargs["cap_drop"] == ["ALL"] and proxy_kwargs["read_only"] is True

    # The sandbox: the configured image, ONLY the internal network, proxy env, still hardened.
    assert image == "my/sandbox:1"
    assert kwargs["network"] == network.name and "network_mode" not in kwargs
    env = kwargs["environment"]
    proxy_url = f"http://{proxy.name}:3128"
    for var in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"):
        assert env[var] == proxy_url
    assert env["NO_PROXY"] == "localhost,127.0.0.1" and "GOPROXY" not in env
    assert kwargs["cap_drop"] == ["ALL"] and kwargs["read_only"] is True

    await session.close()
    assert client.containers.sandboxes[0].removed and proxy.removed and network.removed


@pytest.mark.asyncio
async def test_empty_egress_list_keeps_network_none(tmp_path: Path) -> None:
    client = _EgressClient()
    await DockerSandbox(client=client, egress_hosts=["", " "]).open(workdir=str(tmp_path))
    assert client.networks.created == []
    [(_, kwargs)] = client.containers.started
    assert kwargs["network_mode"] == "none" and "network" not in kwargs
    assert not any("PROXY" in key.upper() for key in kwargs["environment"])


def test_egress_hosts_are_validated_and_exclusive_with_full_network() -> None:
    with pytest.raises(ValueError, match="mutually exclusive"):
        DockerSandbox(client=_EgressClient(), network=True, egress_hosts=["pypi.org"])
    with pytest.raises(ValueError, match="invalid egress allow-list entry"):
        DockerSandbox(client=_EgressClient(), egress_hosts=["10.0.0.1"])


@pytest.mark.asyncio
async def test_failed_open_tears_down_the_proxy_and_network(tmp_path: Path) -> None:
    client = _EgressClient()
    client.containers.fail_sandbox = True
    with pytest.raises(RuntimeError, match="image not found"):
        await DockerSandbox(client=client, egress_hosts=["pypi.org"]).open(workdir=str(tmp_path))
    assert client.containers.proxies[0].removed and client.networks.created[0].removed


@pytest.mark.asyncio
async def test_proxy_that_dies_at_startup_fails_open_and_cleans_up(tmp_path: Path) -> None:
    client = _EgressClient()
    client.containers.proxy_log = b"SyntaxError: boom"
    client.containers.proxy_status = "exited"
    with pytest.raises(RuntimeError, match="egress proxy exited during startup"):
        await DockerSandbox(client=client, egress_hosts=["pypi.org"]).open(workdir=str(tmp_path))
    assert client.containers.proxies[0].removed and client.networks.created[0].removed
    assert client.containers.sandboxes == []


def test_factory_passes_image_and_egress_hosts(monkeypatch: pytest.MonkeyPatch) -> None:
    built: list[tuple[str, dict[str, Any]]] = []

    class _Recorder:
        def __init__(self, image: str, **kwargs: Any) -> None:
            built.append((image, kwargs))

    monkeypatch.setattr(sandbox_docker, "DockerSandbox", _Recorder)
    build_sandbox("docker")
    build_sandbox("docker", image="custom:1", egress_hosts=["pypi.org"])
    assert built[0] == (
        "ghcr.io/astral-sh/uv:python3.12-bookworm-slim",
        {"network": False, "egress_hosts": ()},
    )
    assert built[1] == ("custom:1", {"network": False, "egress_hosts": ("pypi.org",)})
