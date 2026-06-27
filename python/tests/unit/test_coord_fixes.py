"""Regression tests for coordination fixes: ticket state machine (C5), ownership path
normalization + shared manifests (C6), and the durable hash-chained decision log (C7)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lha.contracts.state import DecisionRecord
from lha.coordination.decision_log import DecisionLog, DecisionLogCorruptError
from lha.coordination.ownership import LEAD, FileOwnershipMap, InvalidPathError, is_shared
from lha.coordination.ticket import IllegalTransitionError, TaskContract, Ticket, TicketStatus

# --- C5 --------------------------------------------------------------------------------------


def _ticket(status: TicketStatus = TicketStatus.CREATED) -> Ticket:
    return Ticket(id="t", contract=TaskContract(objective="x"), status=status)


def test_ticket_happy_path_and_attempts() -> None:
    t = _ticket()
    for to in (
        TicketStatus.IN_PROGRESS,
        TicketStatus.AWAITING_VERIFY,
        TicketStatus.IN_PROGRESS,  # verification failed -> another attempt
        TicketStatus.AWAITING_VERIFY,
        TicketStatus.AWAITING_MERGE,
        TicketStatus.DONE,
    ):
        t = t.transition(to)
    assert t.status is TicketStatus.DONE
    assert t.attempts == 2


@pytest.mark.parametrize(
    ("start", "to"),
    [
        (TicketStatus.CREATED, TicketStatus.DONE),  # skips verification
        (TicketStatus.DONE, TicketStatus.CREATED),  # reopening a terminal ticket
        (TicketStatus.FAILED, TicketStatus.IN_PROGRESS),
        (TicketStatus.IN_PROGRESS, TicketStatus.AWAITING_MERGE),
        (TicketStatus.IN_PROGRESS, TicketStatus.IN_PROGRESS),
    ],
)
def test_illegal_ticket_transitions_raise(start: TicketStatus, to: TicketStatus) -> None:
    with pytest.raises(IllegalTransitionError):
        _ticket(start).transition(to)


# --- C6 --------------------------------------------------------------------------------------


def test_normalization_keeps_leading_dots() -> None:
    owners = FileOwnershipMap()
    owners.assign(".env", "impl-1")
    assert owners.owner_of("./.env") == "impl-1"
    assert owners.owner_of("env") is None  # ".env" and "env" are different files
    assert owners.owner_of(".hidden\\..\\.env") == "impl-1"


def test_dotdot_resolved_and_escapes_rejected() -> None:
    owners = FileOwnershipMap()
    owners.assign("src/a.py", "impl-1")
    assert owners.permits(writer="impl-1", path="src/tmp/../a.py")
    assert not owners.permits(writer="impl-1", path="../outside.py")
    assert not owners.permits(writer=LEAD, path="/etc/passwd")
    with pytest.raises(InvalidPathError):
        owners.assign("../../x.py", "impl-1")
    bad = owners.violations(writer="impl-1", paths=["../x"])
    assert [v.path for v in bad] == ["../x"]


@pytest.mark.parametrize(
    "path",
    [
        "Cargo.toml",
        "PYPROJECT.TOML",
        "services/api/pyproject.toml",
        "crates/core/Cargo.toml",
        "tools/go.mod",
        "package.json",
        "web/package-lock.json",
        "pnpm-lock.yaml",
        "yarn.lock",
        "setup.py",
        "setup.cfg",
        "requirements.txt",
        "requirements-dev.txt",
        "./uv.lock",
        "app/Migrations/0002.py",
    ],
)
def test_shared_manifests_detected(path: str) -> None:
    assert is_shared(path)


def test_shared_manifest_cannot_be_assigned_via_case_trick() -> None:
    owners = FileOwnershipMap()
    with pytest.raises(ValueError, match="lead"):
        owners.assign("Package.JSON", "impl-1")
    assert not is_shared("src/requirements_parser.py")
    assert not is_shared(".env")


# --- C7 --------------------------------------------------------------------------------------


def _record(n: int) -> DecisionRecord:
    return DecisionRecord(decision=f"d{n}", rationale="r")


def test_decision_log_chain_verifies(tmp_path: Path) -> None:
    log = DecisionLog(str(tmp_path / "d.ndjson"))
    for n in range(3):
        log.append(_record(n))
    check = log.verify()
    assert check.ok
    assert check.checked == 3


def test_decision_log_detects_tampering(tmp_path: Path) -> None:
    path = tmp_path / "d.ndjson"
    log = DecisionLog(str(path))
    for n in range(3):
        log.append(_record(n))
    lines = path.read_text().splitlines()
    envelope = json.loads(lines[1])
    envelope["record"]["decision"] = "rewritten history"
    lines[1] = json.dumps(envelope)
    path.write_text("\n".join(lines) + "\n")
    check = log.verify()
    assert not check.ok
    assert "line 2" in check.problem

    # Deleting a record breaks the prev-hash link.
    path.write_text("\n".join([lines[0], lines[2]]) + "\n")
    assert not log.verify().ok


def test_decision_log_tolerates_torn_final_line(tmp_path: Path) -> None:
    path = tmp_path / "d.ndjson"
    log = DecisionLog(str(path))
    log.append(_record(0))
    log.append(_record(1))
    with path.open("a") as fh:
        fh.write('{"prev": "abc", "hash": "de')  # crash mid-write
    assert [r.decision for r in log.read()] == ["d0", "d1"]
    contents = log.load()
    assert contents.torn_tail is not None
    check = log.verify()
    assert check.ok
    assert check.torn_tail

    # The next append drops the torn tail and continues the chain.
    log.append(_record(2))
    assert [r.decision for r in log.read()] == ["d0", "d1", "d2"]
    assert log.load().torn_tail is None
    assert log.verify().ok


def test_decision_log_mid_file_corruption_is_fatal(tmp_path: Path) -> None:
    path = tmp_path / "d.ndjson"
    log = DecisionLog(str(path))
    log.append(_record(0))
    log.append(_record(1))
    lines = path.read_text().splitlines()
    path.write_text("\n".join([lines[0], "{garbage", lines[1]]) + "\n")
    with pytest.raises(DecisionLogCorruptError):
        log.read()
