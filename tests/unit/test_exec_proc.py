"""S3/S8: child env allowlist, bounded output, timeout validation, process-group kill."""

from __future__ import annotations

import os
import sys
import time
from pathlib import Path

import pytest

from lha.execution.proc import (
    MAX_TIMEOUT_S,
    BoundedBuffer,
    child_env,
    run_proc,
    validate_timeout,
)

PY = sys.executable


def test_child_env_is_allowlisted() -> None:
    base = {
        "PATH": "/bin",
        "LANG": "en_US.UTF-8",
        "ANTHROPIC_API_KEY": "sk-secret",
        "LHA_DATABASE_URL": "postgres://secret",
        "AWS_SECRET_ACCESS_KEY": "x",
        "HOME": "/Users/me",
    }
    env = child_env("/work", {"EXTRA": "1"}, base=base)
    assert env["HOME"] == "/work"
    assert env["PATH"] == "/bin"
    assert env["EXTRA"] == "1"
    assert "ANTHROPIC_API_KEY" not in env
    assert "LHA_DATABASE_URL" not in env
    assert "AWS_SECRET_ACCESS_KEY" not in env


@pytest.mark.asyncio
async def test_run_proc_does_not_leak_host_secrets(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("OPENAI_API_KEY", "sk-leak")
    monkeypatch.setenv("LHA_ANTHROPIC_API_KEY", "sk-leak2")
    result = await run_proc(
        [PY, "-c", "import os, json; print(json.dumps(dict(os.environ)))"], cwd=str(tmp_path)
    )
    assert result.ok, result.stderr
    assert "sk-leak" not in result.stdout
    assert str(tmp_path) in result.stdout  # HOME points at the workspace


@pytest.mark.parametrize("bad", [True, False, 0, -5, 1.5, "10", None])
def test_validate_timeout_rejects(bad: object) -> None:
    with pytest.raises(ValueError):
        validate_timeout(bad)


def test_validate_timeout_caps() -> None:
    assert validate_timeout(10) == 10
    assert validate_timeout(10**9) == MAX_TIMEOUT_S


@pytest.mark.asyncio
async def test_run_proc_rejects_bool_timeout(tmp_path: Path) -> None:
    with pytest.raises(ValueError):
        await run_proc([PY, "-c", "pass"], cwd=str(tmp_path), timeout_s=True)


def test_bounded_buffer_keeps_head_and_tail() -> None:
    buf = BoundedBuffer(100)
    for _ in range(100):
        buf.write(b"x" * 10)
    buf.write(b"END")
    text = buf.text()
    assert "truncated" in text
    assert text.endswith("END")
    assert len(text) < 200


@pytest.mark.asyncio
async def test_run_proc_output_is_bounded(tmp_path: Path) -> None:
    script = "import sys\nfor _ in range(2000): sys.stdout.write('y' * 1000)\nprint('TAIL')"
    result = await run_proc([PY, "-c", script], cwd=str(tmp_path), max_output_bytes=10_000)
    assert result.ok
    assert len(result.stdout) < 11_000
    assert result.stdout.rstrip().endswith("TAIL")


@pytest.mark.skipif(os.name != "posix", reason="process groups are POSIX-only")
@pytest.mark.asyncio
async def test_timeout_kills_the_whole_process_group(tmp_path: Path) -> None:
    pid_file = tmp_path / "child.pid"
    script = (
        "import subprocess, sys, time\n"
        f"p = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
        f"open({str(pid_file)!r}, 'w').write(str(p.pid))\n"
        "time.sleep(60)\n"
    )
    started = time.monotonic()
    result = await run_proc([PY, "-c", script], cwd=str(tmp_path), timeout_s=2)
    assert result.timed_out and not result.ok
    assert time.monotonic() - started < 20
    grandchild = int(pid_file.read_text())
    for _ in range(50):
        try:
            os.kill(grandchild, 0)
        except ProcessLookupError:
            break
        time.sleep(0.1)
    else:
        pytest.fail("grandchild survived the timeout")
