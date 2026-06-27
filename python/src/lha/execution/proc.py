"""A small, cross-platform subprocess runner shared by the sandbox and the verifier.

Uses blocking ``subprocess.Popen`` in a worker thread (via ``asyncio.to_thread``) rather than the
asyncio subprocess API, to sidestep Windows event-loop subprocess quirks. Never invokes a shell
(argv list only). Hardening:

- **Minimal environment** (``child_env``): children never inherit the host environment, so API keys
  and ``LHA_*`` settings cannot leak into agent-run code. Only an allowlist (``PATH``, locale,
  ``TMPDIR``, a few OS essentials) is copied, ``HOME`` is pointed at the workspace, and callers may
  add explicit extras.
- **Bounded output**: stdout/stderr are drained concurrently into ``BoundedBuffer``s that keep the
  head and tail and drop the middle, so a chatty or hostile process cannot exhaust memory.
- **Validated, capped timeout**: ``timeout_s`` must be a positive int (``bool`` rejected) and is
  capped at ``MAX_TIMEOUT_S``.
- **Whole-tree kill**: POSIX children start in a new session; on timeout the whole process group
  is ``SIGKILL``-ed, not just the direct child.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import signal
import subprocess
import tempfile
import threading
from collections.abc import Mapping
from pathlib import Path
from typing import IO

from lha.contracts.sandbox import ExecResult

MAX_TIMEOUT_S = 3600
DEFAULT_MAX_OUTPUT_BYTES = 1_000_000  # per stream (head + tail kept)
_READ_CHUNK = 65_536
_POSIX = os.name == "posix"

# Host variables a child may inherit. Everything else (API keys, LHA_*, cloud creds, SSH agent
# sockets, ...) is dropped. HOME is always overridden to the workspace.
_INHERITED_ENV = (
    "PATH",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "TZ",
    "TERM",
    "UV_CACHE_DIR",
    # Windows: subprocesses fail to start without these.
    "SYSTEMROOT",
    "SYSTEMDRIVE",
    "COMSPEC",
    "PATHEXT",
    "WINDIR",
)
_DEFAULT_PATH = "/usr/local/bin:/usr/bin:/bin"


def child_env(
    home: str,
    extra: Mapping[str, str] | None = None,
    *,
    base: Mapping[str, str] | None = None,
) -> dict[str, str]:
    """Build the minimal environment for a child process rooted at ``home`` (the workspace).

    ``base`` defaults to ``os.environ``; only allowlisted names are copied from it.
    ``extra`` entries are the caller's explicit choice and are applied last.
    """
    source = os.environ if base is None else base
    env = {k: source[k] for k in _INHERITED_ENV if k in source}
    env.setdefault("PATH", _DEFAULT_PATH)
    env.setdefault("LANG", "C.UTF-8")
    env["HOME"] = home
    env["TMPDIR"] = source.get("TMPDIR") or tempfile.gettempdir()
    # Keep tool caches out of the workspace (HOME) so they are not committed with the agent's work.
    env["XDG_CACHE_HOME"] = source.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    env["GIT_TERMINAL_PROMPT"] = "0"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    if extra:
        env.update(extra)
    return env


def validate_timeout(timeout_s: object) -> int:
    """Return a safe timeout: a positive int capped at ``MAX_TIMEOUT_S``.

    Raises ``ValueError`` for bools, non-ints and non-positive values (``True`` is an ``int``
    subclass and would otherwise mean a 1-second timeout).
    """
    if isinstance(timeout_s, bool) or not isinstance(timeout_s, int):
        raise ValueError(f"timeout_s must be an int, got {type(timeout_s).__name__}")
    if timeout_s <= 0:
        raise ValueError(f"timeout_s must be positive, got {timeout_s}")
    return min(timeout_s, MAX_TIMEOUT_S)


class BoundedBuffer:
    """Accumulates bytes but keeps at most ``limit`` of them: the head and the most recent tail."""

    def __init__(self, limit: int = DEFAULT_MAX_OUTPUT_BYTES) -> None:
        self._head_limit = limit // 2
        self._tail_limit = limit - self._head_limit
        self._head = bytearray()
        self._tail = bytearray()
        self.dropped = 0

    def write(self, chunk: bytes) -> None:
        room = self._head_limit - len(self._head)
        if room > 0:
            self._head += chunk[:room]
            chunk = chunk[room:]
        if not chunk:
            return
        self._tail += chunk
        excess = len(self._tail) - self._tail_limit
        if excess > 0:
            del self._tail[:excess]
            self.dropped += excess

    def text(self) -> str:
        head = bytes(self._head).decode("utf-8", errors="replace")
        tail = bytes(self._tail).decode("utf-8", errors="replace")
        if self.dropped:
            return f"{head}\n…[{self.dropped} bytes truncated]…\n{tail}"
        return head + tail


def _drain(stream: IO[bytes], sink: BoundedBuffer) -> None:
    # Popen pipes are buffered readers: ``read1`` returns what is available without waiting to
    # fill the whole chunk. Fall back to ``read`` for plain ``IO[bytes]`` streams.
    read = getattr(stream, "read1", stream.read)
    try:
        while chunk := read(_READ_CHUNK):
            sink.write(chunk)
    except (OSError, ValueError):
        pass
    finally:
        stream.close()


def _killpg(proc: subprocess.Popen[bytes]) -> None:
    if _POSIX:
        with contextlib.suppress(ProcessLookupError, PermissionError):
            os.killpg(proc.pid, signal.SIGKILL)


def _kill_tree(proc: subprocess.Popen[bytes]) -> None:
    _killpg(proc)
    with contextlib.suppress(ProcessLookupError):
        proc.kill()


async def run_proc(
    argv: list[str],
    *,
    cwd: str,
    timeout_s: int = 600,
    env: dict[str, str] | None = None,
    max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES,
    home: str | None = None,
) -> ExecResult:
    """Run ``argv`` in ``cwd`` and return a real ``ExecResult`` (never raises on non-zero exit).

    The child gets ``child_env(home or cwd, extra=env)`` — never the full host environment.
    Raises ``ValueError`` for an invalid ``timeout_s`` (see ``validate_timeout``).
    """
    timeout = validate_timeout(timeout_s)
    child = child_env(home or cwd, env)

    def _run() -> ExecResult:
        out, err = BoundedBuffer(max_output_bytes), BoundedBuffer(max_output_bytes)
        try:
            proc = subprocess.Popen(
                argv,
                cwd=cwd,
                env=child,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=_POSIX,
            )
        except (FileNotFoundError, PermissionError, NotADirectoryError) as exc:
            return ExecResult(exit_code=127, stdout="", stderr=str(exc))
        assert proc.stdout is not None and proc.stderr is not None
        readers = [
            threading.Thread(target=_drain, args=(proc.stdout, out), daemon=True),
            threading.Thread(target=_drain, args=(proc.stderr, err), daemon=True),
        ]
        for reader in readers:
            reader.start()
        timed_out = False
        try:
            proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            _kill_tree(proc)
            proc.wait()
        finally:
            # Reap stragglers that kept the group alive after the leader exited.
            _killpg(proc)
        for reader in readers:
            reader.join(timeout=5)
        stderr = err.text()
        if timed_out:
            stderr += f"\n[timed out after {timeout}s]"
        return ExecResult(
            exit_code=-1 if timed_out else proc.returncode,
            stdout=out.text(),
            stderr=stderr,
            timed_out=timed_out,
        )

    return await asyncio.to_thread(_run)
