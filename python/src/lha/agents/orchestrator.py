"""The multi-agent orchestrator — drives the full org through a mission (local flow).

Each round of the mission is either a SERIAL cycle or a PARALLEL wave:

* **Serial cycle** (the default): (1) fan out **read-only Researchers**, (2) hand the briefs to
  the single **Lead Engineer** loop, which implements + verifies + checkpoints in the mission
  workspace, then (3) on failure run **reflection** (fed into the next attempt) or (4) on success
  run an independent **Reviewer** whose structured verdict can REOPEN the item.
* **Parallel wave**: when two or more actionable items have disjoint, Planner-assigned
  write-sets (``FileOwnershipMap``), each is worked by its own **Implementer** in its own git
  worktree on its own branch, concurrently. Each implementer's write tools are wrapped in an
  ``OwnershipGuard`` (a write to a file it does not own is refused), and its branch is verified
  in its worktree. The **Integrator** (``BranchIntegrator``) then merges each verified branch —
  after a git-layer ownership check of what the branch actually changed — into the mission
  branch, re-verifies the merged result, and commits the merge together with the checkpoint. The
  Reviewer then reviews each integrated item as in a serial cycle.

Coordination artifacts: each implementer's work order is a typed ``Ticket``/``TaskContract``
whose lifecycle (created → in_progress → awaiting_verify → awaiting_merge → done | failed) is
recorded as ``ticket`` events in the checkpoint; research briefs, implementer summaries and review
notes go to a ``Blackboard`` whose response board is promoted between rounds, so agents in one
round never see each other's output but later rounds see all of it. The ownership map lives in
the anchor (``.lha/ownership.json``); a finished item's files are released back to the lead.

An implementer that needs a file outside its write-set calls ``request_lease``; the
``LeaseBroker`` (``lha.coordination.leases``) grants it when the file is unowned or its owner has
finished, refuses it otherwise, and commits the decision (and a grant's ownership change) to the
anchor at once.

Resuming (``run_mission(resume=True)``, ``lha orchestrate --resume``): the existing anchor is kept
(checklist, ownership map, decision chain, events) instead of being re-initialized; uncommitted
residue, a half-finished merge and leftover implementer worktrees are discarded; the mission id,
the blackboard's main board and the latest reflections are rebuilt from the committed
``orchestrate`` / ``blackboard`` / ``reflection`` events, and cycle ids continue after the last
committed one. Nothing that was committed is lost.

Every model call of every role goes through ONE ``CostMeter`` (``MeteredModel``): it is
budget-checked before it runs (hard stop, ``BudgetExceeded``) and recorded in one ledger after.
Pass the same meter to the Planner's provider (``meter.wrap(...)``) so intake is counted too.
The budget and cycle ceilings apply to one invocation. The durable Temporal workflow runs the
same organization (``lha.durable.org_round``); this is the local driver.
"""

from __future__ import annotations

import asyncio
import contextlib
import re
from pathlib import Path

import httpx

from lha.agent.assembly import (
    build_lead_loop,
    lead_dispatcher,
    lead_verifier,
    open_lead_sandbox,
)
from lha.agent.prompt import render_decisions
from lha.agent.runner import (
    DECISION_CHAIN_STOP,
    MissionSummary,
    build_meter,
    mission_span_attributes,
)
from lha.agents.integrator import BranchIntegrator, prune_worktrees, remove_worktree
from lha.agents.reflection import reflect_on_failure
from lha.agents.reviewer import Reviewer, ReviewResult
from lha.agents.router import model_for_role
from lha.agents.team import research_fanout
from lha.agents.waves import (
    MAX_CONSECUTIVE_FAILURES,
    ImplementerRun,
    diff_since,
    implement_in_worktree,
    implementer_objective,
    integrate_run,
    item_checks,
    maybe_split,
    new_implementer_run,
    parallel_batch,
    reopen_for_review,
)
from lha.config import Settings, get_settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.state import (
    Checklist,
    ChecklistItem,
    Checkpoint,
    EventRecord,
    SituationSnapshot,
)
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check, CheckResult
from lha.coordination.blackboard import Blackboard
from lha.coordination.decision_log import DecisionChainError
from lha.coordination.enforcement import OwnershipGuard, changed_paths, effective_ownership
from lha.coordination.leases import LeaseBroker, lease_handler
from lha.coordination.ownership import LEAD, FileOwnershipMap, writer_for_item
from lha.execution.tools.toolset import build_run_dispatcher, preflight_run_tools
from lha.governor.governor import LoopDetector
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.hitl.approvals import bind_gate_store
from lha.ids import new_id
from lha.obs.events import TraceRecorder, configure_logging
from lha.obs.otel import agent_span, span
from lha.persistence.services import open_run_services
from lha.state import git_ops
from lha.state.mission_anchor import ANCHOR_DIR, MISSION_FILE, GitMissionAnchor
from lha.systemone.build import build_system_one
from lha.verify.verifier import default_python_checks

