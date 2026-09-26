"""The E2B sandbox against a fake SDK: the host workdir and the microVM stay in step.

The fake ``AsyncSandbox`` stands the "VM" up in a temp directory and runs commands with a local
shell, so the real sync helper (``lha_sync.py``) runs end to end, just not in a microVM.
"""

from __future__ import annotations

import os
import stat
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, ClassVar

import pytest

from lha.contracts.sandbox import host_root
from lha.contracts.verify import Check
from lha.execution.factory import build_sandbox
from lha.execution.sandbox_e2b import (
    E2BSandbox,
    E2BSandboxSession,
    E2BSyncError,
    _safe_host_path,
    _unpack_host,
    host_manifest,
)
from lha.state.git_ops import commit_all, init_repo, run_git
from lha.verify.trusted import candidate_commit
from lha.verify.verifier import DeterministicVerifier


@dataclass
class _Result:
    exit_code: Any
    stdout: str = ""
    stderr: str = ""


class CommandExitException(Exception):  # named like the SDK's
    """Like the SDK's: raised for a non-zero exit, carrying the result."""

    def __init__(self, result: _Result) -> None:
        super().__init__(f"exit {result.exit_code}")
        self.exit_code = result.exit_code
        self.stdout = result.stdout
        self.stderr = result.stderr


class TimeoutException(Exception):  # named like the SDK's
    pass


class _Commands:
    def __init__(self, owner: FakeAsyncSandbox) -> None:
        self._owner = owner
        self.calls: list[str] = []

    async def run(
        self,
        cmd: str,
        *,
        cwd: str | None = None,
        timeout: float | None = None,
        envs: dict[str, str] | None = None,
    ) -> _Result:
        self.calls.append(cmd)
        override = self._owner.next_result.pop(0) if self._owner.next_result and cwd else None
        if isinstance(override, BaseException):
            raise override
        env = {**os.environ, **(envs or {})}
        proc = subprocess.run(
            ["sh", "-c", cmd], cwd=cwd, capture_output=True, text=True, env=env, check=False
        )
        result = _Result(proc.returncode, proc.stdout, proc.stderr)
        if override is not None:
            result = override
        if isinstance(result.exit_code, int) and result.exit_code != 0:
            raise CommandExitException(result)
        return result


class _Files:
    async def write(self, path: str, data: str | bytes) -> None:
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data.encode() if isinstance(data, str) else data)

    async def read(self, path: str, format: str = "text") -> Any:
        raw = Path(path).read_bytes()
        return bytearray(raw) if format == "bytes" else raw.decode()


class FakeAsyncSandbox:
    """The slice of ``e2b_code_interpreter.AsyncSandbox`` the adapter uses."""

    created: ClassVar[list[FakeAsyncSandbox]] = []

    def __init__(self, template: str) -> None:
        self.template = template
        self.commands = _Commands(self)
        self.files = _Files()
        self.killed = False
        # Replacement results (or exceptions) for the next workspace commands (those with cwd).
        self.next_result: list[Any] = []

    @classmethod
    async def create(cls, template: str) -> FakeAsyncSandbox:
        sbx = cls(template)
        cls.created.append(sbx)
        return sbx

    async def kill(self) -> None:
        self.killed = True


@pytest.fixture
def host(tmp_path: Path) -> Path:
    repo = tmp_path / "host"
    init_repo(repo)
    (repo / ".gitignore").write_text(".env\nbuild/\n")
    (repo / "a.txt").write_text("a1\n")
    (repo / "keep.txt").write_text("keep\n")
    (repo / "sub").mkdir()
    (repo / "sub" / "b.txt").write_text("b1\n")
    commit_all(repo, "base")
    (repo / "untracked.txt").write_text("u\n")
    (repo / ".env").write_text("SECRET=1\n")
    (repo / ".lha").mkdir()
    (repo / ".lha" / "state.json").write_text("{}")
    (repo / "link").symlink_to("a.txt")
    return repo


