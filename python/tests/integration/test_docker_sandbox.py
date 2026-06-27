"""The Docker sandbox against a real Docker daemon: exit codes, timeouts, isolation."""

from __future__ import annotations

import subprocess
from collections.abc import AsyncIterator
from pathlib import Path

import pytest

from lha.contracts.sandbox import SandboxSession
from tests.integration.conftest import requires_docker

pytestmark = [pytest.mark.integration, requires_docker]
pytest.importorskip("docker")


async def _open(workdir: Path, **kwargs: object) -> SandboxSession:
    from lha.execution.sandbox_docker import DockerSandbox

    return await DockerSandbox(**kwargs).open(workdir=str(workdir))  # type: ignore[arg-type]


@pytest.fixture
async def session(tmp_path: Path) -> AsyncIterator[SandboxSession]:
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    sess = await _open(tmp_path)
    try:
        yield sess
    finally:
        await sess.close()


async def test_success_and_real_exit_codes(session: SandboxSession) -> None:
    ok = await session.exec(["sh", "-c", "echo hello"])
    assert ok.ok and ok.stdout.strip() == "hello"
    failed = await session.exec(["sh", "-c", "echo oops >&2; exit 3"])
    assert failed.exit_code == 3 and not failed.timed_out and "oops" in failed.stderr


async def test_program_exiting_124_quickly_is_not_a_timeout(session: SandboxSession) -> None:
    result = await session.exec(["sh", "-c", "exit 124"], timeout_s=60)
    assert result.exit_code == 124 and not result.timed_out


async def test_deadline_is_reported_as_timeout(session: SandboxSession) -> None:
    result = await session.exec(["sleep", "30"], timeout_s=2)
    assert result.timed_out and result.exit_code == -1


async def test_exit_code_waits_for_process_after_streams_close(session: SandboxSession) -> None:
    # The output stream ends immediately, but the process keeps running before it exits 5.
    result = await session.exec(["sh", "-c", "exec >&- 2>&-; sleep 2; exit 5"], timeout_s=30)
    assert result.exit_code == 5 and not result.timed_out


async def test_out_of_memory_kill_is_not_a_timeout(tmp_path: Path) -> None:
    sess = await _open(tmp_path, mem_limit="64m")
    try:
        result = await sess.exec(
            ["python", "-c", "b = bytearray(512 * 1024 * 1024); print(len(b))"], timeout_s=60
        )
    finally:
        await sess.close()
    assert result.exit_code == 137 and not result.timed_out
    assert "SIGKILL" in result.stderr


async def test_harness_dirs_are_read_only_inside_the_container(session: SandboxSession) -> None:
    result = await session.exec(["sh", "-c", "echo x > .git/hooks/pre-commit"])
    assert not result.ok
    assert (await session.exec(["sh", "-c", "echo x > scratch.txt"])).ok


async def test_no_network_by_default(session: SandboxSession) -> None:
    probe = (
        "import socket\n"
        "try:\n"
        "    socket.create_connection(('1.1.1.1', 53), timeout=3)\n"
        "except OSError:\n"
        "    raise SystemExit(7)\n"
    )
    result = await session.exec(["python", "-c", probe], timeout_s=30)
    assert result.exit_code == 7
