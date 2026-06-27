"""The durable-state contract — the mission's source of truth, outside the context window.

The context window is a lossy cache. Everything load-bearing lives here instead: a structured
progress file, a machine-readable checklist, a decision log, and an append-only event log — all
committed to git. The agent re-reads this every cycle ("assume interruption"), so a crash,
reboot, or context compaction is a routine no-op: situational awareness is reconstructed in
seconds from ``read_situational_awareness()``.
"""

from __future__ import annotations

from datetime import datetime
from typing import Literal, Protocol, runtime_checkable

from pydantic import BaseModel, Field

ItemStatus = Literal["todo", "in_progress", "blocked", "done", "split"]


class MissionSpec(BaseModel):
    """The immutable mission definition, written once by the harness at initialization.

    This — not the (growing, agent-adjacent) progress narrative — is what every cycle recites as
    its anchor, so the goal survives any number of checkpoints, restarts and compactions.
    """

    title: str
    description: str
    acceptance: str = ""
    # Workspace-relative paths of vendored, read-only reference material (API docs, schemas, SDK
    # behaviour notes) the agent should consult; see ``lha vendor``.
    references: list[str] = Field(default_factory=list)
    schema_version: int = 1

    def render_anchor(self) -> str:
        """The mission recitation placed at the top of every cycle's system prompt."""
        text = f"Mission: {self.title}\n\n{self.description}".rstrip()
        if self.acceptance.strip():
            text += f"\n\nDefinition of done: {self.acceptance.strip()}"
        if self.references:
            listed = "\n".join(f"- {ref}" for ref in self.references)
            text += (
                "\n\nReference material (vendored, read-only; consult it instead of guessing):\n"
                + listed
            )
        return text


class ChecklistItem(BaseModel):
    """One mission work-unit; flips to ``done`` only after deterministic verification.

    Status semantics:
      * ``todo`` / ``in_progress`` — actionable once every dependency is ``done``.
      * ``blocked`` — failed verification ``max_consecutive_failures`` times in a row (or was
        blocked by a human); NOT re-picked until ``Checklist.unblock`` is called. Independent
        items keep making progress.
      * ``done`` — verified by at least one gating check (and every one of its witnesses).
      * ``split`` — was blocked and has been replaced by smaller child items (``<id>.1``,
        ``<id>.2``, ...); resolved when they are. Never counted as done.

    ``witnesses`` name the item's own acceptance checks (``go:TestX``, ``pytest:<node>``,
    ``cmd:<shell>``, ``trusted:<name>``; see ``lha.verify.witnesses``). They gate the item in
    addition to the mission's checks, so "Livy on real Spark" cannot pass on unit tests alone.
    """

    id: str
    description: str
    status: ItemStatus = "todo"
    # IDs of the checks (see contracts.verify.Check) that proved this item done.
    verified_by: list[str] = Field(default_factory=list)
    depends_on: list[str] = Field(default_factory=list)
    attempts: int = 0
    # Verification failures since the last success/unblock (drives auto-blocking).
    consecutive_failures: int = 0
    # Harness-written explanation of the most recent failed verification (shown to the agent).
    last_failure: str = ""
    # Opt-in: this item legitimately needs to modify pre-existing tests / test config.
    allow_harness_edits: bool = False
    witnesses: list[str] = Field(default_factory=list)
    notes: str = ""
    schema_version: int = 1

    @property
    def is_open(self) -> bool:
        return self.status in ("todo", "in_progress", "blocked")

    @property
    def is_actionable_status(self) -> bool:
        return self.status in ("todo", "in_progress")