__all__ = ["MissionResumeError", "Orchestrator", "anchor_exists", "parallel_batch"]

_ROLES = ("lead", "researcher", "reviewer", "implementer")
_REVIEW_DIFF_CAP = 20_000
_MAX_CONSECUTIVE_FAILURES = MAX_CONSECUTIVE_FAILURES
_BOARD_ENTRIES = 6  # newest blackboard entries shown to later rounds
_BOARD_ENTRY_CAP = 1_500
# Committed event kinds that let ``--resume`` rebuild the in-memory state of a run.
RUN_EVENT = "orchestrate"
BOARD_EVENT = "blackboard"
REFLECTION_EVENT = "reflection"
_CYCLE_ID = re.compile(r"^c(\d+)$")

# Kept for callers of the old private names.
_ImplementerRun = ImplementerRun
_diff_since = diff_since


class MissionResumeError(ValueError):
    """``resume`` was asked for a workdir without a mission anchor (or vice versa)."""


def anchor_exists(workdir: str | Path) -> bool:
    """True if ``workdir`` holds a committed mission anchor (``.lha/mission.json`` at HEAD)."""
    path = Path(workdir)
    if not (path / ANCHOR_DIR).is_dir() or not git_ops.is_repo(path):
        return False
    return git_ops.exists_at_head(path, f"{ANCHOR_DIR}/{MISSION_FILE}")


class Orchestrator:
    """Runs a mission with the full org (research + Lead or parallel implementers + review)."""

    def __init__(
        self,
        settings: Settings | None = None,
        *,
        research_per_item: int = 2,
        do_review: bool = True,
        meter: CostMeter | None = None,
        models: dict[str, ModelProvider] | None = None,
        max_parallel: int | None = None,
    ) -> None:
        """``meter``: share one budget/ledger with other callers (e.g. the Planner).
        ``models``: per-role provider overrides (``lead``/``researcher``/``reviewer``/
        ``implementer``); unmetered providers are wrapped with the meter here either way.
        ``max_parallel``: items per parallel wave (default ``settings.max_parallel_implementers``;
        below 2 disables parallel waves).
        """
        self._settings = settings or get_settings()
        self._research_per_item = research_per_item
        self._do_review = do_review
        self._meter = meter
        self._models = dict(models or {})
        self._max_parallel = (
            self._settings.max_parallel_implementers if max_parallel is None else max_parallel
        )

    async def run_mission(
        self,
        *,
        workdir: str,
        title: str = "",
        description: str = "",
        checklist: Checklist | None = None,
        checks: list[Check] | None = None,
        allow_egress: bool | None = None,
        gate: HITLGate | None = None,
        references: list[str] | None = None,
        ownership: FileOwnershipMap | None = None,
        resume: bool = False,
    ) -> MissionSummary:
        """Run the org until complete / deadlocked / over-budget / looping.

        ``checks``: gating verification checks (``None`` => ``default_python_checks()``).
        ``gate``: where irreversible commands go for a human decision (``None`` denies them), for
        the Lead and the implementers alike. ``references``: vendored reference paths recited
        every cycle. ``ownership``: the Planner's file-ownership map (persisted in the anchor;
        items with a write-set of their own can run in parallel waves). The sandbox comes from
        ``settings.sandbox`` (``local`` needs ``allow_unsafe_local``). ``allow_egress``: ``None`` =>
        the Lead and the Researchers get the web tools iff ``LHA_WEB_ALLOW_HOSTS`` is set (the
        Reviewer's role hides them); ``False`` drops them. A lethal-trifecta run raises
        ``RuleOfTwoViolation`` before it starts.

        ``resume``: continue the mission already anchored in ``workdir`` (its committed spec,
        checklist, ownership map and decisions); ``title`` / ``description`` / ``checklist`` /
        ``references`` / ``ownership`` are then ignored. Raises ``MissionResumeError`` if there
        is no anchor. Without ``resume`` the anchor is (re-)initialized from the arguments.
        """
        preflight_run_tools(self._settings)
        if resume and not await asyncio.to_thread(anchor_exists, workdir):
            raise MissionResumeError(f"no mission anchor to resume at {workdir!r}")
        if not resume and checklist is None:
            raise ValueError("a checklist is required unless resuming")
        configure_logging()
        run = _MissionRun(
            orchestrator=self,
            workdir=workdir,
            checks=default_python_checks() if checks is None else checks,
            allow_egress=allow_egress,
            gate=gate,
        )
        attributes = {"lha.run_path": "orchestrate", "lha.title": title, "lha.resume": resume}
        with span("lha.mission", attributes) as traced:
            summary = await run.execute(
                title=title,
                description=description,
                checklist=checklist,
                ownership=ownership,
                references=references,
                resume=resume,
            )
            traced.set(mission_span_attributes(summary))
            return summary


