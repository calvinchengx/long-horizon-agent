"""E2B (Firecracker microVM) sandbox adapter, with the workspace synced both ways.

Behind the same ``Sandbox`` interface as Local/Docker, so the agent loop is unchanged. The microVM
has its own filesystem, so the HOST workdir stays the source of truth — it is what harness
integrity, the host-side tools, trusted checks and the git checkpoint read — and the session keeps
the two in step:

- **sync in** (``open``, then before EVERY ``exec``/``write_file``/``read_file``): the host
  workdir's tracked and untracked, non-ignored files (``git ls-files -co --exclude-standard``;
  every file when the workdir is not in a git work tree) are sent to the VM as one tar — on
  ``open`` all of them, afterwards only what changed on the host since the last sync (the harness
  rolling a failed attempt back, restoring a harness file), and host deletions are replayed.
  ``.git``/``.lha`` and any ``.git`` path component are never sent, nor are ignored files
  (``.env``, ``.venv``...); symlinks are sent as symlinks.
- **sync out** (after EVERY ``exec`` and ``write_file``): the VM lists its workspace (path, type,
  size, mtime, ctime); files that are new or changed since the last sync are packed and copied
  to the host, host files the session placed there that are gone from the VM are deleted.
  Paths the HOST's ignore rules ignore are not copied back (build output, virtualenvs). So when
  the verifier's checks run (in the VM, through ``exec``) and the checkpoint commits the host
  tree, both see the same files.

Every sync step fails closed: a sync that does not complete (an unsafe path or symlink, a size
over ``max_sync_bytes``, a VM command that fails) turns the ``exec`` into a FAILED result
(exit code -1) and ``write_file`` into an error, never a success on a tree git did not get.
Copy-back is the security boundary here, so it uses ``tarfile``'s ``data`` filter (no absolute or
escaping paths or links, no device files), only accepts the members it asked for, refuses paths
through ``.git``/``.lha``, and never writes through a host symlink.

A command whose exit code the SDK does not report is a failure, not a success. There are no
snapshots: the durable workflow never stores one, and the host workdir is re-uploaded on every
``open`` instead (``snapshot`` and ``open(snapshot_id=...)`` raise ``NotImplementedError``).

The VM side needs ``python3`` (3.8+) — the sync helper is a small script uploaded on ``open``.
Requires the ``e2b-code-interpreter`` package and an E2B API key (the SDK's own configuration).
Unit-tested against a fake SDK that runs the helper locally; not exercised against the E2B service
in CI.
"""

from __future__ import annotations

import asyncio
import contextlib
import io
import json
import os
import shlex
import stat
import subprocess
import tarfile
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from lha.contracts.sandbox import ExecResult, Sandbox, SandboxSession, Snapshot
from lha.execution.paths import PROTECTED_DIRS, PathEscapeError, contained_posix, normalize_relpath
from lha.execution.proc import DEFAULT_MAX_OUTPUT_BYTES, BoundedBuffer, validate_timeout
from lha.state.git_ops import GIT_TIMEOUT_S, _git_env, toplevel

_WORKDIR = "/home/user/workspace"
_SYNC_DIR = "/tmp/lha-sync"
# Most bytes one sync (in or out) may move; a larger transfer fails closed.
DEFAULT_MAX_SYNC_BYTES = 256 * 1024 * 1024
_SYNC_TIMEOUT_S = 600
_HOST_GRACE_S = 30
_NO_EXIT_CODE = -1

