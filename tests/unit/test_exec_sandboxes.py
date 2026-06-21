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
