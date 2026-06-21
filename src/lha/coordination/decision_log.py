"""Append-only decision log — the never-compacted record of implicit design decisions.

Compaction drops implicit action-level decisions (the dangerous ones). Writing them here, append
-only and exempt from compaction, lets later work be checked against them for contradictions.

Durability + integrity:
- every append is flushed and ``fsync``-ed before returning;
- each line is an envelope ``{"prev": <hash>, "hash": <hash>, "record": {...}}`` forming a
  SHA-256 hash chain, so ``verify()`` detects edited, reordered or deleted records;
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


@dataclass
class DecisionLogContents:
    """What ``DecisionLog.load`` found on disk."""

    records: list[DecisionRecord] = field(default_factory=list)
    last_hash: str = GENESIS_HASH
    torn_tail: str | None = None  # the raw torn final line, if one was skipped
    intact_bytes: int = 0  # length of the file prefix holding only intact lines


@dataclass
class ChainVerification:
    """Result of ``DecisionLog.verify``."""

    ok: bool
    checked: int
    problem: str = ""
    torn_tail: bool = False


def _canonical(record: dict[str, Any]) -> str:
    return json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _chain_hash(prev: str, record: dict[str, Any]) -> str:
    return hashlib.sha256(f"{prev}\n{_canonical(record)}".encode()).hexdigest()


class DecisionLog:
    """A hash-chained JSONL file of ``DecisionRecord``s. Append-only; never rewritten."""

    def __init__(self, path: str) -> None:
        self._path = Path(path)

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
        payload = record.model_dump(mode="json")
        digest = _chain_hash(contents.last_hash, payload)
        line = json.dumps(
            {"prev": contents.last_hash, "hash": digest, "record": payload}, ensure_ascii=False
        )
        with self._path.open("a", encoding="utf-8") as fh:
            fh.write(line + "\n")
            fh.flush()
            os.fsync(fh.fileno())
        return digest

    def load(self) -> DecisionLogContents:
        """Parse the log, skipping (and reporting) a torn final line."""
        contents = DecisionLogContents()
        if not self._path.exists():
            return contents
        parts = self._path.read_bytes().split(b"\n")
        # A well-formed file ends with "\n", so the final element is empty; anything else there
        # is a line whose write never completed (torn), even if it happens to parse.
        tail = parts.pop()
        offset = 0
        for index, raw_line in enumerate(parts):
            if raw_line.strip():
                try:
                    record, digest = self._parse_line(json.loads(raw_line.decode("utf-8")))
                except (ValueError, UnicodeDecodeError) as exc:
                    if index == len(parts) - 1 and not tail.strip():
                        contents.torn_tail = raw_line.decode("utf-8", errors="replace")
                        return contents
                    raise DecisionLogCorruptError(
                        f"{self._path}: unreadable record on line {index + 1}: {exc}"
                    ) from exc
                contents.records.append(record)
                if digest is not None:
                    contents.last_hash = digest
            offset += len(raw_line) + 1
            contents.intact_bytes = offset
        if tail.strip():
            contents.torn_tail = tail.decode("utf-8", errors="replace")
        return contents

    @staticmethod
    def _parse_line(parsed: Any) -> tuple[DecisionRecord, str | None]:
        if isinstance(parsed, dict) and "record" in parsed and "hash" in parsed:
            return DecisionRecord.model_validate(parsed["record"]), str(parsed["hash"])
        # Legacy (pre-chain) line: a bare DecisionRecord.
        return DecisionRecord.model_validate(parsed), None

    def read(self) -> list[DecisionRecord]:
        """All intact records. A torn final line is skipped and logged (``load`` exposes it)."""
        contents = self.load()
        if contents.torn_tail is not None:
            structlog.get_logger("lha").warning(
                "decision_log_torn_tail", path=str(self._path), torn=contents.torn_tail[:200]
            )
        return contents.records

    def verify(self) -> ChainVerification:
        """Recompute the hash chain; any edit, reorder, deletion or unchained line fails it."""
        if not self._path.exists():
            return ChainVerification(ok=True, checked=0)
        try:
            contents = self.load()
        except DecisionLogCorruptError as exc:
            return ChainVerification(ok=False, checked=0, problem=str(exc))
        torn = contents.torn_tail is not None
        prev = GENESIS_HASH
        checked = 0
        # Split ONLY on "\n" (the record separator): ``str.splitlines`` also breaks on U+2028,
        # U+2029, \x85 etc., which ``json.dumps(ensure_ascii=False)`` leaves unescaped in strings.
        raw_lines = self._path.read_bytes()[: contents.intact_bytes].split(b"\n")
        for number, raw in enumerate(raw_lines, start=1):
            if not raw.strip():
                continue
            try:
                envelope = json.loads(raw.decode("utf-8"))
            except (ValueError, UnicodeDecodeError) as exc:
                return ChainVerification(
                    ok=False,
                    checked=checked,
                    problem=f"line {number}: unreadable record: {exc}",
                    torn_tail=torn,
                )
            if not (isinstance(envelope, dict) and "hash" in envelope and "record" in envelope):
                return ChainVerification(
                    ok=False,
                    checked=checked,
                    problem=f"line {number}: unchained record",
                    torn_tail=torn,
                )
            if envelope.get("prev") != prev:
                return ChainVerification(
                    ok=False,
                    checked=checked,
                    problem=f"line {number}: prev-hash mismatch",
                    torn_tail=torn,
                )
            if not isinstance(envelope["record"], dict) or (
                _chain_hash(prev, envelope["record"]) != envelope["hash"]
            ):
                return ChainVerification(
                    ok=False,
                    checked=checked,
                    problem=f"line {number}: hash mismatch",
                    torn_tail=torn,
                )
            prev = str(envelope["hash"])
            checked += 1
        return ChainVerification(ok=True, checked=checked, torn_tail=torn)