# The helper the VM runs to list, pack and unpack its workspace. Plain python3 (no GNU-only
# find/tar flags), so it runs on any template with Python.
_HELPER = r"""
import json, os, sys, tarfile

def skip(rel):
    parts = rel.split("/")
    return parts[0].lower() in (".git", ".lha") or any(p.lower() == ".git" for p in parts)

def listing(root, out):
    entries = {}
    for top, dirs, files in os.walk(root):
        rel_top = os.path.relpath(top, root)
        rel_top = "" if rel_top == "." else rel_top + "/"
        keep = []
        for name in dirs:
            rel = rel_top + name
            if skip(rel):
                continue
            if os.path.islink(os.path.join(top, name)):
                files.append(name)
            else:
                keep.append(name)
        dirs[:] = keep
        for name in files:
            rel = rel_top + name
            if skip(rel):
                continue
            st = os.lstat(os.path.join(top, name))
            kind = "l" if os.path.islink(os.path.join(top, name)) else "f"
            if kind == "f" and not os.path.isfile(os.path.join(top, name)):
                continue
            entries[rel] = [kind, st.st_size, st.st_mtime_ns, st.st_ctime_ns, st.st_mode & 0o777]
    with open(out, "w") as fh:
        json.dump(entries, fh)

def pack(root, want, out):
    with open(want) as fh:
        paths = json.load(fh)
    with tarfile.open(out, "w") as tar:
        for rel in paths:
            full = os.path.join(root, rel)
            if os.path.islink(full) or os.path.isfile(full):
                tar.add(full, arcname=rel, recursive=False)

def unpack(archive, root):
    os.makedirs(root, exist_ok=True)
    kw = {"filter": "fully_trusted"} if hasattr(tarfile, "fully_trusted_filter") else {}
    with tarfile.open(archive) as tar:
        tar.extractall(root, **kw)
    os.remove(archive)

def remove(root, want):
    with open(want) as fh:
        paths = json.load(fh)
    for rel in paths:
        full = os.path.join(root, rel)
        if os.path.islink(full) or os.path.isfile(full):
            os.remove(full)

cmd = sys.argv[1]
if cmd == "list":
    listing(sys.argv[2], sys.argv[3])
elif cmd == "pack":
    pack(sys.argv[2], sys.argv[3], sys.argv[4])
elif cmd == "unpack":
    unpack(sys.argv[2], sys.argv[3])
elif cmd == "remove":
    remove(sys.argv[2], sys.argv[3])
else:
    sys.exit("unknown command")
"""

# One VM entry: [kind "f"|"l", size, mtime_ns, ctime_ns, mode].
Entry = list[Any]


class E2BSyncError(RuntimeError):
    """The workspace could not be synced between the host and the microVM (fails closed)."""


def _clip(text: str) -> str:
    buf = BoundedBuffer(DEFAULT_MAX_OUTPUT_BYTES)
    buf.write(text.encode("utf-8", errors="replace"))
    return buf.text()


def _skipped(rel: str) -> bool:
    """``.git``/``.lha`` at the top, or a ``.git`` component anywhere: never synced."""
    parts = rel.split("/")
    return parts[0].casefold() in PROTECTED_DIRS or any(p.casefold() == ".git" for p in parts)


def _as_bytes(data: Any) -> bytes:
    if isinstance(data, str):
        return data.encode("utf-8", errors="surrogateescape")
    return bytes(data)


