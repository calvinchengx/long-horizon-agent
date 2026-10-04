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
    .lha/decisions.ndjson   — append-only, SHA-256 hash-chained DecisionRecord log (never
                              compacted; line format in ``lha.coordination.decision_log``)
    .lha/events.ndjson      — append-only EventRecord (episodic) log
    .lha/ownership.json     — the FileOwnershipMap (only when the mission declared one)

Decision-chain integrity: the committed ``decisions.ndjson`` is verified on every snapshot read
and before every checkpoint appends to it; a log whose chain does not verify raises
``DecisionChainError`` and the run paths stop the mission rather than build on a rewritten
history. Anchors written before the chain existed (bare ``DecisionRecord`` lines) still load:
those lines form a legacy prefix that the first chained record seals.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

from lha.contracts.state import (
    Checklist,
    Checkpoint,
    DecisionRecord,
    EventRecord,
    MissionSpec,
    SituationSnapshot,
)
from lha.coordination.decision_log import (
    ChainVerification,
    DecisionChainError,
    DecisionLogContents,
    encode_link,
    parse_chain,
    verify_chain,
)
from lha.coordination.ownership import FileOwnershipMap
from lha.state import git_ops

ANCHOR_DIR = ".lha"
MISSION_FILE = "mission.json"
CHECKLIST_FILE = "checklist.json"
PROGRESS_FILE = "progress.md"
DECISIONS_FILE = "decisions.ndjson"
EVENTS_FILE = "events.ndjson"
#: The event an initialization with a mission id writes (who this anchor belongs to in the store).
MISSION_EVENT = "mission"
OWNERSHIP_FILE = "ownership.json"
# The harness-owned anchor files. They are force-added on every commit (when present) so a target
# repo whose ``.gitignore`` excludes ``.lha/`` still gets them committed (otherwise reads would
# silently fall back to the agent-editable working tree).
ANCHOR_FILES = (
    MISSION_FILE,
    CHECKLIST_FILE,
    PROGRESS_FILE,
    DECISIONS_FILE,
    EVENTS_FILE,
    OWNERSHIP_FILE,
)

