"""``GitMissionAnchor`` — the durable source of truth, implemented on git.

Anchor files live under ``.lha/`` inside the target repo and are committed alongside the agent's
code changes, so every checkpoint is one atomic commit capturing *both* the work and the
progress. ``read_situational_awareness`` reconstructs the full picture from these files + git log
in milliseconds, which is what lets any crash/reboot/compaction resume coherently.

Harness truth: the agent works in the same directory, so it *could* edit ``.lha/``. Reads
therefore come from the committed ``HEAD`` (not the working tree), and ``commit_checkpoint``
restores ``.lha/`` to ``HEAD`` before rewriting it from the harness's own in-memory state — any
agent edits to anchor files are discarded, never committed.

Layout (inside the worked repo):
    .lha/mission.json       — the immutable MissionSpec (title/description), written at init
    .lha/checklist.json     — the machine-readable Checklist
    .lha/progress.md        — human-readable progress narrative (appended, size-bounded)
    .lha/decisions.ndjson   — append-only DecisionRecord log (never compacted)
    .lha/events.ndjson      — append-only EventRecord (episodic) log
"""

from __future__ import annotations

import asyncio
import subprocess
from pathlib import Path

from lha.contracts.state import (
    Checklist,
    Checkpoint,
    DecisionRecord,
    EventRecord,
    MissionSpec,
    SituationSnapshot,
)
from lha.state import git_ops

ANCHOR_DIR = ".lha"
MISSION_FILE = "mission.json"
CHECKLIST_FILE = "checklist.json"
PROGRESS_FILE = "progress.md"
DECISIONS_FILE = "decisions.ndjson"
EVENTS_FILE = "events.ndjson"

# progress.md is appended every cycle; keep it bounded (oldest entries are trimmed first).
MAX_PROGRESS_CHARS = 16_000
_PROGRESS_MARKER = "## Progress\n\n"