class Checklist(BaseModel):
    """The ordered set of mission items.

    ``is_complete`` and ``is_deadlocked`` are the two terminal predicates; "no actionable item"
    alone must never be read as "complete".
    """

    items: list[ChecklistItem] = Field(default_factory=list)
    schema_version: int = 1

    def get(self, item_id: str) -> ChecklistItem | None:
        return next((i for i in self.items if i.id == item_id), None)

    def next_actionable(self) -> ChecklistItem | None:
        """The next item to work: an ``in_progress`` one first, else the first ready ``todo``.

        An item is ready when its status is actionable and all its dependencies are ``done``.
        ``blocked`` items are skipped, so they don't stall independent work.
        """
        done = {i.id for i in self.items if i.status == "done"}
        ready = [
            i
            for i in self.items
            if i.is_actionable_status and all(dep in done for dep in i.depends_on)
        ]
        in_progress = next((i for i in ready if i.status == "in_progress"), None)
        return in_progress or (ready[0] if ready else None)

    @property
    def all_done(self) -> bool:
        """Every item is done or was split into children that are (at least one item done)."""
        return any(i.status == "done" for i in self.items) and all(
            i.status in ("done", "split") for i in self.items
        )

    @property
    def is_complete(self) -> bool:
        """Every item is verified done (an empty checklist is NOT complete)."""
        return self.all_done

    @property
    def is_deadlocked(self) -> bool:
        """Not complete, yet nothing is actionable (blocked items, unsatisfiable deps, cycles)."""
        return not self.all_done and self.next_actionable() is None

    @property
    def items_done(self) -> int:
        return sum(1 for i in self.items if i.status == "done")

    @property
    def items_total(self) -> int:
        """Work items: split parents are replaced by their children, so they don't count."""
        return sum(1 for i in self.items if i.status != "split")

    @property
    def blocked_items(self) -> list[ChecklistItem]:
        return [i for i in self.items if i.status == "blocked"]

    def dependency_errors(self) -> list[str]:
        """Structural problems that would make items unreachable: dup ids, unknown deps, cycles."""
        errors: list[str] = []
        ids = [i.id for i in self.items]
        seen: set[str] = set()
        for item_id in ids:
            if item_id in seen:
                errors.append(f"duplicate item id {item_id!r}")
            seen.add(item_id)
        graph: dict[str, list[str]] = {}
        for item in self.items:
            for dep in item.depends_on:
                if dep == item.id:
                    errors.append(f"item {item.id!r} depends on itself")
                elif dep not in seen:
                    errors.append(f"item {item.id!r} depends on unknown item {dep!r}")
            graph[item.id] = [d for d in item.depends_on if d in seen and d != item.id]

        # Cycle detection (iterative DFS, white/grey/black colouring).
        state: dict[str, int] = {}
        for root in graph:
            if state.get(root):
                continue
            stack: list[tuple[str, int]] = [(root, 0)]
            path: list[str] = []
            while stack:
                node, idx = stack.pop()
                if idx == 0:
                    state[node] = 1
                    path.append(node)
                deps = graph.get(node, [])
                if idx < len(deps):
                    stack.append((node, idx + 1))
                    nxt = deps[idx]
                    if state.get(nxt) == 1:
                        cycle = [*path[path.index(nxt) :], nxt]
                        errors.append("dependency cycle: " + " -> ".join(cycle))
                    elif not state.get(nxt):
                        stack.append((nxt, 0))
                else:
                    state[node] = 2
                    path.pop()
        return errors

    def deadlock_reason(self) -> str:
        """Human-readable explanation of why no item is actionable ('' if not deadlocked)."""
        if not self.is_deadlocked:
            return ""
        if not self.items:
            return "checklist has no items"
        reasons: list[str] = []
        blocked = self.blocked_items
        if blocked:
            reasons.append("blocked: " + ", ".join(i.id for i in blocked))
        reasons.extend(self.dependency_errors())
        if not reasons:
            reasons.append("open items wait on dependencies that can never complete")
        return "; ".join(reasons)

    # --- harness-owned state transitions (the agent never edits the checklist) -------------
    def start(self, item_id: str) -> ChecklistItem:
        """Mark ``item_id`` as being worked this cycle."""
        item = self._require(item_id)
        item.status = "in_progress"
        return item

    def record_success(self, item_id: str, verified_by: list[str]) -> ChecklistItem:
        """Mark ``item_id`` done, recording the gating checks that proved it."""
        item = self._require(item_id)
        if not verified_by:
            raise ValueError("an item can only be marked done by at least one gating check")
        item.attempts += 1
        item.status = "done"
        item.verified_by = list(verified_by)
        item.consecutive_failures = 0
        item.last_failure = ""
        return item

    def record_failure(
        self, item_id: str, reason: str, *, max_consecutive_failures: int
    ) -> ChecklistItem:
        """Record a failed attempt; block the item after ``max_consecutive_failures`` in a row."""
        item = self._require(item_id)
        item.attempts += 1
        item.consecutive_failures += 1
        item.last_failure = reason
        if max_consecutive_failures > 0 and item.consecutive_failures >= max_consecutive_failures:
            item.status = "blocked"
        else:
            item.status = "in_progress"
        return item

    def unblock(self, item_id: str) -> ChecklistItem:
        """Human/operator action: make a blocked item actionable again with a fresh budget."""
        item = self._require(item_id)
        if item.status == "blocked":
            item.status = "todo"
        item.consecutive_failures = 0
        return item

    def split(self, item_id: str, drafts: list[ChecklistItem]) -> list[ChecklistItem]:
        """Replace an unfinished item with ordered child items ``<id>.1``, ``<id>.2``, ...

        Only each draft's ``description``, ``witnesses`` and ``allow_harness_edits`` are used; ids
        and dependencies are assigned here: the first child inherits the parent's dependencies,
        each later child depends on the one before it, and the parent's witnesses move to the LAST
        child (so the original acceptance still gates the result). Items that depended on the
        parent now depend on the last child. The parent becomes ``split`` (never ``done``).
        """
        parent = self._require(item_id)
        if parent.status not in ("todo", "in_progress", "blocked"):
            raise ValueError(f"item {item_id!r} is {parent.status}; only open items can be split")
        if len(drafts) < 2:
            raise ValueError("a split needs at least two child items")
        ids = [f"{parent.id}.{n}" for n in range(1, len(drafts) + 1)]
        clash = [i for i in ids if self.get(i) is not None]
        if clash:
            raise ValueError(f"child ids already exist: {clash}")
        children: list[ChecklistItem] = []
        for n, (child_id, draft) in enumerate(zip(ids, drafts, strict=True)):
            witnesses = list(draft.witnesses)
            if n == len(drafts) - 1:
                witnesses += [w for w in parent.witnesses if w not in witnesses]
            children.append(
                ChecklistItem(
                    id=child_id,
                    description=draft.description,
                    depends_on=list(parent.depends_on) if n == 0 else [ids[n - 1]],
                    witnesses=witnesses,
                    allow_harness_edits=draft.allow_harness_edits or parent.allow_harness_edits,
                    notes=f"split from {parent.id}",
                )
            )
        for other in self.items:
            if parent.id in other.depends_on:
                other.depends_on = [ids[-1] if d == parent.id else d for d in other.depends_on]
        parent.status = "split"
        note = f"split into {', '.join(ids)}"
        parent.notes = f"{parent.notes}; {note}" if parent.notes else note
        at = self.items.index(parent) + 1
        self.items[at:at] = children
        return children

    def _require(self, item_id: str) -> ChecklistItem:
        item = self.get(item_id)
        if item is None:
            raise KeyError(f"no checklist item {item_id!r}")
        return item