# progress.md is appended every cycle; keep it bounded (oldest entries are trimmed first).
MAX_PROGRESS_CHARS = 16_000
_PROGRESS_MARKER = "## Progress\n\n"
# How many of the newest decisions the situational snapshot (and so the prompt) carries.
RECENT_DECISIONS = 5


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
        # Decisions recorded mid-cycle (the ``record_decision`` tool) and an ownership map staged
        # by the orchestrator. Both are held in memory — never in the agent-editable working
        # tree — and written by the next ``commit_checkpoint``.
        self._pending_decisions: list[DecisionRecord] = []
        self._pending_ownership: FileOwnershipMap | None = None

    # --- paths -----------------------------------------------------------------------
    def _path(self, name: str) -> Path:
        return self.anchor / name

    # --- DurableState API ------------------------------------------------------------
    async def initialize(
        self,
        *,
        title: str,
        description: str,
        items: Checklist,
        acceptance: str = "",
        references: list[str] | None = None,
        ownership: FileOwnershipMap | None = None,
        mission_id: str = "",
    ) -> str:
        """Write the immutable mission spec + initial anchor and commit; reject broken plans.

        ``ownership``: the Planner's file-ownership map, persisted as ``.lha/ownership.json``
        (``None`` => no ownership file; ``read_ownership`` then returns an empty map).
        ``mission_id``: recorded as a ``mission`` event in the initial commit, so the store's
        rows for this anchor can be found later (``lha mission-report``, ``lha labels export``).
        """
        errors = items.dependency_errors()
        if errors:
            raise ValueError("invalid checklist: " + "; ".join(errors))
        spec = MissionSpec(
            title=title,
            description=description,
            acceptance=acceptance,
            references=list(references or []),
        )
        return await asyncio.to_thread(self._initialize_sync, spec, items, ownership, mission_id)

    async def read_situational_awareness(self) -> SituationSnapshot:
        return await asyncio.to_thread(self._read_sync)

    async def read_checklist(self) -> Checklist:
        """Return the full committed checklist (all items, not just the open ones)."""
        return await asyncio.to_thread(self._read_checklist)

    async def read_mission(self) -> MissionSpec | None:
        """Return the immutable mission spec (``None`` for anchors created before it existed)."""
        return await asyncio.to_thread(self._read_mission)

    async def read_events(self) -> list[EventRecord]:
        """The committed episodic events, oldest first (unreadable lines are skipped)."""
        return await asyncio.to_thread(self._read_events)

    async def append_event(self, event: EventRecord) -> None:
        await asyncio.to_thread(self._append_event_sync, event)

    async def commit_anchor_update(self, checkpoint: Checkpoint) -> str:
        """Like ``commit_checkpoint``, but the commit holds ONLY ``.lha/`` files.

        For harness bookkeeping between cycles (e.g. a lease granted mid-wave): whatever else is
        in the work tree or the index is neither committed nor discarded.
        """
        return await asyncio.to_thread(self._commit_sync, checkpoint, anchor_only=True)

    async def commit_checkpoint(self, checkpoint: Checkpoint) -> str:
        return await asyncio.to_thread(self._commit_sync, checkpoint)

    async def restore_from_head(self, relpaths: list[str]) -> list[str]:
        """Restore tracked ``relpaths`` to their ``HEAD`` content; return the ones restored."""
        return await asyncio.to_thread(self._restore_sync, relpaths)

    # --- decisions -------------------------------------------------------------------
    def record_decision(self, record: DecisionRecord) -> int:
        """Queue ``record`` for the next checkpoint (in memory); return how many are queued.

        The ``record_decision`` tool calls this mid-cycle. The next ``commit_checkpoint`` chains
        the record onto ``decisions.ndjson`` — stamped with the checkpoint's ``cycle_id`` if it
        has none — so it is committed together with the cycle's work.
        """
        self._pending_decisions.append(record)
        return len(self._pending_decisions)

    @property
    def has_pending_records(self) -> bool:
        """Whether events, decisions or an ownership map await the next commit."""
        return bool(self._pending_events or self._pending_decisions) or (
            self._pending_ownership is not None
        )

    @property
    def pending_decisions(self) -> list[DecisionRecord]:
        return list(self._pending_decisions)

    async def read_decisions(self) -> list[DecisionRecord]:
        """Every committed decision, oldest first (verified first: ``DecisionChainError``)."""
        return await asyncio.to_thread(lambda: self._load_decisions().records)

    async def verify_decisions(self) -> ChainVerification:
        """Verify the committed decision chain (never raises; see ``ChainVerification``)."""
        return await asyncio.to_thread(self._verify_decisions)

    # --- ownership -------------------------------------------------------------------
    async def read_ownership(self) -> FileOwnershipMap:
        """The committed file-ownership map (empty if the mission never declared one)."""
        return await asyncio.to_thread(self._read_ownership)

    def stage_ownership(self, ownership: FileOwnershipMap) -> None:
        """Write ``ownership`` to ``.lha/ownership.json`` with the next checkpoint."""
        self._pending_ownership = ownership.model_copy(deep=True)

    # --- sync implementations (run in a thread) --------------------------------------
    def _initialize_sync(
        self,
        spec: MissionSpec,
        items: Checklist,
        ownership: FileOwnershipMap | None,
        mission_id: str = "",
    ) -> str:
        git_ops.init_repo(self.workdir)
        self.anchor.mkdir(parents=True, exist_ok=True)
        self._path(MISSION_FILE).write_text(spec.model_dump_json(indent=2), encoding="utf-8")
        self._write_checklist(items)
        if ownership is not None:
            self._write_ownership(ownership)
        else:  # re-initialized without a map: never inherit a stale one
            self._path(OWNERSHIP_FILE).unlink(missing_ok=True)
        self._path(PROGRESS_FILE).write_text(
            f"# Mission: {spec.title}\n\n{spec.description}\n\n"
            f"{_PROGRESS_MARKER}- _initialized; no work yet._\n",
            encoding="utf-8",
        )
        # Create the append-only logs (empty).
        self._path(DECISIONS_FILE).write_text("", encoding="utf-8")
        started = (
            EventRecord(kind=MISSION_EVENT, payload={"mission_id": mission_id}).model_dump_json()
            + "\n"
            if mission_id
            else ""
        )
        self._path(EVENTS_FILE).write_text(started, encoding="utf-8")
        self._pending_events.clear()
        self._pending_decisions.clear()
        self._pending_ownership = None
        return self._commit_all("lha: initialize mission anchor")

    def _read_sync(self) -> SituationSnapshot:
        checklist = self._read_checklist()
        return SituationSnapshot(
            head_sha=git_ops.head_sha(self.workdir),
            recent_commits=git_ops.log_oneline(self.workdir, 10),
            members=git_ops.member_paths(self.workdir),
            mission=self._read_mission(),
            progress_summary=self._read_anchor_file(PROGRESS_FILE) or "",
            open_items=[i for i in checklist.items if i.is_open],
            last_decisions=self._load_decisions().records[-RECENT_DECISIONS:],
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

    def _commit_sync(self, checkpoint: Checkpoint, *, anchor_only: bool = False) -> str:
        # Verify the committed decision chain BEFORE touching anything: an altered history is
        # never extended (``DecisionChainError``).
        chain = self._load_decisions()
        # Harness truth: discard whatever the agent did to .lha/ during the cycle.
        self._restore_anchor_dir()
        self.anchor.mkdir(parents=True, exist_ok=True)
        self._write_checklist(checkpoint.checklist)
        if checkpoint.progress_summary.strip():
            self._append_progress(checkpoint.progress_summary)
        if self._pending_ownership is not None:
            self._write_ownership(self._pending_ownership)
        # The append-only logs are REBUILT from their committed content + the new records, so
        # the write is idempotent: pending events already appended to the working tree (and not
        # discarded by the restore, e.g. when the file is not yet tracked) are never duplicated.
        decisions = [
            d if d.cycle_id else d.model_copy(update={"cycle_id": checkpoint.cycle_id})
            for d in [*self._pending_decisions, *checkpoint.decisions]
        ]
        self._append_decisions(chain, decisions)
        events = [*self._pending_events, *checkpoint.events]
        self._rebuild_log(EVENTS_FILE, [e.model_dump_json() for e in events])
        message = checkpoint.commit_message or f"lha: checkpoint {checkpoint.cycle_id}"
        sha = self._commit_anchor(message) if anchor_only else self._commit_all(message)
        self._pending_events.clear()
        self._pending_decisions.clear()
        self._pending_ownership = None
        return sha

    def _commit_all(self, message: str) -> str:
        force = tuple(f"{ANCHOR_DIR}/{name}" for name in ANCHOR_FILES)
        return git_ops.commit_all(self.workdir, message, force_paths=force)

    def _commit_anchor(self, message: str) -> str:
        paths = tuple(f"{ANCHOR_DIR}/{name}" for name in ANCHOR_FILES)
        return git_ops.commit_paths(self.workdir, message, paths)

    def _committed_text(self, name: str) -> str:
        """The committed (``HEAD``) content of anchor file ``name``; ``""`` if not committed."""
        rel = f"{ANCHOR_DIR}/{name}"
        return git_ops.show_at_head(self.workdir, rel) if self._tracked_at_head(rel) else ""

    def _rebuild_log(self, name: str, new_lines: list[str]) -> None:
        """Rewrite an append-only log as committed content + ``new_lines`` (idempotent)."""
        base = self._committed_text(name)
        if base and not base.endswith("\n"):
            base += "\n"
        self._path(name).write_text(base + "".join(f"{ln}\n" for ln in new_lines), "utf-8")

    def _append_decisions(self, chain: DecisionLogContents, records: list[DecisionRecord]) -> None:
        """Rewrite ``decisions.ndjson`` as the committed chain + ``records`` chained onto it."""
        prev = chain.last_hash
        lines: list[str] = []
        for record in records:
            line, prev = encode_link(prev, record)
            lines.append(line)
        self._rebuild_log(DECISIONS_FILE, lines)

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
        # ``HEAD:./path`` is resolved relative to the workdir, not the repository root.
        return git_ops.exists_at_head(self.workdir, relpath)

    def _read_anchor_file(self, name: str) -> str | None:
        """Read an anchor file from ``HEAD`` (committed truth); fall back to the working tree."""
        rel = f"{ANCHOR_DIR}/{name}"
        if self._tracked_at_head(rel):
            return git_ops.show_at_head(self.workdir, rel)
        path = self._path(name)
        return path.read_text(encoding="utf-8") if path.exists() else None

    def _append_progress(self, entry: str) -> None:
        path = self._path(PROGRESS_FILE)
        # Committed truth first (never the agent-editable working tree when HEAD has it).
        current = self._read_anchor_file(PROGRESS_FILE) or _PROGRESS_MARKER
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

    def _read_events(self) -> list[EventRecord]:
        out: list[EventRecord] = []
        for line in self._committed_text(EVENTS_FILE).split("\n"):
            if not line.strip():
                continue
            try:
                out.append(EventRecord.model_validate_json(line))
            except ValueError:
                continue
        return out

    def _write_ownership(self, ownership: FileOwnershipMap) -> None:
        self._path(OWNERSHIP_FILE).write_text(ownership.model_dump_json(indent=2), "utf-8")

    def _read_ownership(self) -> FileOwnershipMap:
        raw = self._read_anchor_file(OWNERSHIP_FILE)
        return FileOwnershipMap() if raw is None else FileOwnershipMap.model_validate_json(raw)

    def _decisions_bytes(self) -> bytes:
        """The decision log's exact bytes: ``HEAD`` first, else the (never committed) file."""
        rel = f"{ANCHOR_DIR}/{DECISIONS_FILE}"
        if self._tracked_at_head(rel):
            return git_ops.show_at_head_bytes(self.workdir, rel)
        path = self._path(DECISIONS_FILE)
        return path.read_bytes() if path.exists() else b""

    def _verify_decisions(self) -> ChainVerification:
        check = verify_chain(self._decisions_bytes(), source=f"{ANCHOR_DIR}/{DECISIONS_FILE}")
        if check.ok and check.torn_tail:
            # The harness writes the whole file before committing it, so an incomplete final
            # line in the anchor is never a crash artefact: treat it as an altered log.
            return ChainVerification(
                ok=False,
                checked=check.checked,
                problem="the final line is incomplete (no trailing newline)",
                torn_tail=True,
                legacy=check.legacy,
            )
        return check

    def _load_decisions(self) -> DecisionLogContents:
        """Parse the committed decision chain, verifying it first (``DecisionChainError``)."""
        check = self._verify_decisions()
        if not check.ok:
            raise DecisionChainError(
                f"{ANCHOR_DIR}/{DECISIONS_FILE} in {self.workdir} failed hash-chain verification "
                f"({check.problem}): the committed decision history was altered, so the mission "
                "refuses to continue. Inspect it with `lha decisions --verify` and "
                f"`git log -p -- {ANCHOR_DIR}/{DECISIONS_FILE}`."
            )
        return parse_chain(self._decisions_bytes(), source=f"{ANCHOR_DIR}/{DECISIONS_FILE}")


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