def _sandbox(tmp_path: Path, **kw: Any) -> E2BSandbox:
    vm = tmp_path / "vm"
    return E2BSandbox(
        "base",
        sandbox_cls=FakeAsyncSandbox,
        vm_workdir=str(vm / "workspace"),
        sync_dir=str(vm / "sync"),
        python=sys.executable,
        **kw,
    )


async def _open(tmp_path: Path, host: Path, **kw: Any) -> tuple[E2BSandboxSession, Path]:
    session = await _sandbox(tmp_path, **kw).open(workdir=str(host))
    assert isinstance(session, E2BSandboxSession)
    return session, Path(session.workdir)


def _tree(root: Path) -> set[str]:
    return {
        p.relative_to(root).as_posix() for p in root.rglob("*") if p.is_file() or p.is_symlink()
    }


async def test_open_syncs_the_committable_tree_in(tmp_path: Path, host: Path) -> None:
    session, vm = await _open(tmp_path, host)
    assert _tree(vm) == {".gitignore", "a.txt", "keep.txt", "sub/b.txt", "untracked.txt", "link"}
    assert (vm / "link").is_symlink() and os.readlink(vm / "link") == "a.txt"
    assert not (vm / ".env").exists() and not (vm / ".git").exists()
    assert host_root(session) == str(host.resolve())
    await session.close()
    assert FakeAsyncSandbox.created[-1].killed