class GitMissionAnchor:
    """A ``DurableState`` backed by a git repo at ``workdir``."""

    def __init__(
        self, workdir: str | Path, *, max_progress_chars: int = MAX_PROGRESS_CHARS
    ) -> None:
        self.workdir = Path(workdir)
        self.anchor = self.workdir / ANCHOR_DIR
        self._max_progress = max_progress_chars
        # Events appended (uncommitted) via ``append_event``; re-applied after the ``.lha``
        # restore in ``commit_checkpoint`` so they are not lost.
        self._pending_events: list[EventRecord] = []

    # --- paths -----------------------------------------------------------------------
    def _path(self, name: str) -> Path:
        return self.anchor / name

    # --- DurableState API ------------------------------------------------------------
    async def initialize(
        self, *, title: str, description: str, items: Checklist, acceptance: str = ""
    ) -> str:
        """Write the immutable mission spec + initial anchor and commit; reject broken plans."""
        errors = items.dependency_errors()
        if errors:
            raise ValueError("invalid checklist: " + "; ".join(errors))
        spec = MissionSpec(title=title, description=description, acceptance=acceptance)
        return await asyncio.to_thread(self._initialize_sync, spec, items)

    async def read_situational_awareness(self) -> SituationSnapshot:
        return await asyncio.to_thread(self._read_sync)

    async def read_checklist(self) -> Checklist:
        """Return the full committed checklist (all items, not just the open ones)."""
        return await asyncio.to_thread(self._read_checklist)

    async def read_mission(self) -> MissionSpec | None:
        """Return the immutable mission spec (``None`` for anchors created before it existed)."""
        return await asyncio.to_thread(self._read_mission)

    async def append_event(self, event: EventRecord) -> None:
        await asyncio.to_thread(self._append_event_sync, event)

    async def commit_checkpoint(self, checkpoint: Checkpoint) -> str:
        return await asyncio.to_thread(self._commit_sync, checkpoint)

    async def restore_from_head(self, relpaths: list[str]) -> list[str]:
        """Restore tracked ``relpaths`` to their ``HEAD`` content; return the ones restored."""
        return await asyncio.to_thread(self._restore_sync, relpaths)

    # --- sync implementations (run in a thread) --------------------------------------
    def _initialize_sync(self, spec: MissionSpec, items: Checklist) -> str:
        git_ops.init_repo(self.workdir)
        self.anchor.mkdir(parents=True, exist_ok=True)
        self._path(MISSION_FILE).write_text(spec.model_dump_json(indent=2), encoding="utf-8")
        self._write_checklist(items)
        self._path(PROGRESS_FILE).write_text(
            f"# Mission: {spec.title}\n\n{spec.description}\n\n"
            f"{_PROGRESS_MARKER}- _initialized; no work yet._\n",
            encoding="utf-8",
        )
        # Create the append-only logs (empty).
        self._path(DECISIONS_FILE).write_text("", encoding="utf-8")
        self._path(EVENTS_FILE).write_text("", encoding="utf-8")
        self._pending_events.clear()
        return git_ops.commit_all(self.workdir, "lha: initialize mission anchor")

    def _read_sync(self) -> SituationSnapshot:
        checklist = self._read_checklist()
        return SituationSnapshot(
            head_sha=git_ops.head_sha(self.workdir),
            recent_commits=git_ops.log_oneline(self.workdir, 10),
            mission=self._read_mission(),
            progress_summary=self._read_anchor_file(PROGRESS_FILE) or "",
            open_items=[i for i in checklist.items if i.is_open],
            last_decisions=self._read_recent_decisions(5),
            active_item=checklist.next_actionable(),
            is_complete=checklist.is_complete,
            is_deadlocked=checklist.is_deadlocked,
            deadlock_reason=checklist.deadlock_reason(),
            items_done=checklist.items_done,
            items_total=len(checklist.items),
        )

    def _append_event_sync(self, event: EventRecord) -> None:
        self.anchor.mkdir(parents=True, exist_ok=True)
        with self._path(EVENTS_FILE).open("a", encoding="utf-8") as fh:
            fh.write(event.model_dump_json() + "\n")
        self._pending_events.append(event)

    def _commit_sync(self, checkpoint: Checkpoint) -> str:
        # Harness truth: discard whatever the agent did to .lha/ during the cycle.
        self._restore_anchor_dir()
        self.anchor.mkdir(parents=True, exist_ok=True)
        self._write_checklist(checkpoint.checklist)
        if checkpoint.progress_summary.strip():
            self._append_progress(checkpoint.progress_summary)
        if checkpoint.decisions:
            with self._path(DECISIONS_FILE).open("a", encoding="utf-8") as fh:
                for decision in checkpoint.decisions:
                    fh.write(decision.model_dump_json() + "\n")
        events = [*self._pending_events, *checkpoint.events]
        if events:
            with self._path(EVENTS_FILE).open("a", encoding="utf-8") as fh:
                for event in events:
                    fh.write(event.model_dump_json() + "\n")
        message = checkpoint.commit_message or f"lha: checkpoint {checkpoint.cycle_id}"
        sha = git_ops.commit_all(self.workdir, message)
        self._pending_events.clear()
        return sha

    def _restore_sync(self, relpaths: list[str]) -> list[str]:
        restored: list[str] = []
        for rel in relpaths:
            if self._tracked_at_head(rel):
                git_ops.run_git(self.workdir, "checkout", "HEAD", "--", rel)
                restored.append(rel)
        return restored

    # --- helpers ---------------------------------------------------------------------
    def _restore_anchor_dir(self) -> None:
        """Reset ``.lha/`` to ``HEAD`` (tracked files restored, untracked additions removed)."""
        if not self._tracked_at_head(ANCHOR_DIR):
            return
        git_ops.run_git(self.workdir, "checkout", "HEAD", "--", ANCHOR_DIR)
        git_ops.run_git(self.workdir, "clean", "-fdq", "--", ANCHOR_DIR)

    def _tracked_at_head(self, relpath: str) -> bool:
        if not git_ops.has_commits(self.workdir):
            return False
        proc = subprocess.run(
            ["git", "cat-file", "-e", f"HEAD:{relpath}"],
            cwd=str(self.workdir),
            capture_output=True,
            text=True,
        )
        return proc.returncode == 0

    def _read_anchor_file(self, name: str) -> str | None:
        """Read an anchor file from ``HEAD`` (committed truth); fall back to the working tree."""
        rel = f"{ANCHOR_DIR}/{name}"
        if self._tracked_at_head(rel):
            return git_ops.run_git(self.workdir, "show", f"HEAD:{rel}")
        path = self._path(name)
        return path.read_text(encoding="utf-8") if path.exists() else None

    def _append_progress(self, entry: str) -> None:
        path = self._path(PROGRESS_FILE)
        current = path.read_text(encoding="utf-8") if path.exists() else _PROGRESS_MARKER
        if not current.endswith("\n"):
            current += "\n"
        current += entry.strip() + "\n"
        path.write_text(_bound_progress(current, self._max_progress), encoding="utf-8")

    def _write_checklist(self, checklist: Checklist) -> None:
        self._path(CHECKLIST_FILE).write_text(checklist.model_dump_json(indent=2), encoding="utf-8")

    def _read_checklist(self) -> Checklist:
        raw = self._read_anchor_file(CHECKLIST_FILE)
        return Checklist() if raw is None else Checklist.model_validate_json(raw)

    def _read_mission(self) -> MissionSpec | None:
        raw = self._read_anchor_file(MISSION_FILE)
        return None if raw is None else MissionSpec.model_validate_json(raw)

    def _read_recent_decisions(self, n: int) -> list[DecisionRecord]:
        raw = self._read_anchor_file(DECISIONS_FILE)
        if not raw:
            return []
        lines = [ln for ln in raw.splitlines() if ln.strip()]
        return [DecisionRecord.model_validate_json(ln) for ln in lines[-n:]]


def _bound_progress(text: str, limit: int) -> str:
    """Trim the OLDEST progress entries so ``text`` fits in ``limit`` chars (header is kept)."""
    if len(text) <= limit:
        return text
    marker_at = text.find(_PROGRESS_MARKER)
    split = marker_at + len(_PROGRESS_MARKER) if marker_at != -1 else 0
    header, body = text[:split], text[split:]
    note = "- _(older entries trimmed)_\n"
    lines = [ln for ln in body.splitlines(keepends=True) if ln != note]
    size = len(header) + len(note) + sum(len(ln) for ln in lines)
    while lines and size > limit:
        size -= len(lines.pop(0))
    return header + note + "".join(lines)
