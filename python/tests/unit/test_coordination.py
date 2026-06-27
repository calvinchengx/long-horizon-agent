"""Tests for the ticket lifecycle and the file-ownership map (conflict prevention)."""

from __future__ import annotations

import pytest

from lha.coordination.ownership import LEAD, FileOwnershipMap, is_shared
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus


def test_ticket_transition_is_pure() -> None:
    ticket = Ticket(id="t1", contract=TaskContract(objective="do x"))
    advanced = ticket.transition(TicketStatus.IN_PROGRESS)
    assert advanced.status is TicketStatus.IN_PROGRESS
    assert ticket.status is TicketStatus.CREATED  # original unchanged


def test_shared_files_detected() -> None:
    assert is_shared("pyproject.toml")
    assert is_shared("uv.lock")
    assert is_shared("src/pkg/__init__.py")
    assert is_shared("tests/conftest.py")
    assert is_shared("app/db/migrations/0001_init.py")
    assert not is_shared("src/pkg/feature.py")


def test_ownership_single_writer_per_file() -> None:
    owners = FileOwnershipMap()
    owners.assign("src/pkg/feature_a.py", "impl-1")

    # The assigned writer may write its file; others (and the lead) may not.
    assert owners.permits(writer="impl-1", path="src/pkg/feature_a.py")
    assert not owners.permits(writer="impl-2", path="src/pkg/feature_a.py")
    assert not owners.permits(writer=LEAD, path="src/pkg/feature_a.py")

    # Unassigned, non-shared space belongs to the serial lead.
    assert owners.permits(writer=LEAD, path="src/pkg/other.py")
    assert not owners.permits(writer="impl-1", path="src/pkg/other.py")


def test_shared_files_cannot_be_assigned_to_implementers() -> None:
    owners = FileOwnershipMap()
    with pytest.raises(ValueError, match="lead"):
        owners.assign("pyproject.toml", "impl-1")
    # The lead always owns shared files.
    assert owners.permits(writer=LEAD, path="pyproject.toml")
    assert not owners.permits(writer="impl-1", path="pyproject.toml")


def test_violations_reports_unowned_writes() -> None:
    owners = FileOwnershipMap()
    owners.assign("src/pkg/a.py", "impl-1")
    violations = owners.violations(
        writer="impl-1", paths=["src/pkg/a.py", "src/pkg/b.py", "pyproject.toml"]
    )
    bad_paths = {v.path for v in violations}
    assert bad_paths == {"src/pkg/b.py", "pyproject.toml"}  # a.py is owned by impl-1, allowed
