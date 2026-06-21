"""Tests for the local sandbox and the deterministic verifier.

Uses ``sys.executable -c ...`` so the commands are deterministic and cross-platform (no reliance
on shell builtins). Proves the verifier gates on real exit codes and that advisory checks don't
block — the core of the "done == verified" guarantee.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from lha.contracts.verify import Check
from lha.execution.sandbox_local import LocalSandbox
from lha.state import git_ops
from lha.verify.verifier import DeterministicVerifier

PY = sys.executable


@pytest.mark.asyncio
async def test_local_sandbox_exec_and_file_io(tmp_path: Path) -> None:
    sandbox = LocalSandbox()
    session = await sandbox.open(workdir=str(tmp_path))

    await session.write_file("sub/hello.txt", "hi")
    assert await session.read_file("sub/hello.txt") == "hi"

    ok = await session.exec([PY, "-c", "print('ok')"])
    assert ok.ok
    assert "ok" in ok.stdout

    bad = await session.exec([PY, "-c", "import sys; sys.exit(3)"])
    assert not bad.ok
    assert bad.exit_code == 3

    await session.close()


@pytest.mark.asyncio
async def test_verifier_gates_on_exit_codes(tmp_path: Path) -> None:
    verifier = DeterministicVerifier(default_timeout_s=60)
    session = await LocalSandbox().open(workdir=str(tmp_path))
    result = await verifier.verify(
        session,
        [
            Check(name="ok", command=[PY, "-c", "print('fine')"]),
            Check(name="fails", command=[PY, "-c", "import sys; sys.exit(1)"]),
        ],
    )
    assert not result.all_green  # a gating check failed
    by_name = {r.name: r for r in result.results}
    assert by_name["ok"].passed
    assert not by_name["fails"].passed
    assert by_name["fails"].exit_code == 1


@pytest.mark.asyncio
async def test_verifier_advisory_check_does_not_block(tmp_path: Path) -> None:
    verifier = DeterministicVerifier(default_timeout_s=60)
    session = await LocalSandbox().open(workdir=str(tmp_path))
    result = await verifier.verify(
        session,
        [
            Check(name="ok", command=[PY, "-c", "print('fine')"]),
            Check(name="advisory", command=[PY, "-c", "import sys; sys.exit(1)"], gating=False),
        ],
    )
    assert result.all_green  # advisory failure is recorded but does not gate
    assert any(not r.passed for r in result.results)


@pytest.mark.asyncio
async def test_local_sandbox_snapshot_returns_git_head(tmp_path: Path) -> None:
    git_ops.init_repo(tmp_path)
    (tmp_path / "f.txt").write_text("x", encoding="utf-8")
    git_ops.commit_all(tmp_path, "init")

    sandbox = LocalSandbox()
    session = await sandbox.open(workdir=str(tmp_path))
    snap = await sandbox.snapshot(session)

    assert snap.kind == "local"
    assert snap.snapshot_id == git_ops.head_sha(tmp_path)
