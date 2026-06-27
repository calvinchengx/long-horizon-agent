"""Content-addressed object store for large-blob spillover (claim-check pattern).

Big LLM/tool payloads are written here keyed by their sha256; only the key is journaled in the
durable history. ``LocalFileObjectStore`` is the $0/dev default; an S3 adapter implements the same
interface for production.

Integrity guarantees of the local store:
  * keys are validated (``^[0-9a-f]{64}$``) before they ever touch the filesystem, so a key can
    never be used as a path (no traversal);
  * the root is resolved to an ABSOLUTE path at construction, so a later ``chdir`` (or a worker
    started from a different directory) cannot silently point at a different store;
  * ``put`` is atomic (temp file in the same directory + ``os.replace``) — a crash mid-write never
    leaves a truncated blob under a valid key;
  * ``get`` re-hashes the bytes and raises ``ObjectCorruptError`` on mismatch.

Blobs are stored in plaintext; there is no encryption at rest here.
"""

from __future__ import annotations

import asyncio
import hashlib
import os
import re
import tempfile
from pathlib import Path
from typing import Protocol, runtime_checkable

_KEY_RE = re.compile(r"^[0-9a-f]{64}$")


class InvalidObjectKeyError(ValueError):
    """The key is not a lowercase hex sha256 digest."""


class ObjectCorruptError(RuntimeError):
    """The stored bytes do not hash to their key."""


def validate_key(key: str) -> str:
    """Return ``key`` if it is a well-formed sha256 hex digest, else raise."""
    if not isinstance(key, str) or not _KEY_RE.fullmatch(key):
        raise InvalidObjectKeyError(f"invalid object key {key!r}: expected 64 lowercase hex chars")
    return key


@runtime_checkable
class ObjectStore(Protocol):
    async def put(self, data: bytes) -> str:
        """Store ``data`` and return its content-addressed key."""
        ...

    async def get(self, key: str) -> bytes:
        """Fetch the bytes for ``key``."""
        ...


class LocalFileObjectStore(ObjectStore):
    """Stores blobs as files under ``root``, named by sha256 (immutable, dedup-friendly).

    ``root`` is required and resolved to an absolute path immediately.
    """

    def __init__(self, root: str | Path) -> None:
        if not str(root).strip():
            raise ValueError("LocalFileObjectStore requires an explicit root directory")
        self._root = Path(root).expanduser().resolve()

    @property
    def root(self) -> Path:
        return self._root

    async def put(self, data: bytes) -> str:
        key = hashlib.sha256(data).hexdigest()

        def _write() -> str:
            self._root.mkdir(parents=True, exist_ok=True)
            path = self._root / key
            if path.exists():
                return key
            fd, tmp = tempfile.mkstemp(dir=self._root, prefix=f".{key[:12]}.", suffix=".tmp")
            try:
                with os.fdopen(fd, "wb") as fh:
                    fh.write(data)
                    fh.flush()
                    os.fsync(fh.fileno())
                os.replace(tmp, path)
            except BaseException:
                Path(tmp).unlink(missing_ok=True)
                raise
            return key

        return await asyncio.to_thread(_write)

    async def get(self, key: str) -> bytes:
        validate_key(key)

        def _read() -> bytes:
            data = (self._root / key).read_bytes()
            if hashlib.sha256(data).hexdigest() != key:
                raise ObjectCorruptError(f"object {key} is corrupt (sha256 mismatch)")
            return data

        return await asyncio.to_thread(_read)