class _MissionRun:
    """The state of one ``run_mission`` call (kept off the reusable ``Orchestrator``)."""

    def __init__(
        self,
        *,
        orchestrator: Orchestrator,
        workdir: str,
        checks: list[Check],
        allow_egress: bool | None,
        gate: HITLGate | None,
    ) -> None:
        self.org = orchestrator
        self.settings = orchestrator._settings
        self.workdir = workdir
        self.mission_checks = checks
        self.allow_egress = allow_egress
        self.hitl_gate = gate
        self.trusted = self.settings.trusted_check_commands()
        self.recorder = TraceRecorder()
        self.meter = orchestrator._meter or build_meter(self.settings)
        self.loop_detector = LoopDetector(threshold=self.settings.stall_limit)
        self.mission_id = new_id("mission")
        self.board = Blackboard()
        self.reflections: dict[str, str] = {}
        self.cycles = 0
        # Cycle ids continue after the last committed one when a run is resumed.
        self.cycle_offset = 0
        self.run_number = 1
        self.last_head = ""
        self.stopped = "max_cycles"

    def _cycle_id(self, n: int) -> str:
        """The id of this invocation's ``n``-th cycle (1-based)."""
        return f"c{self.cycle_offset + n}"

    # --- the mission loop --------------------------------------------------------------
    async def execute(
        self,
        *,
        title: str,
        description: str,
        checklist: Checklist | None,
        ownership: FileOwnershipMap | None,
        references: list[str] | None,
        resume: bool,
    ) -> MissionSummary:
        settings = self.settings
        meter = self.meter
        # The sandbox session and one shared HTTP pool for every role's provider are both closed
        # when the mission ends, however it ends.
        async with contextlib.AsyncExitStack() as stack:
            self.anchor = GitMissionAnchor(self.workdir)
            if resume:
                await asyncio.to_thread(self._recover_workspace)
                title, description = await self._restore_run_state(title, description)
            else:
                assert checklist is not None  # checked by run_mission
                await self.anchor.initialize(
                    title=title,
                    description=description,
                    items=checklist,
                    references=references,
                    ownership=ownership,
                )
            await asyncio.to_thread(prune_worktrees, self.workdir)
            await self.anchor.append_event(
                EventRecord(
                    kind=RUN_EVENT,
                    payload={
                        "mission_id": self.mission_id,
                        "resumed": resume,
                        "run": self.run_number,
                    },
                )
            )
            self.session = await open_lead_sandbox(settings, self.workdir)
            stack.push_async_callback(self.session.close)
            http = await stack.enter_async_context(httpx.AsyncClient(timeout=300.0))
            raw = {
                role: self.org._models.get(role) or model_for_role(role, settings, client=http)
                for role in _ROLES
            }
            self.lead_model = meter.wrap(raw["lead"], role="lead")
            self.research_model = meter.wrap(raw["researcher"], role="researcher")
            self.review_model = meter.wrap(raw["reviewer"], role="reviewer")
            self.reflection_model = meter.wrap(raw["lead"], role="reflection")
            self.implementer_model = meter.wrap(raw["implementer"], role="implementer")

            # Researchers get the web tools with the lead (the Reviewer's role hides them).
            self.read_tools = build_run_dispatcher(
                settings, allow_mutating=False, allow_egress=self.allow_egress
            )
            # Persistence + memory (mission row, persistent cost ledger, tiered memory). A resumed
            # run keeps the mission id, so its ledger keys get their own prefix.
            self.services = await open_run_services(
                settings,
                mission_id=self.mission_id,
                workdir=self.workdir,
                meter=meter,
                system_one=build_system_one(settings, meter),
                title=title,
                description=description,
                model=meter.wrap(raw["lead"], role="librarian"),
                recorder=self.recorder,
                key_prefix="" if self.run_number == 1 else f"run{self.run_number}",
            )
            stack.push_async_callback(self.services.close)
            await self.services.tracker.running()
            if self.hitl_gate is not None:
                bind_gate_store(self.hitl_gate, self.services.store)
            # The Lead writes through an ownership guard: unassigned space, shared files and the
            # active item's own files — never another open item's leased files.
            self.lead_guard = OwnershipGuard(
                lead_dispatcher(settings, self.hitl_gate, allow_egress=self.allow_egress),
                FileOwnershipMap(),
                writers=(LEAD,),
            )
            self.lead = build_lead_loop(
                settings,
                model=self.lead_model,
                anchor=self.anchor,
                workdir=self.workdir,
                gate=self.hitl_gate,
                recorder=self.recorder,
                dispatcher=self.lead_guard,
                memory=self.services.memory,
                system_one=self.services.system_one,
            )
            self.reviewer = Reviewer(self.review_model, self.read_tools)
            # Integration is gated exactly like a Lead cycle would be for the same item: the
            # mission checks plus the item's witnesses (``_item_checks``), on the lead verifier
            # (trusted checks run outside the sandbox).
            self.integrator = BranchIntegrator(
                workdir=self.workdir,
                session=self.session,
                verifier=lead_verifier(self.workdir, self.settings),
                checks=self.mission_checks,
            )
            # Leases are decided against this run's own anchor instance, so a grant and the
            # ownership release staged by ``_current_ownership`` are committed together.
            self.leases = LeaseBroker(self.anchor)
            self.ctx = ToolContext(mission_id=self.mission_id, session=self.session)

            try:
                await self._loop(title=title, description=description)
            except BudgetExceeded as exc:
                self.recorder.record("governor_block", mission_id=self.mission_id, reason=str(exc))
                self.stopped = f"governor: {exc.decision.reason}"
            except DecisionChainError as exc:  # altered decision history: refuse to continue
                self.recorder.record(
                    "decision_chain_invalid", mission_id=self.mission_id, reason=str(exc)
                )
                self.stopped = f"{DECISION_CHAIN_STOP}: {exc}"
            except BaseException as exc:
                await self.services.finish(f"error: {type(exc).__name__}", head_sha=self.last_head)
                raise
            finally:
                await asyncio.to_thread(self.integrator.abort)
                await asyncio.to_thread(prune_worktrees, self.workdir)

            final = await self.anchor.read_checklist()
            await self.services.finish(self.stopped, head_sha=self.last_head)
        return MissionSummary(
            mission_id=self.mission_id,
            completed=final.is_complete,
            cycles=self.cycles,
            items_done=final.items_done,
            items_total=final.items_total,
            total_usd=meter.ledger.total_usd,
            head_sha=self.last_head,
            stopped_reason=self.stopped,
            trace_jsonl=self.recorder.to_jsonl(),
        )

    # --- resume ---------------------------------------------------------------------------
    def _recover_workspace(self) -> None:
        """Discard what an interrupted run left uncommitted (never anything committed)."""
        if (git_ops.git_dir(self.workdir) / "MERGE_HEAD").exists():
            git_ops.run_git(self.workdir, "merge", "--abort", check=False)
        git_ops.discard_changes(self.workdir)
        self.last_head = git_ops.head_sha(self.workdir)

    async def _restore_run_state(self, title: str, description: str) -> tuple[str, str]:
        """Rebuild the mission id, board, reflections and cycle numbering from committed events.

        Returns the committed mission's title and description (the arguments are fallbacks).
        """
        # The decision chain must still verify before anything builds on it.
        await self.anchor.read_decisions()
        spec = await self.anchor.read_mission()
        events = await self.anchor.read_events()
        checklist = await self.anchor.read_checklist()
        runs = [e for e in events if e.kind == RUN_EVENT]
        if runs:
            self.mission_id = str(runs[-1].payload.get("mission_id") or self.mission_id)
        self.run_number = len(runs) + 1
        numbers = [
            int(m.group(1)) for e in events if (m := _CYCLE_ID.match(e.cycle_id)) is not None
        ]
        self.cycle_offset = max(numbers, default=0)
        for event in events:
            if event.kind == BOARD_EVENT:
                self.board.post(
                    str(event.payload.get("author", "")), str(event.payload.get("text", ""))
                )
        open_ids = {i.id for i in checklist.items if i.is_open}
        for event in events:
            item_id = str(event.payload.get("item", ""))
            if event.kind == REFLECTION_EVENT and item_id in open_ids:
                self.reflections[item_id] = str(event.payload.get("text", ""))
        self.recorder.record(
            "resumed",
            mission_id=self.mission_id,
            run=self.run_number,
            cycle_offset=self.cycle_offset,
            board=self.board.posted,
        )
        if spec is None:
            return title, description
        return spec.title, spec.description

    async def _post(self, author: str, content: str) -> None:
        """Post to this round's response board, and record it for a later ``--resume``."""
        self.board.respond(author, content)
        await self.anchor.append_event(
            EventRecord(
                kind=BOARD_EVENT, payload={"author": author, "text": content[:_BOARD_ENTRY_CAP]}
            )
        )

    async def _loop(self, *, title: str, description: str) -> None:
        while self.cycles < self.settings.max_cycles:
            decision = self.meter.governor.authorize_next(
                self.meter.ledger, cycles_done=self.cycles
            )
            if not decision.allow:
                self.recorder.record(
                    "governor_block", mission_id=self.mission_id, reason=decision.reason
                )
                self.stopped = f"governor: {decision.reason}"
                return

            snapshot = await self.anchor.read_situational_awareness()
            if snapshot.is_complete:
                self.stopped = "complete"
                return
            if snapshot.is_deadlocked or snapshot.active_item is None:
                self.stopped = f"deadlocked: {snapshot.deadlock_reason or 'no actionable item'}"
                return

            checklist = await self.anchor.read_checklist()
            ownership = await self._current_ownership(checklist)
            batch = parallel_batch(checklist, ownership, self.org._max_parallel)
            if batch:
                keep_going = await self._parallel_wave(snapshot, batch, ownership)
            else:
                keep_going = await self._serial_cycle(
                    snapshot, snapshot.active_item, ownership, title, description
                )
            self.board.commit_round()
            if not keep_going:
                return

    async def _current_ownership(self, checklist: Checklist) -> FileOwnershipMap:
        """The committed map with finished items' files released (staged for the next commit)."""
        persisted = await self.anchor.read_ownership()
        done = [writer_for_item(i.id) for i in checklist.items if i.status == "done"]
        effective = effective_ownership(persisted, done)
        if effective != persisted:
            self.anchor.stage_ownership(effective)
            released = sorted(set(persisted.owners) - set(effective.owners))
            self.recorder.record("ownership_released", mission_id=self.mission_id, paths=released)
        return effective

    # --- context shared by both round kinds -------------------------------------------
    def _board_context(self) -> str:
        entries = self.board.read()[-_BOARD_ENTRIES:]
        if not entries:
            return ""
        lines = [f"[{e.author}] {e.content[:_BOARD_ENTRY_CAP]}" for e in entries]
        return "Team board (earlier rounds):\n" + "\n---\n".join(lines)

    async def _research(self, items: list[ChecklistItem]) -> dict[str, list[str]]:
        """Research briefs per item (all items' queries fan out together)."""
        per_item = self.org._research_per_item
        if per_item <= 0:
            return {}
        queries: list[tuple[str, str]] = []
        for item in items:
            templates = [
                f"Find context relevant to: {item.description}",
                f"Find existing files/code related to: {item.description}",
            ][:per_item]
            queries.extend((item.id, q) for q in templates)
        results = await research_fanout(
            model=self.research_model,
            dispatcher=self.read_tools,
            ctx=self.ctx,
            queries=[q for _, q in queries],
        )
        briefs: dict[str, list[str]] = {item.id: [] for item in items}
        failed: dict[str, list[str]] = {item.id: [] for item in items}
        for (item_id, _query), result in zip(queries, results, strict=True):
            if result.error is not None:
                failed[item_id].append(result.error)
            elif result.brief:
                briefs[item_id].append(result.brief)
                await self._post(f"researcher:{item_id}", result.brief)
        for item in items:
            self.recorder.record(
                "research",
                mission_id=self.mission_id,
                item=item.id,
                n=len(briefs[item.id]),
                failed=len(failed[item.id]),
            )
            for error in failed[item.id]:
                self.recorder.record(
                    "research_failed", mission_id=self.mission_id, item=item.id, error=error
                )
        return briefs

    # --- serial round: the Lead ----------------------------------------------------------
    async def _serial_cycle(
        self,
        snapshot: SituationSnapshot,
        item: ChecklistItem,
        ownership: FileOwnershipMap,
        title: str,
        description: str,
    ) -> bool:
        cycle_id = self._cycle_id(self.cycles + 1)
        self.meter.cycle_id = cycle_id
        briefs = (await self._research([item])).get(item.id, [])

        # The anchor comes from the immutable mission spec, never from progress.md.
        mission_text = (
            snapshot.mission.render_anchor()
            if snapshot.mission is not None
            else f"Mission: {title}\n\n{description}"
        )
        anchor_text = (
            f"{mission_text}\n"
            f"{self.reflections.get(item.id, '')}\n\n"
            f"Research briefs:\n" + ("\n---\n".join(briefs) or "(none)")
        )
        board = self._board_context()
        if board:
            anchor_text += f"\n\n{board}"

        writers = (LEAD, writer_for_item(item.id))
        self.lead_guard.update(ownership=ownership, writers=writers)
        with agent_span("cycle", mission_id=self.mission_id, item=item.id):
            outcome = await self.lead.run_cycle(
                ctx=self.ctx,
                mission_id=self.mission_id,
                cycle_id=cycle_id,
                anchor_text=anchor_text,
                checks=self.mission_checks,
            )
        if not outcome.advanced:  # nothing actionable after all
            self.stopped = f"deadlocked: {outcome.reason or 'no actionable item'}"
            return False
        self.cycles += 1
        self.last_head = outcome.head_sha or self.last_head
        await self._audit_lead_writes(ownership, writers, snapshot.head_sha, outcome.head_sha)

        if not outcome.verified:
            if outcome.is_deadlocked:
                self.recorder.record(
                    "deadlocked", mission_id=self.mission_id, reason=outcome.reason
                )
                self.stopped = f"deadlocked: {outcome.reason or 'no actionable item'}"
                return False
            if self.loop_detector.observe(f"{item.id}:failed"):
                self.stopped = f"loop on item {item.id}"
                return False
            if outcome.item_blocked or outcome.item_split:  # not re-picked; nothing to reflect on
                return True
            await self._reflect(
                item,
                f"{item.id} failed verification ({outcome.verdict}) after "
                f"{outcome.turns} turns: {outcome.reason}",
            )
            return True
        self.loop_detector.observe(f"{item.id}:failed", failed=False)

        review = await self._review(item, cycle_id, snapshot.head_sha, outcome.head_sha)
        if review != "approved":
            return review == "reopened"  # "looping" stops the mission
        if outcome.is_complete:
            self.stopped = "complete"
            return False
        return True

    async def _audit_lead_writes(
        self, ownership: FileOwnershipMap, writers: tuple[str, ...], base: str, head: str
    ) -> None:
        """Git-layer check of the Lead's cycle: record writes to other items' leased files.

        Detection only: in a serial round no other writer is active, so such a write cannot
        conflict; the tool-level guard already refused the ones made with ``write_file``.
        """
        paths = await asyncio.to_thread(changed_paths, self.workdir, base, head)
        violations = ownership.violations_any(writers=writers, paths=paths)
        if violations:
            self.recorder.record(
                "ownership_violation",
                mission_id=self.mission_id,
                writer="+".join(writers),
                paths=[v.path for v in violations],
            )

    async def _reflect(self, item: ChecklistItem, failure_summary: str) -> None:
        reflection = await reflect_on_failure(
            model=self.reflection_model,
            item_description=item.description,
            failure_summary=failure_summary,
        )
        await self._set_reflection(item.id, f"\nReflection on {item.id}: {reflection}\n")
        self.recorder.record("reflection", mission_id=self.mission_id, item=item.id)

    async def _set_reflection(self, item_id: str, text: str) -> None:
        """Remember ``text`` for the item's next attempt (recorded for a later ``--resume``)."""
        self.reflections[item_id] = text
        await self.anchor.append_event(
            EventRecord(kind=REFLECTION_EVENT, payload={"item": item_id, "text": text[:2_000]})
        )

    async def _review(self, item: ChecklistItem, cycle_id: str, base: str, head: str) -> str:
        """Independent review of a verified item's ``base..head`` diff.

        Returns ``"approved"``, ``"reopened"`` (a blocking verdict put the item back to
        ``todo``) or ``"looping"`` (the review keeps blocking; ``stopped`` is set).
        """
        if not self.org._do_review:
            return "approved"
        diff = await asyncio.to_thread(diff_since, self.workdir, base, head)
        review = await self.reviewer.review(
            diff=diff[:_REVIEW_DIFF_CAP], criteria=item.description, ctx=self.ctx
        )
        self.recorder.record(
            "review",
            mission_id=self.mission_id,
            item=item.id,
            blocking=review.blocking,
            verdict=review.verdict,
        )
        if not review.blocking:
            self.loop_detector.observe(f"{item.id}:review_blocked", failed=False)
            return "approved"
        await self._set_reflection(item.id, f"\n{review.notes()}\n")
        await self._post(f"reviewer:{item.id}", review.notes())
        self.last_head = await self._reopen(item.id, cycle_id, review) or self.last_head
        self.recorder.record("review_reopened", mission_id=self.mission_id, item=item.id)
        if self.loop_detector.observe(f"{item.id}:review_blocked"):
            self.stopped = f"loop on item {item.id} (review keeps blocking)"
            return "looping"
        return "reopened"

    async def _reopen(self, item_id: str, cycle_id: str, review: ReviewResult) -> str | None:
        """Put a verified-but-review-blocked item back to ``todo`` with the review notes."""
        checklist = await self.anchor.read_checklist()
        if reopen_for_review(checklist, item_id, review) is None:
            return None
        return await self.anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=cycle_id,
                progress_summary=f"{cycle_id}: review reopened {item_id} ({review.verdict})",
                checklist=checklist,
                commit_message=f"lha: review reopened {item_id}",
            )
        )

    # --- parallel round: implementers + integrator ---------------------------------------
    async def _parallel_wave(
        self,
        snapshot: SituationSnapshot,
        batch: list[ChecklistItem],
        ownership: FileOwnershipMap,
    ) -> bool:
        base = snapshot.head_sha
        first = self.cycles + 1
        self.meter.cycle_id = self._cycle_id(first)
        self.recorder.record(
            "parallel_wave",
            mission_id=self.mission_id,
            items=[i.id for i in batch],
            base=base,
        )
        briefs = await self._research(batch)
        runs = [
            self._new_run(item, self._cycle_id(first + n), ownership)
            for n, item in enumerate(batch)
        ]
        try:
            outcomes = await asyncio.gather(
                *(
                    self._implement(run, base, ownership, snapshot, briefs.get(run.item.id, []))
                    for run in runs
                ),
                return_exceptions=True,
            )
            for run, outcome in zip(runs, outcomes, strict=True):
                if isinstance(outcome, BaseException):
                    if not isinstance(outcome, Exception) or isinstance(outcome, BudgetExceeded):
                        raise outcome  # cancellation / budget: stop, don't paper over it
                    run.error = f"{type(outcome).__name__}: {outcome}"[:500]
            keep_going = True
            for run in runs:
                self.cycles += 1
                if not await self._integrate(run, snapshot):
                    keep_going = False
                    break
            return keep_going
        finally:
            for run in runs:
                if run.worktree is not None:
                    await asyncio.to_thread(
                        remove_worktree, self.workdir, path=run.worktree, branch=run.branch
                    )

    def _new_run(
        self, item: ChecklistItem, cycle_id: str, ownership: FileOwnershipMap
    ) -> ImplementerRun:
        return new_implementer_run(
            item,
            cycle_id,
            ownership,
            tool_budget=self.settings.max_turns_per_cycle,
            acceptance=[c.name for c in self.mission_checks],
        )

    def _item_checks(self, item: ChecklistItem) -> tuple[list[Check], list[CheckResult]]:
        """The mission checks plus the item's witnesses (as the Lead's cycle would gate it),
        and a failing result for each witness that cannot be turned into a check."""
        return item_checks(item, self.mission_checks, self.trusted)

    async def _implement(
        self,
        run: ImplementerRun,
        base: str,
        ownership: FileOwnershipMap,
        snapshot: SituationSnapshot,
        briefs: list[str],
    ) -> None:
        """Run one implementer in its own worktree; verify there; commit on its branch."""
        objective, extra = implementer_objective(
            run,
            mission_text=snapshot.mission.render_anchor() if snapshot.mission is not None else "",
            reflection=self.reflections.get(run.item.id, ""),
            decisions_text=render_decisions(snapshot.last_decisions),
            briefs=briefs,
            board=self._board_context(),
        )
        summaries: list[str] = []
        await implement_in_worktree(
            run,
            settings=self.settings,
            workdir=self.workdir,
            base=base,
            ownership=ownership,
            model=self.implementer_model,
            gate=self.hitl_gate,
            allow_egress=self.allow_egress,
            mission_id=self.mission_id,
            mission_checks=self.mission_checks,
            objective=objective,
            extra=extra,
            lease=lease_handler(
                self.leases,
                writer=run.writer,
                cycle_id=run.cycle_id,
                ownership=ownership,
                log=run.leases,
            ),
            on_summary=summaries.append,
        )
        for decision in run.leases:
            self.recorder.record(
                "lease",
                mission_id=self.mission_id,
                writer=decision.writer,
                path=decision.path,
                granted=decision.granted,
                why=decision.why,
            )
        for summary in summaries:
            await self._post(run.writer, f"[{run.item.id}] {summary}")

    async def _integrate(self, run: ImplementerRun, snapshot: SituationSnapshot) -> bool:
        """Offer one implementer branch to the integrator and checkpoint the result."""
        item = run.item

        async def split(checklist: Checklist, current: ChecklistItem) -> list[str]:
            return await maybe_split(
                checklist,
                current,
                settings=self.settings,
                model=self.lead_model,
                mission_text=(
                    snapshot.mission.render_anchor() if snapshot.mission is not None else ""
                ),
            )

        report = await integrate_run(
            run,
            anchor=self.anchor,
            integrator=self.integrator,
            workdir=self.workdir,
            base=snapshot.head_sha,
            checks=self._item_checks(item)[0],
            split=split,
        )
        merged = report.merged
        self.last_head = report.head or self.last_head
        self.recorder.record(
            "integration",
            mission_id=self.mission_id,
            item=item.id,
            merged=merged,
            branch=run.branch,
            reason=report.reason[:500],
        )

        if not merged:
            if report.checklist.is_deadlocked:
                self.stopped = f"deadlocked: {report.checklist.deadlock_reason()}"
                return False
            if self.loop_detector.observe(f"{item.id}:failed"):
                self.stopped = f"loop on item {item.id}"
                return False
            if report.status not in ("blocked", "split"):
                await self._reflect(item, f"{item.id} was not integrated: {report.reason}")
            return True
        self.loop_detector.observe(f"{item.id}:failed", failed=False)
        review = await self._review(item, run.cycle_id, report.before, report.head)
        if review != "approved":
            return review == "reopened"
        if report.checklist.is_complete:
            self.stopped = "complete"
            return False
        return True