def _git_z(cwd: Path, args: list[str], stdin: bytes | None = None) -> list[str]:
    """``git <args>`` with NUL-separated output, as a list of paths (raises ``E2BSyncError``)."""
    try:
        proc = subprocess.run(
            ["git", *args],
            cwd=str(cwd),
            input=stdin,
            capture_output=True,
            env=_git_env(),
            timeout=GIT_TIMEOUT_S,
            stdin=None if stdin is not None else subprocess.DEVNULL,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise E2BSyncError(f"git {args[0]} failed: {exc}") from exc
    # check-ignore exits 1 when nothing is ignored.
    if proc.returncode not in (0, 1) or (proc.returncode == 1 and args[0] != "check-ignore"):
        raise E2BSyncError(f"git {args[0]} failed: {os.fsdecode(proc.stderr).strip()}")
    return [os.fsdecode(p) for p in proc.stdout.split(b"\0") if p]


def host_manifest(host: Path) -> list[str]:
    """Workspace-relative paths the VM gets: the files git would commit (tracked + untracked,
    non-ignored) that exist, or every file when ``host`` is not in a git work tree."""
    if toplevel(host) is not None:
        listed = _git_z(host, ["ls-files", "-z", "-co", "--exclude-standard"])
        paths = sorted({p for p in listed if os.path.lexists(host / p)})
    else:
        paths = []
        for top, dirs, files in os.walk(host):
            rel_top = Path(top).relative_to(host)
            links = [d for d in dirs if (Path(top) / d).is_symlink()]
            dirs[:] = [d for d in dirs if d not in links and not _skipped((rel_top / d).as_posix())]
            paths.extend((rel_top / name).as_posix() for name in [*files, *links])
        paths.sort()
    return [
        p for p in paths if not _skipped(p) and ((host / p).is_symlink() or not (host / p).is_dir())
    ]


def host_state(host: Path) -> dict[str, Entry]:
    """``host_manifest`` with each path's [kind, size, mtime_ns, mode]: what changed on the host
    between two session operations (the harness rolling an attempt back, restoring a harness
    file...) is what differs from the state recorded at the last sync."""
    state: dict[str, Entry] = {}
    for rel in host_manifest(host):
        try:
            st = (host / rel).lstat()
        except OSError:
            continue
        if stat.S_ISLNK(st.st_mode) or stat.S_ISREG(st.st_mode):
            kind = "l" if stat.S_ISLNK(st.st_mode) else "f"
            state[rel] = [kind, st.st_size, st.st_mtime_ns, st.st_mode & 0o777]
    return state


def _pack_host(host: Path, paths: Iterable[str], limit: int) -> bytes:
    buf = io.BytesIO()
    total = 0
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for rel in paths:
            full = host / rel
            st = full.lstat()
            if not (stat.S_ISREG(st.st_mode) or stat.S_ISLNK(st.st_mode)):
                continue
            total += st.st_size
            if total > limit:
                raise E2BSyncError(
                    f"the workspace is larger than the {limit}-byte sync limit; "
                    "ignore generated files in .gitignore"
                )
            tar.add(full, arcname=rel, recursive=False)
    return buf.getvalue()


def _host_ignored(host: Path, paths: list[str]) -> set[str]:
    if not paths or toplevel(host) is None:
        return set()
    stdin = b"\0".join(os.fsencode(p) for p in paths) + b"\0"
    return set(_git_z(host, ["check-ignore", "-z", "--stdin"], stdin=stdin))


def _safe_host_path(host: Path, rel: str) -> Path:
    """``host/rel`` for a path the VM names: lexically contained, not protected, and its parent
    directory (if it exists) resolves inside ``host`` — so nothing is written through a symlink."""
    try:
        norm = normalize_relpath(rel)
    except PathEscapeError as exc:
        raise E2BSyncError(f"unsafe path from the sandbox: {rel!r}") from exc
    if str(norm) == "." or str(norm) != rel or _skipped(rel):
        raise E2BSyncError(f"unsafe path from the sandbox: {rel!r}")
    target = host / rel
    parent = target.parent
    while not parent.exists() and parent != host:
        parent = parent.parent
    if not parent.resolve().is_relative_to(host):
        raise E2BSyncError(f"path leaves the workspace through a symlink: {rel!r}")
    return target


def _clear(target: Path, rel: str) -> None:
    """Remove the host entry at ``target`` (a file or symlink; an empty directory)."""
    if target.is_symlink() or target.is_file():
        target.unlink()
    elif target.is_dir():
        try:
            target.rmdir()
        except OSError as exc:
            raise E2BSyncError(f"cannot replace the non-empty host directory {rel!r}") from exc
    elif os.path.lexists(target):
        raise E2BSyncError(f"cannot replace the special host file {rel!r}")


def _unpack_host(host: Path, data: bytes, wanted: set[str]) -> set[str]:
    """Extract the VM's tar of ``wanted`` paths into ``host``; returns the paths written."""
    written: set[str] = set()
    try:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as tar:
            for member in tar.getmembers():
                rel = member.name
                if rel not in wanted or not (member.isfile() or member.issym()):
                    raise E2BSyncError(f"unexpected entry in the sandbox archive: {rel!r}")
                target = _safe_host_path(host, rel)
                _clear(target, rel)
                tar.extract(member, path=host, filter="data")
                written.add(rel)
    except (tarfile.TarError, OSError) as exc:
        raise E2BSyncError(f"could not copy files back from the sandbox: {exc}") from exc
    return written


class E2BSandboxSession(SandboxSession):
    """A live microVM whose workspace is kept in step with ``host_workdir`` (see module doc)."""

    def __init__(
        self,
        sandbox: Any,
        workdir: str = _WORKDIR,
        *,
        host_workdir: str,
        sync_dir: str = _SYNC_DIR,
        python: str = "python3",
        max_sync_bytes: int = DEFAULT_MAX_SYNC_BYTES,
    ) -> None:
        # The optional ``e2b`` SDK is untyped here (the extra may be absent), hence ``Any``.
        self._sbx: Any = sandbox
        self.workdir = workdir
        # The host directory this VM mirrors (see ``contracts.sandbox.host_root``).
        self.host_workdir = host_workdir
        self._host = Path(host_workdir).resolve()
        self._sync_dir = sync_dir
        self._python = python
        self._limit = max_sync_bytes
        self._baseline: dict[str, Entry] = {}  # the VM listing at the last completed sync
        self._owned: set[str] = set()  # host paths the VM mirrors (deleted when the VM deletes)
        self._host_state: dict[str, Entry] = {}  # ``host_state`` at the last completed sync

    # --- VM plumbing -----------------------------------------------------------------------
    async def _vm(self, argv: list[str], *, timeout: int = _SYNC_TIMEOUT_S) -> None:
        """Run a sync command in the VM; anything but a reported exit 0 is an ``E2BSyncError``."""
        command = " ".join(shlex.quote(a) for a in argv)
        try:
            result = await self._sbx.commands.run(command, timeout=timeout)
        except Exception as exc:
            raise E2BSyncError(f"sandbox command failed: {type(exc).__name__}: {exc}") from exc
        code = getattr(result, "exit_code", None)
        if code != 0:
            stderr = str(getattr(result, "stderr", "") or "").strip()
            raise E2BSyncError(f"sandbox command failed (exit {code}): {stderr}")

    def _helper(self, *args: str) -> list[str]:
        return [self._python, f"{self._sync_dir}/lha_sync.py", *args]

    async def _read_vm(self, path: str) -> bytes:
        return _as_bytes(await self._sbx.files.read(path, format="bytes"))

    async def _listing(self) -> dict[str, Entry]:
        out = f"{self._sync_dir}/list.json"
        await self._vm(self._helper("list", self.workdir, out))
        try:
            data = json.loads(await self._read_vm(out))
        except ValueError as exc:
            raise E2BSyncError(f"unreadable workspace listing from the sandbox: {exc}") from exc
        if not isinstance(data, dict):
            raise E2BSyncError("unreadable workspace listing from the sandbox")
        return {str(k): list(v) for k, v in data.items() if not _skipped(str(k))}

    # --- sync --------------------------------------------------------------------------------
    async def sync_in(self) -> None:
        """Create the VM workspace, install the helper and push the whole host workspace."""
        await self._vm(["mkdir", "-p", self.workdir, self._sync_dir])
        await self._sbx.files.write(f"{self._sync_dir}/lha_sync.py", _HELPER)
        self._host_state = {}
        await self.push()

    async def push(self) -> None:
        """Send host changes made since the last sync (all of it on ``open``) into the VM.

        Between two session operations the harness may change the host tree — roll a failed
        attempt back, restore a tampered harness file — and the VM must follow, or the next
        check would run on files git no longer has. The host wins for the paths it changed.
        """
        current = await asyncio.to_thread(host_state, self._host)
        changed = sorted(p for p, e in current.items() if self._host_state.get(p) != e)
        deleted = sorted(set(self._host_state) - set(current))
        if changed:
            archive = await asyncio.to_thread(_pack_host, self._host, changed, self._limit)
            tar_path = f"{self._sync_dir}/in.tar"
            await self._sbx.files.write(tar_path, archive)
            await self._vm(self._helper("unpack", tar_path, self.workdir))
        if deleted:
            want = f"{self._sync_dir}/remove.json"
            await self._sbx.files.write(want, json.dumps(deleted))
            await self._vm(self._helper("remove", self.workdir, want))
        if changed or deleted:
            # Only the pushed paths get a new VM baseline: other VM changes (a background
            # process still writing) are still picked up by the next ``sync_out``.
            listing = await self._listing()
            for rel in changed:
                if rel in listing:
                    self._baseline[rel] = listing[rel]
            for rel in deleted:
                self._baseline.pop(rel, None)
        self._owned = (self._owned | set(changed)) - set(deleted)
        self._host_state = current

    async def _pull(self, paths: list[str], listing: dict[str, Entry]) -> set[str]:
        if not paths:
            return set()
        size = sum(int(listing[p][1]) for p in paths)
        if size > self._limit:
            raise E2BSyncError(
                f"the sandbox changed {size} bytes, over the {self._limit}-byte sync limit; "
                "ignore generated files in .gitignore"
            )
        want, out = f"{self._sync_dir}/want.json", f"{self._sync_dir}/out.tar"
        await self._sbx.files.write(want, json.dumps(paths))
        await self._vm(self._helper("pack", self.workdir, want, out))
        data = await self._read_vm(out)
        return await asyncio.to_thread(_unpack_host, self._host, data, set(paths))

    async def sync_out(self) -> None:
        """Copy the VM's changes since the last sync back to the host (see module doc)."""
        listing = await self._listing()
        changed = sorted(p for p, e in listing.items() if self._baseline.get(p) != e)
        # Deletions first, so a path that changed type (a symlink now a directory) is gone
        # before anything is written beneath it.
        for rel in sorted(self._owned - set(listing)):
            target = _safe_host_path(self._host, rel)
            if target.is_symlink() or target.is_file():
                target.unlink()
            self._owned.discard(rel)
        # .gitignore files first: the host's ignore rules decide what else comes back.
        rules = [p for p in changed if p.rsplit("/", 1)[-1] == ".gitignore"]
        ignored_rules = await asyncio.to_thread(_host_ignored, self._host, rules)
        rules = [p for p in rules if p not in ignored_rules]
        written = await self._pull(rules, listing)
        rest = [p for p in changed if p not in rules]
        ignored = await asyncio.to_thread(_host_ignored, self._host, rest)
        written |= await self._pull([p for p in rest if p not in ignored], listing)
        self._owned |= written
        self._baseline = listing
        self._host_state = await asyncio.to_thread(host_state, self._host)

    # --- SandboxSession ----------------------------------------------------------------------
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
        try:
            await self.push()
        except E2BSyncError as exc:
            return ExecResult(
                exit_code=_NO_EXIT_CODE, stderr=_clip(f"[e2b] workspace sync failed: {exc}")
            )
        result = await self._run(command, cwd=workdir, timeout=timeout, envs=envs)
        try:
            await self.sync_out()
        except E2BSyncError as exc:
            return ExecResult(
                exit_code=_NO_EXIT_CODE,
                stdout=result.stdout,
                stderr=_clip(f"{result.stderr}\n[e2b] workspace sync failed: {exc}".lstrip("\n")),
                timed_out=result.timed_out,
            )
        return result

    async def _run(
        self, command: str, *, cwd: str, timeout: int, envs: dict[str, str]
    ) -> ExecResult:
        try:
            result: Any = await asyncio.wait_for(
                self._sbx.commands.run(command, cwd=cwd, timeout=timeout, envs=envs),
                timeout=timeout + _HOST_GRACE_S,
            )
        except TimeoutError:
            return ExecResult(
                exit_code=124, stderr=f"[e2b] timed out after {timeout}s", timed_out=True
            )
        except Exception as exc:
            if type(exc).__name__ == "TimeoutException":
                return ExecResult(
                    exit_code=124, stderr=f"[e2b] timed out after {timeout}s: {exc}", timed_out=True
                )
            if not isinstance(getattr(exc, "exit_code", None), int):
                return ExecResult(
                    exit_code=_NO_EXIT_CODE,
                    stderr=_clip(f"[e2b] command failed: {type(exc).__name__}: {exc}"),
                )
            result = exc  # CommandExitException: a non-zero exit, with stdout/stderr
        stdout = _clip(str(getattr(result, "stdout", "") or ""))
        stderr = str(getattr(result, "stderr", "") or "")
        code = getattr(result, "exit_code", None)
        if isinstance(code, bool) or not isinstance(code, int):
            # An unknown exit status is never success.
            stderr = f"{stderr}\n[e2b] the sandbox reported no exit code; counted as a failure"
            return ExecResult(
                exit_code=_NO_EXIT_CODE, stdout=stdout, stderr=_clip(stderr.lstrip("\n"))
            )
        return ExecResult(exit_code=code, stdout=stdout, stderr=_clip(stderr))

    async def write_file(self, relpath: str, content: str) -> None:
        target = contained_posix(self.workdir, relpath)
        await self.push()
        await self._vm(["mkdir", "-p", target.rsplit("/", 1)[0]])
        await self._sbx.files.write(target, content)
        await self.sync_out()

    async def read_file(self, relpath: str) -> str:
        await self.push()
        data = await self._sbx.files.read(contained_posix(self.workdir, relpath))
        return str(data)

    async def close(self) -> None:
        await self._sbx.kill()


class E2BSandbox(Sandbox):
    """E2B microVMs; ``sandbox_cls`` is the SDK's ``AsyncSandbox`` (injectable for tests)."""

    def __init__(
        self,
        template: str = "base",
        *,
        sandbox_cls: Any = None,
        vm_workdir: str = _WORKDIR,
        sync_dir: str = _SYNC_DIR,
        python: str = "python3",
        max_sync_bytes: int = DEFAULT_MAX_SYNC_BYTES,
    ) -> None:
        self.name = "e2b"
        self._template = template
        self._cls = sandbox_cls
        self._vm_workdir = vm_workdir
        self._sync_dir = sync_dir
        self._python = python
        self._limit = max_sync_bytes

    async def open(self, *, workdir: str, snapshot_id: str | None = None) -> SandboxSession:
        if snapshot_id:
            raise NotImplementedError(
                "e2b sessions are not restored from snapshots: the host workdir is the source of "
                "truth and is uploaded on every open"
            )
        if not Path(workdir).is_dir():
            raise FileNotFoundError(f"workdir does not exist: {workdir}")
        cls = self._cls
        if cls is None:
            from e2b_code_interpreter import AsyncSandbox

            cls = AsyncSandbox
        sandbox = await cls.create(self._template)
        session = E2BSandboxSession(
            sandbox,
            self._vm_workdir,
            host_workdir=str(Path(workdir).resolve()),
            sync_dir=self._sync_dir,
            python=self._python,
            max_sync_bytes=self._limit,
        )
        try:
            await session.sync_in()
        except BaseException:
            with contextlib.suppress(Exception):
                await sandbox.kill()
            raise
        return session

    async def snapshot(self, session: SandboxSession) -> Snapshot:
        raise NotImplementedError(
            "e2b sessions are not snapshotted: the host workdir is the source of truth"
        )
