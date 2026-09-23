"""Append-only decision log — the never-compacted record of implicit design decisions.

Compaction drops implicit action-level decisions (the dangerous ones). Writing them here, append
-only and exempt from compaction, lets later work be checked against them for contradictions.
This is the format of the mission anchor's ``.lha/decisions.ndjson`` (``GitMissionAnchor`` writes
it through ``encode_link``/``parse_chain``/``verify_chain``); ``DecisionLog`` is the same format
as a standalone fsync-ed file.

Line format (one per line, ``\\n``-terminated, UTF-8):

- **chained**: an envelope ``{"prev": <hash>, "hash": <hash>, "record": {...}}`` where
  ``hash = sha256(prev + "\\n" + canonical(record))`` (hex) and ``canonical`` is JSON with sorted
  keys, ``(",", ":")`` separators and ``ensure_ascii=False``. The first ``prev`` is the genesis
  hash (64 zeros) — or, after a legacy prefix, the running hash of that prefix (below).
- **legacy**: a bare ``DecisionRecord`` object, as written before the log was chained. Legacy
  lines are accepted ONLY as a leading prefix; each one is folded into the running hash exactly as
  if it had been chained (``running = sha256(running + "\\n" + canonical(line_object))``), so the
  first chained line seals the whole legacy prefix: editing, reordering or deleting a legacy line
  afterwards breaks that line's ``prev``. A log that is still legacy-only is readable but carries
  no integrity protection (``ChainVerification.legacy`` counts such lines). A legacy line after a
  chained one is tampering (an unchained insertion).

Durability + integrity (``DecisionLog``):
- every append is flushed and ``fsync``-ed before returning;
- ``verify()`` detects edited, reordered, deleted or inserted records;
- a torn final line (a crash mid-write) is skipped by ``read()`` and reported, never fatal; the
  next ``append`` truncates it so the chain continues from the last intact record. Corruption
  anywhere *before* the final line is not a torn write and raises ``DecisionLogCorruptError``.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import structlog

from lha.contracts.state import DecisionRecord

GENESIS_HASH = "0" * 64


class DecisionLogCorruptError(ValueError):
    """A non-final line of the decision log is unreadable (not explainable by a torn write)."""


class DecisionChainError(ValueError):
    """A decision log failed hash-chain verification: its history was altered.

    Raised by ``GitMissionAnchor`` when the committed ``.lha/decisions.ndjson`` does not verify;
    the run paths stop the mission instead of building on a rewritten decision history.
    """


@dataclass
class DecisionLogContents:
    """What ``parse_chain`` / ``DecisionLog.load`` found."""

    records: list[DecisionRecord] = field(default_factory=list)
    last_hash: str = GENESIS_HASH
    torn_tail: str | None = None  # the raw torn final line, if one was skipped
    intact_bytes: int = 0  # length of the prefix holding only intact lines
    legacy: int = 0  # leading unchained (pre-chain) lines


@dataclass
class ChainVerification:
    """Result of ``verify_chain`` / ``DecisionLog.verify``."""

    ok: bool
    checked: int  # records that verified (legacy prefix lines included)
    problem: str = ""
    torn_tail: bool = False
    legacy: int = 0  # of ``checked``, how many are legacy lines (sealed only once chained)


def _canonical(record: dict[str, Any]) -> str:
    return json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _chain_hash(prev: str, record: dict[str, Any]) -> str:
    return hashlib.sha256(f"{prev}\n{_canonical(record)}".encode()).hexdigest()


def _is_envelope(parsed: object) -> bool:
    return isinstance(parsed, dict) and "hash" in parsed and "record" in parsed


def encode_link(prev: str, record: DecisionRecord) -> tuple[str, str]:
    """The envelope line (without its ``\\n``) chaining ``record`` after ``prev``, and its hash."""
    payload = record.model_dump(mode="json")
    digest = _chain_hash(prev, payload)
    line = json.dumps({"prev": prev, "hash": digest, "record": payload}, ensure_ascii=False)
    return line, digest


def _split(data: bytes) -> tuple[list[bytes], bytes]:
    # Split ONLY on "\n" (the record separator): ``str.splitlines`` also breaks on U+2028,
    # U+2029, \x85 etc., which ``json.dumps(ensure_ascii=False)`` leaves unescaped in strings.
    parts = data.split(b"\n")
    # A well-formed log ends with "\n", so the final element is empty; anything else there is a
    # line whose write never completed (torn), even if it happens to parse.
    tail = parts.pop()
    return parts, tail


def parse_chain(data: bytes, *, source: str = "decision log") -> DecisionLogContents:
    """Parse a decision log (chained and/or legacy lines), skipping a torn final line.

    Records are validated but hashes are NOT checked here (``verify_chain`` does that).
    ``last_hash`` is the hash the next appended record must chain from.
    """
    contents = DecisionLogContents()
    parts, tail = _split(data)
    offset = 0
    for index, raw_line in enumerate(parts):
        if raw_line.strip():
            try:
                parsed = json.loads(raw_line.decode("utf-8"))
                if _is_envelope(parsed):
                    record = DecisionRecord.model_validate(parsed["record"])
                    digest = str(parsed["hash"])
                else:
                    record = DecisionRecord.model_validate(parsed)
                    # Legacy line: fold it into the running hash (see the module docstring).
                    digest = _chain_hash(contents.last_hash, parsed)
                    contents.legacy += 1
            except (ValueError, UnicodeDecodeError) as exc:
                if index == len(parts) - 1 and not tail.strip():
                    contents.torn_tail = raw_line.decode("utf-8", errors="replace")
                    return contents
                raise DecisionLogCorruptError(
                    f"{source}: unreadable record on line {index + 1}: {exc}"
                ) from exc
            contents.records.append(record)
            contents.last_hash = digest
        offset += len(raw_line) + 1
        contents.intact_bytes = offset
    if tail.strip():
        contents.torn_tail = tail.decode("utf-8", errors="replace")
    return contents


def verify_chain(data: bytes, *, source: str = "decision log") -> ChainVerification:
    """Recompute the hash chain over the intact lines of ``data``.

    Fails on an unreadable line, a legacy line after the chain began, a ``prev`` mismatch or a
    hash mismatch. A torn final line is reported (``torn_tail``) but is not itself a failure.
    """
    try:
        contents = parse_chain(data, source=source)
    except DecisionLogCorruptError as exc:
        return ChainVerification(ok=False, checked=0, problem=str(exc))
    torn = contents.torn_tail is not None
    prev = GENESIS_HASH
    checked = 0
    legacy = 0
    chained = False
    problem = ""
    for number, raw in enumerate(data[: contents.intact_bytes].split(b"\n"), start=1):
        if not raw.strip():
            continue
        parsed = json.loads(raw.decode("utf-8"))  # parse_chain already proved it parses
        if not _is_envelope(parsed):
            if chained:
                problem = f"line {number}: unchained record after the chain began"
                break
            prev = _chain_hash(prev, parsed)
            legacy += 1
            checked += 1
            continue
        chained = True
        if parsed.get("prev") != prev:
            problem = f"line {number}: prev-hash mismatch"
            break
        if (
            not isinstance(parsed["record"], dict)
            or _chain_hash(prev, parsed["record"]) != (parsed["hash"])
        ):
            problem = f"line {number}: hash mismatch"
            break
        prev = str(parsed["hash"])
        checked += 1
    return ChainVerification(
        ok=not problem, checked=checked, problem=problem, torn_tail=torn, legacy=legacy
    )


class DecisionLog:
    """A hash-chained JSONL file of ``DecisionRecord``s. Append-only; never rewritten."""

    def __init__(self, path: str) -> None:
        self._path = Path(path)

    def _bytes(self) -> bytes:
        return self._path.read_bytes() if self._path.exists() else b""

    def append(self, record: DecisionRecord) -> str:
        """Durably append ``record``; returns its chain hash."""
        self._path.parent.mkdir(parents=True, exist_ok=True)
        contents = self.load()
        if contents.torn_tail is not None:
            # Drop the partial line left by a crashed write so the chain stays well-formed.
            with self._path.open("r+b") as fh:
                fh.truncate(contents.intact_bytes)
                fh.flush()
                os.fsync(fh.fileno())
        line, digest = encode_link(contents.last_hash, record)
        with self._path.open("a", encoding="utf-8") as fh:
            fh.write(line + "\n")
            fh.flush()
            os.fsync(fh.fileno())
        return digest

    def load(self) -> DecisionLogContents:
        """Parse the log, skipping (and reporting) a torn final line."""
        return parse_chain(self._bytes(), source=str(self._path))

    def read(self) -> list[DecisionRecord]:
        """All intact records. A torn final line is skipped and logged (``load`` exposes it)."""
        contents = self.load()
        if contents.torn_tail is not None:
            structlog.get_logger("lha").warning(
                "decision_log_torn_tail", path=str(self._path), torn=contents.torn_tail[:200]
            )
        return contents.records

    def verify(self) -> ChainVerification:
        """Recompute the hash chain; any edit, reorder, deletion or inserted line fails it."""
        if not self._path.exists():
            return ChainVerification(ok=True, checked=0)
        return verify_chain(self._bytes(), source=str(self._path))
