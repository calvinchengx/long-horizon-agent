"""Identifier and idempotency-key helpers.

Two distinct needs:

- ``new_id`` mints fresh, random identifiers. It is NON-deterministic and must only be
  called from Temporal *activities* (never from workflow code, which must be replayable).
- ``idempotency_key`` derives a STABLE key from its parts via a hash, so a re-executed or
  replayed side effect can be detected and skipped exactly. Safe to compute anywhere.
"""

from __future__ import annotations

import hashlib
import uuid


def new_id(prefix: str) -> str:
    """Mint a fresh random id like ``mission_3f9a1c0b2d4e``. Activity-only (non-deterministic)."""
    return f"{prefix}_{uuid.uuid4().hex[:12]}"


def idempotency_key(*parts: str | int) -> str:
    """Derive a deterministic idempotency key from ``parts``.

    Used to make side-effecting work (git commits, branch pushes, DB writes) safe under
    at-least-once execution and Temporal replay: the same logical step always maps to the
    same key, so a repeat is a detectable no-op.
    """
    joined = "\x1f".join(str(p) for p in parts)
    return hashlib.sha256(joined.encode("utf-8")).hexdigest()[:32]
