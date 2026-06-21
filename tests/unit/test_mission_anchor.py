"""Tests for the git mission anchor — the durable source of truth.

These prove the core long-horizon property at the storage layer: state survives outside any
process/window, and a fresh anchor object (simulating a crash + restart) reconstructs the exact
situational awareness from git alone.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from lha.contracts.state import (
    Checklist,
    ChecklistItem,
    Checkpoint,
    DecisionRecord,
    EventRecord,
)
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor


def _checklist() -> Checklist:
    return Checklist(
        items=[
            ChecklistItem(id="01", description="add model"),
            ChecklistItem(id="02", description="add tests", depends_on=["01"]),
        ]
    )


@pytest.mark.asyncio
async def test_initialize_creates_committed_anchor(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    sha = await anchor.initialize(title="T", description="D", items=_checklist())

    assert sha  # a real commit happened
    assert git_ops.has_commits(tmp_path)
    assert (tmp_path / ".lha" / "checklist.json").exists()
    assert (tmp_path / ".lha" / "progress.md").exists()
    log = git_ops.log_oneline(tmp_path)
    assert any("initialize mission anchor" in line for line in log)


@pytest.mark.asyncio
async def test_situational_awareness_picks_next_actionable(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())

    snap = await anchor.read_situational_awareness()
    assert snap.head_sha
    assert len(snap.open_items) == 2
    # 02 depends on 01, so the actionable item must be 01.
    assert snap.active_item is not None
    assert snap.active_item.id == "01"
    assert not snap.is_complete


@pytest.mark.asyncio
async def test_checkpoint_advances_and_survives_restart(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    base = await anchor.initialize(title="T", description="D", items=_checklist())

    # Complete item 01 and checkpoint atomically.
    checklist = _checklist()
    checklist.items[0].status = "done"
    checklist.items[0].verified_by = ["pytest"]
    new_sha = await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="# Progress\n\nDone 01.\n",
            checklist=checklist,
            decisions=[DecisionRecord(decision="use dataclass", rationale="simple", cycle_id="c1")],
            events=[EventRecord(kind="item_done", cycle_id="c1", payload={"id": "01"})],
            commit_message="lha: checkpoint c1 (done 01)",
        )
    )
    assert new_sha and new_sha != base

    # Simulate a crash + restart: a brand-new anchor object reading the same repo.
    reloaded = GitMissionAnchor(tmp_path)
    snap = await reloaded.read_situational_awareness()

    assert snap.head_sha == new_sha
    # 01 is done, so the next actionable is now 02.
    assert snap.active_item is not None
    assert snap.active_item.id == "02"
    assert [i.id for i in snap.open_items] == ["02"]
    assert snap.last_decisions and snap.last_decisions[-1].decision == "use dataclass"


@pytest.mark.asyncio
async def test_completion_detected_when_all_done(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())

    checklist = _checklist()
    for item in checklist.items:
        item.status = "done"
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c2", progress_summary="done", checklist=checklist)
    )

    snap = await GitMissionAnchor(tmp_path).read_situational_awareness()
    assert snap.is_complete
    assert snap.active_item is None
    assert snap.open_items == []


@pytest.mark.asyncio
async def test_append_event_is_idempotent_path(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    await anchor.append_event(EventRecord(kind="started", cycle_id="c1"))
    await anchor.append_event(EventRecord(kind="acted", cycle_id="c1"))

    events_file = tmp_path / ".lha" / "events.ndjson"
    lines = [ln for ln in events_file.read_text(encoding="utf-8").splitlines() if ln.strip()]
    assert len(lines) == 2


@pytest.mark.asyncio
async def test_mission_spec_is_persisted_and_recited(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="Ship X", description="Make X work.", items=_checklist())
    mission = await GitMissionAnchor(tmp_path).read_mission()
    assert mission is not None and mission.title == "Ship X"
    snap = await anchor.read_situational_awareness()
    assert "Ship X" in snap.anchor_text() and "Make X work." in snap.anchor_text()
    assert (snap.items_done, snap.items_total) == (0, 2)


@pytest.mark.asyncio
async def test_progress_is_appended_and_bounded(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path, max_progress_chars=300)
    await anchor.initialize(title="Ship X", description="Make X work.", items=_checklist())
    for n in range(40):
        await anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=f"c{n}", progress_summary=f"- entry {n:02d}", checklist=_checklist()
            )
        )
    progress = (tmp_path / ".lha" / "progress.md").read_text(encoding="utf-8")
    assert len(progress) <= 300
    assert progress.startswith("# Mission: Ship X")  # header (mission) is never trimmed
    assert "entry 39" in progress and "entry 38" in progress  # newest kept
    assert "entry 00" not in progress and "older entries trimmed" in progress


@pytest.mark.asyncio
async def test_commit_discards_agent_edits_to_anchor(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    forged = _checklist()
    for item in forged.items:
        item.status = "done"
    (tmp_path / ".lha" / "checklist.json").write_text(forged.model_dump_json(), encoding="utf-8")
    (tmp_path / ".lha" / "progress.md").write_text("pwned", encoding="utf-8")
    (tmp_path / ".lha" / "extra.txt").write_text("junk", encoding="utf-8")

    # Reads come from HEAD, not the tampered working tree.
    assert not (await anchor.read_checklist()).is_complete

    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1 attempt", checklist=_checklist())
    )
    progress = (tmp_path / ".lha" / "progress.md").read_text(encoding="utf-8")
    assert "pwned" not in progress and "- c1 attempt" in progress
    assert not (tmp_path / ".lha" / "extra.txt").exists()
    assert not (await GitMissionAnchor(tmp_path).read_checklist()).is_complete


@pytest.mark.asyncio
async def test_pending_events_survive_anchor_restore(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    await anchor.append_event(EventRecord(kind="started", cycle_id="c1"))
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=_checklist(),
            events=[EventRecord(kind="cycle", cycle_id="c1")],
        )
    )
    lines = (tmp_path / ".lha" / "events.ndjson").read_text(encoding="utf-8").splitlines()
    assert [EventRecord.model_validate_json(ln).kind for ln in lines if ln] == ["started", "cycle"]


@pytest.mark.asyncio
async def test_initialize_rejects_unreachable_plan(tmp_path: Path) -> None:
    bad = Checklist(items=[ChecklistItem(id="01", description="x", depends_on=["1"])])
    with pytest.raises(ValueError, match="unknown item"):
        await GitMissionAnchor(tmp_path).initialize(title="T", description="D", items=bad)


@pytest.mark.asyncio
async def test_snapshot_reports_deadlock_not_completion(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    blocked = _checklist()
    blocked.items[0].status = "blocked"
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- blocked", checklist=blocked)
    )
    snap = await anchor.read_situational_awareness()
    assert snap.active_item is None
    assert not snap.is_complete and snap.is_deadlocked
    assert "blocked: 01" in snap.deadlock_reason
