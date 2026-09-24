"""Tests for the blackboard, decision log, episodic log, and flaky-quarantine."""

from __future__ import annotations

from pathlib import Path

import pytest

from lha.contracts.state import DecisionRecord, EventRecord
from lha.coordination.blackboard import Blackboard
from lha.coordination.decision_log import DecisionLog
from lha.memory.episodic import InMemoryEpisodicLog
from lha.verify.flaky_quarantine import FlakyQuarantine


def test_blackboard_response_board_separation() -> None:
    board = Blackboard()
    board.post("planner", "main entry")
    board.respond("reviewer", "round response")
    assert [e.content for e in board.read()] == ["main entry"]  # response not yet on main board
    assert [e.content for e in board.read_responses()] == ["round response"]
    board.commit_round()
    assert [e.content for e in board.read()] == ["main entry", "round response"]
    assert board.read_responses() == []


def test_decision_log_append_and_read(tmp_path: Path) -> None:
    log = DecisionLog(str(tmp_path / "decisions.ndjson"))
    log.append(DecisionRecord(decision="use dataclass", rationale="simple"))
    log.append(DecisionRecord(decision="pin embeddings", rationale="avoid drift"))
    assert [r.decision for r in log.read()] == ["use dataclass", "pin embeddings"]


@pytest.mark.asyncio
async def test_episodic_log_append_tail() -> None:
    log = InMemoryEpisodicLog()
    for i in range(5):
        await log.append(EventRecord(kind="e", payload={"i": i}))
    tail = await log.tail(2)
    assert len(tail) == 2
    assert tail[-1].payload["i"] == 4
    assert len(log) == 5


def test_flaky_quarantine_needs_a_pass_and_a_fail_on_one_revision() -> None:
    quarantine = FlakyQuarantine()
    for passed in (True, False, True):  # flake evidence: pass+fail on the same revision
        quarantine.record("flaky_test", revision="abc", passed=passed)
    quarantine.mark_flaky("flaky_test")
    assert quarantine.is_flaky("flaky_test") and not quarantine.is_flaky("solid")
    assert quarantine.counts("flaky_test", "abc") == (2, 1)