class DecisionRecord(BaseModel):
    """A durable, never-compacted record of an implicit design decision (anti-drift).

    Compaction drops implicit action-level decisions — the dangerous ones. Externalizing them
    here means later work can be checked against them for contradictions.
    """

    decision: str
    rationale: str
    alternatives_rejected: str = ""
    affected: list[str] = Field(default_factory=list)
    cycle_id: str = ""


class EventRecord(BaseModel):
    """One entry in the append-only episodic event log."""

    kind: str
    cycle_id: str = ""
    payload: dict[str, object] = Field(default_factory=dict)
    # Pointer to a large blob in the object store (claim-check), to keep the log compact.
    payload_ref: str | None = None


class SituationSnapshot(BaseModel):
    """Reconstructed situational awareness: what a fresh agent needs to resume coherently.

    ``active_item is None`` does NOT mean complete: check ``is_complete`` / ``is_deadlocked``.
    """

    head_sha: str
    recent_commits: list[str] = Field(default_factory=list)
    mission: MissionSpec | None = None
    progress_summary: str = ""
    open_items: list[ChecklistItem] = Field(default_factory=list)
    last_decisions: list[DecisionRecord] = Field(default_factory=list)
    active_item: ChecklistItem | None = None
    is_complete: bool = False
    is_deadlocked: bool = False
    deadlock_reason: str = ""
    items_done: int = 0
    items_total: int = 0

    def anchor_text(self) -> str:
        """The mission anchor to recite: the immutable spec (falls back to the progress file)."""
        if self.mission is not None:
            return self.mission.render_anchor()
        return self.progress_summary or "Mission (spec unavailable)"


class Checkpoint(BaseModel):
    """The inputs to one atomic checkpoint commit."""

    cycle_id: str
    # One progress ENTRY, appended (bounded) to ``progress.md`` — never a replacement.
    progress_summary: str
    checklist: Checklist
    decisions: list[DecisionRecord] = Field(default_factory=list)
    events: list[EventRecord] = Field(default_factory=list)
    commit_message: str = ""
    committed_at: datetime | None = None


@runtime_checkable
class DurableState(Protocol):
    """Read/write the mission's durable source of truth. Impl: ``src/lha/state/``."""

    async def initialize(self, *, title: str, description: str, items: Checklist) -> str:
        """Create the anchor (progress/checklist/decisions/events) and return the baseline sha."""
        ...

    async def read_situational_awareness(self) -> SituationSnapshot:
        """Reconstruct full mission awareness from git + the anchor files (seconds, any restart)."""
        ...

    async def append_event(self, event: EventRecord) -> None:
        """Append to the episodic event log (not necessarily committed immediately)."""
        ...

    async def commit_checkpoint(self, checkpoint: Checkpoint) -> str:
        """Atomically write the updated anchor and commit it; return the new commit sha.

        The harness rewrites the anchor from ``checkpoint`` (discarding any edits the agent made
        to anchor files during the cycle) before committing.
        """
        ...
