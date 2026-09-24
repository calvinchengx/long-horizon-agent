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

Every model call of every role goes through ONE ``CostMeter`` (``MeteredModel``): it is
budget-checked before it runs (hard stop, ``BudgetExceeded``) and recorded in one ledger after.
Pass the same meter to the Planner's provider (``meter.wrap(...)``) so intake is counted too.
The durable Temporal spine runs only the serial Lead composition; this is the $0/local driver.
"""

from __future__ import annotations

import asyncio
import contextlib
from dataclasses import dataclass, field
from pathlib import Path

import httpx

from lha.agent.assembly import (
    build_lead_loop,
    lead_dispatcher,
    lead_verifier,
    open_lead_sandbox,
)
from lha.agent.prompt import render_decisions
from lha.agent.runner import DECISION_CHAIN_STOP, MissionSummary, build_meter
from lha.agents.integrator import (
    BranchIntegrator,
    add_worktree,
    branch_for,
    commit_worktree,
    prune_worktrees,
    remove_worktree,
)
from lha.agents.reflection import reflect_on_failure
from lha.agents.replanner import Replanner
from lha.agents.reviewer import Reviewer, ReviewResult
from lha.agents.router import model_for_role
from lha.agents.specialists import Implementer
from lha.agents.team import research_fanout
from lha.config import Settings, get_settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.state import (
    Checklist,
    ChecklistItem,
    Checkpoint,
    DecisionRecord,
    EventRecord,
    SituationSnapshot,
)
from lha.contracts.tools import ToolContext, ToolDispatcher
from lha.contracts.verify import Check, CheckResult, VerificationResult, ensure_unique_check_names
from lha.coordination.blackboard import Blackboard
from lha.coordination.decision_log import DecisionChainError
from lha.coordination.enforcement import OwnershipGuard, changed_paths, effective_ownership
from lha.coordination.ownership import LEAD, FileOwnershipMap, OwnershipViolation, writer_for_item
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus
from lha.execution.tools import DecisionBuffer, with_decision_tool
from lha.execution.tools.toolset import build_run_dispatcher, preflight_run_tools
from lha.governor.governor import LoopDetector
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.hitl.approvals import bind_gate_store
from lha.ids import new_id
from lha.obs.events import TraceRecorder, configure_logging
from lha.obs.otel import agent_span
from lha.persistence.services import open_run_services
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.harness_integrity import harness_violations, integrity_result, snapshot_harness
from lha.verify.verifier import default_python_checks
from lha.verify.witnesses import parse_witness

_ROLES = ("lead", "researcher", "reviewer", "implementer")
_REVIEW_DIFF_CAP = 20_000
_MAX_CONSECUTIVE_FAILURES = 3  # same threshold as the Lead's AgentLoop
_BOARD_ENTRIES = 6  # newest blackboard entries shown to later rounds
_BOARD_ENTRY_CAP = 1_500


def _diff_since(workdir: str, base: str, head: str) -> str:
    if not base or not head or base == head:
        return "(no new commits)"
    diff = git_ops.run_git(
        workdir, "diff", f"{base}..{head}", "--", ".", ":(exclude).lha", check=False
    )
    return diff or "(empty diff)"


def parallel_batch(
    checklist: Checklist, ownership: FileOwnershipMap, limit: int
) -> list[ChecklistItem]:
    """Actionable items that may run concurrently: each has a non-empty write-set of its own.

    Write-sets are disjoint by construction (single writer per file), so any subset is safe.
    Fewer than two such items means there is nothing to parallelize (the Lead works serially).
    """
    if limit < 2:
        return []
    done = {i.id for i in checklist.items if i.status == "done"}
    batch = [
        item
        for item in checklist.items
        if item.is_actionable_status
        and all(dep in done for dep in item.depends_on)
        and ownership.write_set(writer_for_item(item.id))
    ]
    return batch[:limit] if len(batch) >= 2 else []


@dataclass
class _ImplementerRun:
    """One implementer's attempt at one item, before integration."""

    item: ChecklistItem
    cycle_id: str
    writer: str
    ticket: Ticket
    branch: str = ""
    worktree: Path | None = None
    head: str = ""
    brief: str = ""
    tool_calls: int = 0
    verification: VerificationResult | None = None
    violations: list[OwnershipViolation] = field(default_factory=list)
    decisions: list[DecisionRecord] = field(default_factory=list)
    error: str = ""
    tickets: list[dict[str, object]] = field(default_factory=list)  # lifecycle, for events

    def advance(self, to: TicketStatus, note: str = "") -> None:
        self.ticket = self.ticket.transition(to)
        self.tickets.append({"status": to.value, "note": note[:300]})


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
        title: str,
        description: str,
        checklist: Checklist,
        checks: list[Check] | None = None,
        allow_egress: bool | None = None,
        gate: HITLGate | None = None,
        references: list[str] | None = None,
        ownership: FileOwnershipMap | None = None,
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
        """
        preflight_run_tools(self._settings)
        configure_logging()
        run = _MissionRun(
            orchestrator=self,
            workdir=workdir,
            checks=default_python_checks() if checks is None else checks,
            allow_egress=allow_egress,
            gate=gate,
        )
        return await run.execute(
            title=title,
            description=description,
            checklist=checklist,
            ownership=ownership,
            references=references,
        )


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
        self.last_head = ""
        self.stopped = "max_cycles"

    # --- the mission loop --------------------------------------------------------------
    async def execute(
        self,
        *,
        title: str,
        description: str,
        checklist: Checklist,
        ownership: FileOwnershipMap | None,
        references: list[str] | None,
    ) -> MissionSummary:
        settings = self.settings
        meter = self.meter
        # The sandbox session and one shared HTTP pool for every role's provider are both closed
        # when the mission ends, however it ends.
        async with contextlib.AsyncExitStack() as stack:
            self.session = await open_lead_sandbox(settings, self.workdir)
            stack.push_async_callback(self.session.close)
            http = await stack.enter_async_context(httpx.AsyncClient(timeout=300.0))
            self.anchor = GitMissionAnchor(self.workdir)
            await self.anchor.initialize(
                title=title,
                description=description,
                items=checklist,
                references=references,
                ownership=ownership,
            )
            await asyncio.to_thread(prune_worktrees, self.workdir)
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
            # Persistence + memory (mission row, persistent cost ledger, tiered memory).
            self.services = await open_run_services(
                settings,
                mission_id=self.mission_id,
                workdir=self.workdir,
                meter=meter,
                title=title,
                description=description,
                model=meter.wrap(raw["lead"], role="librarian"),
                recorder=self.recorder,
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
            )
            self.reviewer = Reviewer(self.review_model, self.read_tools)
            # Integration is gated exactly like a Lead cycle would be for the same item: the
            # mission checks plus the item's witnesses (``_item_checks``), on the lead verifier
            # (trusted checks run outside the sandbox).
            self.integrator = BranchIntegrator(
                workdir=self.workdir,
                session=self.session,
                verifier=lead_verifier(self.workdir),
                checks=self.mission_checks,
            )
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
                self.board.respond(f"researcher:{item_id}", result.brief)
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
        cycle_id = f"c{self.cycles + 1}"
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
        self.reflections[item.id] = f"\nReflection on {item.id}: {reflection}\n"
        self.recorder.record("reflection", mission_id=self.mission_id, item=item.id)

    async def _review(self, item: ChecklistItem, cycle_id: str, base: str, head: str) -> str:
        """Independent review of a verified item's ``base..head`` diff.

        Returns ``"approved"``, ``"reopened"`` (a blocking verdict put the item back to
        ``todo``) or ``"looping"`` (the review keeps blocking; ``stopped`` is set).
        """
        if not self.org._do_review:
            return "approved"
        diff = await asyncio.to_thread(_diff_since, self.workdir, base, head)
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
        self.last_head = await self._reopen(item.id, cycle_id, review) or self.last_head
        self.reflections[item.id] = f"\n{review.notes()}\n"
        self.board.respond(f"reviewer:{item.id}", review.notes())
        self.recorder.record("review_reopened", mission_id=self.mission_id, item=item.id)
        if self.loop_detector.observe(f"{item.id}:review_blocked"):
            self.stopped = f"loop on item {item.id} (review keeps blocking)"
            return "looping"
        return "reopened"

    async def _reopen(self, item_id: str, cycle_id: str, review: ReviewResult) -> str | None:
        """Put a verified-but-review-blocked item back to ``todo`` with the review notes."""
        checklist = await self.anchor.read_checklist()
        item = checklist.get(item_id)
        if item is None:
            return None
        item.status = "todo"
        item.verified_by = []
        item.notes = review.notes()
        item.last_failure = "reviewer blocked: " + "; ".join(review.blocking_issues or ["unparsed"])
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
        self.meter.cycle_id = f"c{first}"
        self.recorder.record(
            "parallel_wave",
            mission_id=self.mission_id,
            items=[i.id for i in batch],
            base=base,
        )
        briefs = await self._research(batch)
        runs = [self._new_run(item, f"c{first + n}", ownership) for n, item in enumerate(batch)]
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
    ) -> _ImplementerRun:
        writer = writer_for_item(item.id)
        contract = TaskContract(
            objective=item.description,
            role="implementer",
            output_schema="a short summary of what you changed and why",
            boundaries=[
                "write only the files in write_set; request a lease instead of writing others",
                "do not edit .lha/ (harness-owned) or existing tests / test configuration",
            ],
            write_set=ownership.write_set(writer),
            tool_budget=self.settings.max_turns_per_cycle,
            acceptance=[c.name for c in self.mission_checks],
        )
        ticket = Ticket(
            id=f"{cycle_id}-{item.id}",
            contract=contract,
            item_id=item.id,
            attempts=item.attempts,
        )
        run = _ImplementerRun(item=item, cycle_id=cycle_id, writer=writer, ticket=ticket)
        run.tickets.append({"status": TicketStatus.CREATED.value, "note": ""})
        return run

    def _objective(
        self, run: _ImplementerRun, snapshot: SituationSnapshot, briefs: list[str]
    ) -> tuple[str, str]:
        contract = run.ticket.contract
        objective = (
            f"Checklist item [{run.item.id}]: {contract.objective}\n\n"
            f"Your write-set (the ONLY files you may create or modify): "
            f"{', '.join(contract.write_set)}\n"
            f"Boundaries: {'; '.join(contract.boundaries)}\n"
            f"Acceptance: the gating checks {', '.join(contract.acceptance) or '(none)'} must "
            "pass in your worktree. When the item is done, reply with "
            '{"done": true, "summary": "' + contract.output_schema + '"}.'
        )
        parts: list[str] = []
        if snapshot.mission is not None:
            parts.append(snapshot.mission.render_anchor())
        if run.item.last_failure:
            parts.append(f"Previous attempt FAILED:\n{run.item.last_failure[-3000:]}")
        if self.reflections.get(run.item.id):
            parts.append(self.reflections[run.item.id].strip())
        decisions = render_decisions(snapshot.last_decisions)
        if decisions:
            parts.append(decisions)
        if briefs:
            parts.append("Research briefs:\n" + "\n---\n".join(briefs))
        board = self._board_context()
        if board:
            parts.append(board)
        return objective, "\n\n".join(parts)

    def _item_checks(self, item: ChecklistItem) -> tuple[list[Check], list[CheckResult]]:
        """The mission checks plus the item's witnesses (as the Lead's cycle would gate it),
        and a failing result for each witness that cannot be turned into a check."""
        witnesses: list[Check] = []
        errors: list[CheckResult] = []
        for witness in item.witnesses:
            try:
                witnesses.append(parse_witness(witness, self.trusted))
            except ValueError as exc:
                errors.append(
                    CheckResult(
                        name=witness,
                        passed=False,
                        exit_code=2,
                        output_tail=f"invalid witness: {exc}",
                    )
                )
        return ensure_unique_check_names([*self.mission_checks, *witnesses]), errors

    async def _implement(
        self,
        run: _ImplementerRun,
        base: str,
        ownership: FileOwnershipMap,
        snapshot: SituationSnapshot,
        briefs: list[str],
    ) -> None:
        """Run one implementer in its own worktree; verify there; commit on its branch."""
        run.branch = branch_for(run.writer, run.cycle_id)
        run.worktree = await asyncio.to_thread(
            add_worktree, self.workdir, branch=run.branch, base=base
        )
        run.ticket = run.ticket.model_copy(update={"branch": run.branch})
        run.advance(TicketStatus.IN_PROGRESS)
        worktree = run.worktree
        globs = tuple(self.settings.harness_globs())
        session = await open_lead_sandbox(self.settings, str(worktree))
        try:
            harness_before = (
                None
                if run.item.allow_harness_edits
                else await asyncio.to_thread(snapshot_harness, worktree, globs)
            )
            buffer = DecisionBuffer()
            # The Lead's tools and human gate, behind the implementer's ownership guard.
            guarded: ToolDispatcher = OwnershipGuard(
                lead_dispatcher(self.settings, self.hitl_gate, allow_egress=self.allow_egress),
                ownership,
                writers=(run.writer,),
            )
            implementer = Implementer(
                self.implementer_model,
                with_decision_tool(guarded, buffer),
                max_turns=self.settings.max_turns_per_cycle,
            )
            objective, extra = self._objective(run, snapshot, briefs)
            with agent_span("implement", mission_id=self.mission_id, item=run.item.id):
                result = await implementer.run(
                    objective=objective,
                    ctx=ToolContext(mission_id=self.mission_id, session=session),
                    extra_context=extra,
                )
            run.brief = result.brief
            run.tool_calls = result.tool_calls
            run.decisions = list(buffer.records)
            self.board.respond(run.writer, f"[{run.item.id}] {result.brief}")
            run.advance(TicketStatus.AWAITING_VERIFY)
            checks, witness_errors = self._item_checks(run.item)
            verification = await lead_verifier(str(worktree)).verify(session, checks)
            if witness_errors:
                verification = verification.with_results(witness_errors)
            if harness_before is not None:
                after = await asyncio.to_thread(snapshot_harness, worktree, globs)
                tampered = harness_violations(harness_before, after)
                if tampered:
                    verification = verification.with_results([integrity_result(tampered)])
            run.verification = verification
        finally:
            await session.close()
        run.head = await asyncio.to_thread(
            commit_worktree,
            worktree,
            f"lha: {run.writer} {run.item.id} ({run.item.description})",
        )
        paths = await asyncio.to_thread(changed_paths, worktree, base, run.head)
        run.violations = ownership.violations(writer=run.writer, paths=paths)

    async def _integrate(self, run: _ImplementerRun, snapshot: SituationSnapshot) -> bool:
        """Offer one implementer branch to the integrator and checkpoint the result."""
        item = run.item
        own = run.verification
        verified = own is not None and own.all_green
        before = await asyncio.to_thread(git_ops.head_sha, self.workdir)
        post: VerificationResult | None = None
        if run.error:
            reason = f"implementer failed: {run.error}"
        elif not verified:
            reason = own.failure_report() if own is not None else "the branch was not verified"
        else:
            run.advance(TicketStatus.AWAITING_MERGE)
            integration = await self.integrator.integrate(
                branch=run.branch,
                branch_head=run.head,
                base=snapshot.head_sha,
                verified=True,
                violations=run.violations,
                checks=self._item_checks(item)[0],
            )
            post = integration.verification if integration.merged else None
            reason = integration.reason

        checklist = await self.anchor.read_checklist()
        merged = post is not None
        if post is not None:
            run.advance(TicketStatus.DONE, f"merged {run.branch}")
            checklist.record_success(
                item.id, [r.name for r in post.results if r.gating and r.passed]
            )
            # The slice is finished: its lease ends in the same commit that merges it (as do
            # those of any other finished item not yet released).
            persisted = await self.anchor.read_ownership()
            done = [writer_for_item(i.id) for i in checklist.items if i.status == "done"]
            released = effective_ownership(persisted, done)
            if released != persisted:
                self.anchor.stage_ownership(released)
        else:
            run.advance(TicketStatus.FAILED, reason)
            checklist.record_failure(
                item.id, reason, max_consecutive_failures=_MAX_CONSECUTIVE_FAILURES
            )
        current = checklist.get(item.id)
        status = current.status if current is not None else ""
        attempts = current.attempts if current is not None else 0
        split_into: list[str] = []
        if merged:
            verb, note, verdict = "complete", "verified + integrated", "passed"
        else:
            verb = "block" if status == "blocked" else "attempt"
            note = f"not integrated (attempt {attempts}, status {status})"
            verdict = own.verdict if own is not None and not own.all_green else "failed"
            if status == "blocked" and current is not None:
                split_into = await self._maybe_split(checklist, current, snapshot)
                if split_into:
                    verb, status = "split", "split"
                    note += f"; split into {', '.join(split_into)}"
        shown = post if post is not None else own
        head = await self.anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=run.cycle_id,
                progress_summary=(
                    f"- {run.cycle_id} [{item.id}] {item.description}: {note} "
                    f"({run.writer}, {run.branch or 'no branch'})"
                ),
                checklist=checklist,
                # Decisions travel with the code they describe: only merged work records them.
                decisions=run.decisions if merged else [],
                events=[
                    EventRecord(
                        kind="cycle",
                        cycle_id=run.cycle_id,
                        payload={
                            "item_id": item.id,
                            "verified": merged,
                            "verdict": verdict,
                            "status": status,
                            "tool_calls": run.tool_calls,
                            "writer": run.writer,
                            "branch": run.branch,
                            "split_into": split_into,
                            "checks": [
                                {
                                    "name": r.name,
                                    "passed": r.passed,
                                    "gating": r.gating,
                                    "exit_code": r.exit_code,
                                    "duration_s": round(r.duration_s, 3),
                                }
                                for r in (shown.results if shown is not None else [])
                            ],
                        },
                    ),
                    EventRecord(
                        kind="ticket",
                        cycle_id=run.cycle_id,
                        payload={
                            "ticket_id": run.ticket.id,
                            "item_id": item.id,
                            "role": run.ticket.contract.role,
                            "write_set": run.ticket.contract.write_set,
                            "branch": run.branch,
                            "status": run.ticket.status.value,
                            "history": run.tickets,
                            "ownership_violations": [v.path for v in run.violations],
                        },
                    ),
                ],
                commit_message=f"lha: {verb} {item.id} ({item.description})"
                + (f" [merged {run.branch}]" if merged and run.head != snapshot.head_sha else ""),
            )
        )
        await asyncio.to_thread(self.integrator.abort)  # never leave a half-finished merge
        self.last_head = head or self.last_head
        self.recorder.record(
            "integration",
            mission_id=self.mission_id,
            item=item.id,
            merged=merged,
            branch=run.branch,
            reason=reason[:500],
        )

        if not merged:
            if checklist.is_deadlocked:
                self.stopped = f"deadlocked: {checklist.deadlock_reason()}"
                return False
            if self.loop_detector.observe(f"{item.id}:failed"):
                self.stopped = f"loop on item {item.id}"
                return False
            if status not in ("blocked", "split"):
                await self._reflect(item, f"{item.id} was not integrated: {reason}")
            return True
        self.loop_detector.observe(f"{item.id}:failed", failed=False)
        review = await self._review(item, run.cycle_id, before, head)
        if review != "approved":
            return review == "reopened"
        if checklist.is_complete:
            self.stopped = "complete"
            return False
        return True

    async def _maybe_split(
        self, checklist: Checklist, item: ChecklistItem, snapshot: SituationSnapshot
    ) -> list[str]:
        """Split a newly blocked item with the replanner, within the mission's replan budget
        (the same bounds as the Lead's ``AgentLoop``); returns the child ids ([] if not split)."""
        settings = self.settings
        if settings.max_replans <= 0:
            return []
        if sum(1 for i in checklist.items if i.status == "split") >= settings.max_replans:
            return []
        if item.id.count(".") >= settings.max_split_depth:
            return []
        mission_text = snapshot.mission.render_anchor() if snapshot.mission is not None else ""
        drafts = await Replanner(self.lead_model).split(mission_text=mission_text, item=item)
        if len(drafts) < 2:
            return []
        return [child.id for child in checklist.split(item.id, drafts)]