async def test_exec_changes_come_back_to_the_host(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    script = (
        "echo a2 > a.txt; echo new > sub/new.txt; rm untracked.txt; chmod +x keep.txt; "
        "mkdir -p build && echo junk > build/out; echo LEAK > .env; "
        "mkdir -p .git/hooks && echo evil > .git/hooks/pre-commit; "
        "mkdir -p nested/.git && echo x > nested/.git/config; ln -s sub/b.txt link2"
    )
    result = await session.exec(["sh", "-c", script])
    assert result.ok, result.stderr
    assert (host / "a.txt").read_text() == "a2\n"
    assert (host / "sub" / "new.txt").read_text() == "new\n"
    assert not (host / "untracked.txt").exists()
    assert os.stat(host / "keep.txt").st_mode & stat.S_IXUSR
    assert os.readlink(host / "link2") == "sub/b.txt"
    # Ignored output, the host's ignored secrets and any .git path stay where they are.
    assert not (host / "build").exists()
    assert (host / ".env").read_text() == "SECRET=1\n"
    assert not (host / ".git" / "hooks" / "pre-commit").exists()
    assert not (host / "nested").exists()
    assert (host / ".lha" / "state.json").read_text() == "{}"


async def test_verification_runs_on_the_tree_the_checkpoint_commits(
    tmp_path: Path, host: Path
) -> None:
    session, _vm = await _open(tmp_path, host)
    await session.write_file("feature.py", "VALUE = 42\n")
    assert (host / "feature.py").read_text() == "VALUE = 42\n"
    assert await session.read_file("feature.py") == "VALUE = 42\n"
    check = Check(name="feature", command=["grep", "-q", "VALUE = 42", "feature.py"])
    verdict = await DeterministicVerifier().verify(session, [check])
    assert verdict.all_green
    commit = candidate_commit(host)
    assert run_git(host, "show", f"{commit}:feature.py") == "VALUE = 42"


async def test_host_changes_between_operations_reach_the_vm(tmp_path: Path, host: Path) -> None:
    session, vm = await _open(tmp_path, host)
    assert (await session.exec(["sh", "-c", "echo attempt > a.txt; echo x > extra.txt"])).ok
    assert (host / "extra.txt").exists()
    # The harness rolls the failed attempt back on the host.
    run_git(host, "checkout", "--", "a.txt")
    (host / "extra.txt").unlink()
    (host / "keep.txt").write_text("restored by the harness\n")
    result = await session.exec(["cat", "a.txt", "keep.txt"])
    assert result.stdout == "a1\nrestored by the harness\n"
    assert not (vm / "extra.txt").exists()
    # ...and those host changes are not mistaken for VM changes afterwards.
    assert (host / "keep.txt").read_text() == "restored by the harness\n"
    assert not (host / "extra.txt").exists()


async def test_a_missing_exit_code_is_a_failure(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    FakeAsyncSandbox.created[-1].next_result = [_Result(None, "looked fine", "")]
    result = await session.exec(["true"])
    assert result.exit_code == -1 and not result.ok
    assert "no exit code" in result.stderr and result.stdout == "looked fine"
    FakeAsyncSandbox.created[-1].next_result = [_Result(True)]
    assert (await session.exec(["true"])).exit_code == -1


async def test_sdk_exceptions_map_to_failed_results(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    result = await session.exec(["sh", "-c", "echo out; echo err >&2; exit 3"])
    assert (result.exit_code, result.stdout, result.stderr) == (3, "out\n", "err\n")
    sbx = FakeAsyncSandbox.created[-1]
    sbx.next_result = [TimeoutException("deadline")]
    result = await session.exec(["sleep", "100"], timeout_s=5)
    assert result.timed_out and not result.ok and "timed out after 5s" in result.stderr
    sbx.next_result = [TimeoutError()]
    assert (await session.exec(["true"])).timed_out
    sbx.next_result = [RuntimeError("connection reset")]
    result = await session.exec(["true"])
    assert result.exit_code == -1 and "connection reset" in result.stderr


async def test_escaping_symlink_from_the_vm_fails_closed(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    outside = tmp_path / "outside.txt"
    outside.write_text("do not touch\n")
    result = await session.exec(["sh", "-c", f"ln -s {outside} evil"])
    assert result.exit_code == -1 and "workspace sync failed" in result.stderr
    assert not (host / "evil").is_symlink()
    assert outside.read_text() == "do not touch\n"


async def test_directory_swapped_for_a_symlink_fails_closed(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    outside = tmp_path / "outside"
    outside.mkdir()
    result = await session.exec(["sh", "-c", f"rm -rf sub && ln -s {outside} sub"])
    assert result.exit_code == -1 and "workspace sync failed" in result.stderr
    assert not (host / "sub").is_symlink()
    assert list(outside.iterdir()) == []
    # Fixing the tree in the VM brings the host back in step.
    assert (await session.exec(["sh", "-c", "rm sub && mkdir sub && echo b2 > sub/b.txt"])).ok
    assert (host / "sub" / "b.txt").read_text() == "b2\n"


async def test_a_symlink_that_becomes_a_directory(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    assert (await session.exec(["sh", "-c", "ln -s sub d"])).ok
    assert (host / "d").is_symlink()
    assert (await session.exec(["sh", "-c", "rm d && mkdir d && echo f > d/f.txt"])).ok
    assert (host / "d").is_dir() and not (host / "d").is_symlink()
    assert (host / "d" / "f.txt").read_text() == "f\n"
    assert not (host / "sub" / "f.txt").exists()


async def test_sync_size_limit_fails_closed(tmp_path: Path, host: Path) -> None:
    with pytest.raises(E2BSyncError, match="sync limit"):
        await _open(tmp_path, host, max_sync_bytes=10)
    assert FakeAsyncSandbox.created[-1].killed
    session, _vm = await _open(tmp_path, host, max_sync_bytes=4096)
    result = await session.exec(["sh", "-c", "head -c 10000 /dev/zero > big.bin"])
    assert result.exit_code == -1 and "sync limit" in result.stderr
    assert not (host / "big.bin").exists()
    (host / "big-host.bin").write_bytes(b"\0" * 10000)
    result = await session.exec(["true"])
    assert result.exit_code == -1 and "sync limit" in result.stderr
    with pytest.raises(E2BSyncError):
        await session.read_file("a.txt")


async def test_a_failing_sync_command_fails_open(tmp_path: Path, host: Path) -> None:
    with pytest.raises(E2BSyncError, match="sandbox command failed"):
        await E2BSandbox(
            sandbox_cls=FakeAsyncSandbox,
            vm_workdir=str(tmp_path / "vm" / "w"),
            sync_dir=str(tmp_path / "vm" / "s"),
            python="/definitely/not/python3",
        ).open(workdir=str(host))


async def test_unreadable_listing_fails_closed(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    helper = tmp_path / "vm" / "sync" / "lha_sync.py"
    helper.write_text("import sys\nopen(sys.argv[3], 'w').write('[1, 2]')\n")
    result = await session.exec(["true"])
    assert result.exit_code == -1 and "listing" in result.stderr
    helper.write_text("import sys\nopen(sys.argv[3], 'w').write('not json')\n")
    assert (await session.exec(["true"])).exit_code == -1


async def test_snapshots_are_not_supported(tmp_path: Path, host: Path) -> None:
    sandbox = _sandbox(tmp_path)
    with pytest.raises(NotImplementedError):
        await sandbox.open(workdir=str(host), snapshot_id="snap-1")
    session = await sandbox.open(workdir=str(host))
    with pytest.raises(NotImplementedError):
        await sandbox.snapshot(session)
    with pytest.raises(FileNotFoundError):
        await sandbox.open(workdir=str(tmp_path / "missing"))


async def test_a_workdir_outside_git_syncs_every_file(tmp_path: Path) -> None:
    plain = tmp_path / "plain"
    (plain / "d").mkdir(parents=True)
    (plain / "d" / "f.txt").write_text("f\n")
    (plain / ".git").mkdir()
    (plain / ".git" / "config").write_text("x")
    (plain / "dirlink").symlink_to("d")
    assert host_manifest(plain) == ["d/f.txt", "dirlink"]
    session, vm = await _open(tmp_path, plain)
    assert _tree(vm) == {"d/f.txt", "dirlink"}
    assert (await session.exec(["sh", "-c", "echo g > d/g.txt"])).ok
    assert (plain / "d" / "g.txt").read_text() == "g\n"


async def test_a_gitignore_change_applies_to_the_same_sync(tmp_path: Path, host: Path) -> None:
    session, _vm = await _open(tmp_path, host)
    script = "printf '.env\\nbuild/\\n*.tmp\\n' > .gitignore; echo t > scratch.tmp; echo k > k.txt"
    assert (await session.exec(["sh", "-c", script])).ok
    assert "*.tmp" in (host / ".gitignore").read_text()
    assert not (host / "scratch.tmp").exists() and (host / "k.txt").exists()


def test_safe_host_path(tmp_path: Path) -> None:
    root = tmp_path / "root"
    root.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    (root / "out").symlink_to(outside)
    assert _safe_host_path(root, "a/b.txt") == root / "a" / "b.txt"
    for bad in ["../x", "/etc/passwd", ".git/config", "x/.GIT/hooks", ".lha/s", "./a", ""]:
        with pytest.raises(E2BSyncError):
            _safe_host_path(root, bad)
    with pytest.raises(E2BSyncError, match="through a symlink"):
        _safe_host_path(root, "out/new/file.txt")


def test_unpack_refuses_members_it_did_not_ask_for(tmp_path: Path) -> None:
    import io
    import tarfile

    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        info = tarfile.TarInfo("sneaky.txt")
        info.size = 1
        tar.addfile(info, io.BytesIO(b"x"))
    with pytest.raises(E2BSyncError, match="unexpected entry"):
        _unpack_host(tmp_path, buf.getvalue(), {"wanted.txt"})
    with pytest.raises(E2BSyncError, match="could not copy"):
        _unpack_host(tmp_path, b"not a tar archive at all" * 40, set())


def test_factory_builds_the_e2b_sandbox() -> None:
    sandbox = build_sandbox("e2b", template="custom")
    assert isinstance(sandbox, E2BSandbox) and sandbox.name == "e2b"
