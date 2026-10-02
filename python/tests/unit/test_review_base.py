"""A review covers the item's whole streak (docs/11-multi-agent-organization.md)."""

from __future__ import annotations

from lha.agents.waves import review_base
from lha.contracts.state import EventRecord


def _review(item: str, blocking: bool, base: str, cycle: str) -> EventRecord:
    return EventRecord(
        kind="review", cycle_id=cycle, payload={"item_id": item, "blocking": blocking, "base": base}
    )


def test_review_base_tracks_the_first_blocking_review_until_an_approval() -> None:
    assert review_base([], "01", "fb") == "fb"
    events = [_review("01", True, "b1", "c1")]
    assert review_base(events, "01", "fb") == "b1"  # the first attempt's base, not the fallback
    events.append(_review("01", True, "b2", "c2"))
    assert review_base(events, "01", "fb") == "b1"  # still the first
    events.append(_review("01", False, "b1", "c3"))
    assert review_base(events, "01", "fb") == "fb"  # approved: the next streak starts afresh
    events.append(_review("02", True, "x", "c4"))
    assert review_base(events, "01", "fb") == "fb"  # another item's reviews do not count
    assert review_base(events, "02", "fb") == "x"
