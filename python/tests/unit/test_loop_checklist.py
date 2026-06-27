"""Checklist semantics: complete vs deadlocked, blocking, dependency validation (L3)."""

from __future__ import annotations

import pytest

from lha.contracts.state import Checklist, ChecklistItem


def _items(*specs: tuple[str, list[str]]) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id=i, description=f"item {i}", depends_on=d) for i, d in specs]
    )


def test_empty_checklist_is_deadlocked_not_complete() -> None:
    checklist = Checklist()
    assert not checklist.is_complete
    assert checklist.is_deadlocked
    assert "no items" in checklist.deadlock_reason()


def test_blocked_item_does_not_block_independent_items() -> None:
    checklist = _items(("01", []), ("02", ["01"]), ("03", []))
    checklist.items[0].status = "blocked"
    nxt = checklist.next_actionable()
    assert nxt is not None and nxt.id == "03"
    checklist.items[2].status = "done"
    assert checklist.next_actionable() is None
    assert checklist.is_deadlocked and not checklist.is_complete
    assert "blocked: 01" in checklist.deadlock_reason()


def test_in_progress_item_is_resumed_first() -> None:
    checklist = _items(("01", []), ("02", []))
    checklist.items[1].status = "in_progress"
    nxt = checklist.next_actionable()
    assert nxt is not None and nxt.id == "02"


def test_dependency_errors_detect_unknown_self_dup_and_cycles() -> None:
    checklist = _items(("01", ["1"]), ("02", ["03"]), ("03", ["02"]), ("04", ["04"]), ("04", []))
    errors = checklist.dependency_errors()
    joined = "\n".join(errors)
    assert "unknown item '1'" in joined
    assert "dependency cycle" in joined
    assert "depends on itself" in joined
    assert "duplicate item id '04'" in joined
    assert checklist.next_actionable() is not None  # the dup "04" with no deps is actionable


def test_unknown_dep_deadlock_is_explained() -> None:
    checklist = _items(("01", ["1"]))
    assert checklist.is_deadlocked
    assert "unknown item '1'" in checklist.deadlock_reason()


def test_record_failure_blocks_after_consecutive_limit_and_unblock_resets() -> None:
    checklist = _items(("01", []))
    checklist.record_failure("01", "red", max_consecutive_failures=2)
    assert checklist.items[0].status == "in_progress"
    checklist.record_failure("01", "red again", max_consecutive_failures=2)
    item = checklist.items[0]
    assert item.status == "blocked" and item.attempts == 2 and item.last_failure == "red again"
    checklist.unblock("01")
    assert item.status == "todo" and item.consecutive_failures == 0


def test_record_success_requires_gating_evidence() -> None:
    checklist = _items(("01", []))
    with pytest.raises(ValueError):
        checklist.record_success("01", [])
    checklist.record_success("01", ["pytest"])
    assert checklist.is_complete and not checklist.is_deadlocked
