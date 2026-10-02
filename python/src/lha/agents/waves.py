"""The building blocks of a parallel implementer wave, shared by both run paths.

``lha orchestrate`` (``lha.agents.orchestrator``) runs a wave in one process; the durable
``MissionWorkflow`` runs each implementer as its own activity and each integration as another
(``lha.durable.org_activities``). Both use these functions, so a wave means the same thing on
either path:

* ``parallel_batch`` picks the actionable items that each own a non-empty write-set;
* ``new_implementer_run`` creates the item's ``Ticket`` / ``TaskContract``;
* ``implement_in_worktree`` runs one ``Implementer`` in its own git worktree behind an
  ``OwnershipGuard`` (plus ``record_decision`` and, when a lease handler is given,
  ``request_lease``), verifies the worktree and commits the work on the implementer's branch;
* ``integrate_run`` offers the branch to the ``BranchIntegrator`` and commits the checkpoint:
  the merge commit IS the checkpoint when the branch is merged, and a failed attempt is recorded
  otherwise (blocked after 3 in a row, then optionally split by the replanner).
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path

from lha.agent.assembly import lead_dispatcher, lead_verifier, open_lead_sandbox
from lha.agents.integrator import BranchIntegrator, add_worktree, branch_for, commit_worktree
from lha.agents.replanner import Replanner
from lha.agents.reviewer import ReviewResult
from lha.agents.specialists import Implementer
from lha.config import Settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.state import (
    Checklist,
    ChecklistItem,
    Checkpoint,
    DecisionRecord,
    EventRecord,
)
from lha.contracts.tools import ToolContext, ToolDispatcher
from lha.contracts.verify import Check, CheckResult, VerificationResult, ensure_unique_check_names
from lha.coordination.enforcement import OwnershipGuard, changed_paths, effective_ownership
from lha.coordination.leases import LeaseDecision, LeaseHandler
from lha.coordination.ownership import FileOwnershipMap, OwnershipViolation, writer_for_item
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus
from lha.execution.tools import DecisionBuffer, with_decision_tool, with_lease_tool
from lha.obs.otel import agent_span
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.harness_integrity import harness_violations, integrity_result, snapshot_harness
from lha.verify.witnesses import parse_witness, witness_paths

MAX_CONSECUTIVE_FAILURES = 3  # same threshold as the Lead's AgentLoop


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


def diff_since(workdir: str | Path, base: str, head: str) -> str:
    """``git diff base..head`` without the harness's ``.lha/`` files."""
    if not base or not head or base == head:
        return "(no new commits)"
    # ``--no-ext-diff``: the hardening config sets ``diff.external`` to an empty string, and git
    # would otherwise try to run it and die, leaving the reviewer an empty diff.
    diff = git_ops.run_git(
        workdir,
        "diff",
        "--no-ext-diff",
        f"{base}..{head}",
        "--",
        ".",
        ":(exclude).lha",
        check=False,
    )
    return diff or "(empty diff)"


@dataclass
class ImplementerRun:
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
    leases: list[LeaseDecision] = field(default_factory=list)

    def advance(self, to: TicketStatus, note: str = "") -> None:
        self.ticket = self.ticket.transition(to)
        self.tickets.append({"status": to.value, "note": note[:300]})


def new_implementer_run(
    item: ChecklistItem,
    cycle_id: str,
    ownership: FileOwnershipMap,
    *,
    tool_budget: int,
    acceptance: list[str],
) -> ImplementerRun:
    """The run (and its ``created`` ticket) for ``item``'s implementer."""
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
        tool_budget=tool_budget,
        acceptance=acceptance,
    )
    ticket = Ticket(
        id=f"{cycle_id}-{item.id}", contract=contract, item_id=item.id, attempts=item.attempts
    )
    run = ImplementerRun(item=item, cycle_id=cycle_id, writer=writer, ticket=ticket)
    run.tickets.append({"status": TicketStatus.CREATED.value, "note": ""})
    return run


# --- the blackboard and reflections, as committed events -----------------------------------
# Both run paths record a board post and a reflection as anchor events; ``lha orchestrate``
# keeps them in memory too, the durable organization reads them back from the committed log.
BOARD_EVENT = "blackboard"
REFLECTION_EVENT = "reflection"
BOARD_ENTRIES = 6  # newest blackboard entries shown to later rounds
BOARD_ENTRY_CAP = 1_500
REFLECTION_CAP = 2_000


def board_event(author: str, text: str, cycle_id: str = "") -> EventRecord:
    """A ``blackboard`` event for one post (capped as the board caps its entries)."""
    return EventRecord(
        kind=BOARD_EVENT,
        cycle_id=cycle_id,
        payload={"author": author, "text": text[:BOARD_ENTRY_CAP]},
    )


def reflection_event(item_id: str, text: str, cycle_id: str = "") -> EventRecord:
    """A ``reflection`` event: the lesson for ``item_id``'s next attempt."""
    return EventRecord(
        kind=REFLECTION_EVENT,
        cycle_id=cycle_id,
        payload={"item": item_id, "text": text[:REFLECTION_CAP]},
    )


def board_context(events: Sequence[EventRecord]) -> str:
    """The newest committed board posts as the "Team board" block a prompt shows (``""`` if none)."""
    posts = [e for e in events if e.kind == BOARD_EVENT][-BOARD_ENTRIES:]
    if not posts:
        return ""
    lines = [
        f"[{e.payload.get('author', '')}] {str(e.payload.get('text', ''))[:BOARD_ENTRY_CAP]}"
        for e in posts
    ]
    return "Team board (earlier rounds):\n" + "\n---\n".join(lines)


def reflection_for(events: Sequence[EventRecord], item_id: str) -> str:
    """The latest committed reflection for ``item_id`` (``""`` if none)."""
    for event in reversed(events):
        if event.kind == REFLECTION_EVENT and str(event.payload.get("item", "")) == item_id:
            return str(event.payload.get("text", ""))
    return ""


def review_base(events: Sequence[EventRecord], item_id: str, fallback: str) -> str:
    """The commit a review of ``item_id`` should diff from.

    A reopened item's next attempt may change nothing more (the work landed in the first), so
    reviewing only the last cycle shows an empty diff and the reviewer blocks again. The review
    therefore covers everything since the item's FIRST attempt in its current streak: the base
    the first blocking review recorded, until a review approves; ``fallback`` otherwise.
    """
    tracked = ""
    for event in events:
        if event.kind != "review" or str(event.payload.get("item_id", "")) != item_id:
            continue
        if event.payload.get("blocking"):
            tracked = tracked or str(event.payload.get("base") or "")
        else:
            tracked = ""
    return tracked or fallback


def implementer_objective(
    run: ImplementerRun,
    *,
    mission_text: str,
    reflection: str = "",
    decisions_text: str = "",
    briefs: list[str] | None = None,
    board: str = "",
    lease_tool: bool = True,
) -> tuple[str, str]:
    """The implementer's objective (from its contract) and its extra context."""
    contract = run.ticket.contract
    lease = (
        "If the item truly needs a file outside your write-set, call request_lease first. "
        if lease_tool
        else ""
    )
    objective = (
        f"Checklist item [{run.item.id}]: {contract.objective}\n\n"
        f"Your write-set (the ONLY files you may create or modify): "
        f"{', '.join(contract.write_set)}\n"
        f"Boundaries: {'; '.join(contract.boundaries)}\n"
        f"{lease}"
        f"Acceptance: the gating checks {', '.join(contract.acceptance) or '(none)'} must "
        "pass in your worktree. When the item is done, reply with "
        '{"done": true, "summary": "' + contract.output_schema + '"}.'
    )
    parts: list[str] = []
    if mission_text:
        parts.append(mission_text)
    if run.item.last_failure:
        parts.append(f"Previous attempt FAILED:\n{run.item.last_failure[-3000:]}")
    if reflection:
        parts.append(reflection.strip())
    if decisions_text:
        parts.append(decisions_text)
    if briefs:
        parts.append("Research briefs:\n" + "\n---\n".join(briefs))
    if board:
        parts.append(board)
    return objective, "\n\n".join(parts)


def item_checks(
    item: ChecklistItem, mission_checks: list[Check], trusted: Mapping[str, list[str]]
) -> tuple[list[Check], list[CheckResult]]:
    """The mission checks plus the item's witnesses (as the Lead's cycle would gate it), and a
    failing result for each witness that cannot be turned into a check."""
    witnesses: list[Check] = []
    errors: list[CheckResult] = []
    for witness in item.witnesses:
        try:
            witnesses.append(parse_witness(witness, trusted))
        except ValueError as exc:
            errors.append(
                CheckResult(
                    name=witness,
                    passed=False,
                    exit_code=2,
                    output_tail=f"invalid witness: {exc}",
                )
            )
    return ensure_unique_check_names([*mission_checks, *witnesses]), errors


async def implement_in_worktree(
    run: ImplementerRun,
    *,
    settings: Settings,
    workdir: str,
    base: str,
    ownership: FileOwnershipMap,
    model: ModelProvider,
    gate: HITLGate | None,
    allow_egress: bool | None,
    mission_id: str,
    mission_checks: list[Check],
    objective: str,
    extra: str,
    lease: LeaseHandler | None = None,
    on_summary: Callable[[str], None] | None = None,
) -> None:
    """Run one implementer in its own worktree; verify there; commit on its branch.

    Sets ``run.branch`` / ``run.worktree`` first (the caller removes the worktree), then the
    brief, decisions, verification, branch head and the git-layer ownership violations (against
    ``ownership`` as it is after any lease the implementer was granted).
    """
    run.branch = branch_for(run.writer, run.cycle_id)
    run.worktree = await asyncio.to_thread(add_worktree, workdir, branch=run.branch, base=base)
    run.ticket = run.ticket.model_copy(update={"branch": run.branch})
    run.advance(TicketStatus.IN_PROGRESS)
    worktree = run.worktree
    # The item's witness scripts are protected like harness files: a witness the agent can
    # rewrite proves nothing.
    globs = (*settings.harness_globs(), *witness_paths(run.item.witnesses))
    trusted = settings.trusted_check_commands()
    session = await open_lead_sandbox(settings, str(worktree))
    try:
        harness_before = (
            None
            if run.item.allow_harness_edits
            else await asyncio.to_thread(snapshot_harness, worktree, globs)
        )
        buffer = DecisionBuffer()
        # The Lead's tools and human gate, behind the implementer's ownership guard; the lease
        # tool sits outside the guard (asking is always allowed, writing only after a grant).
        guarded: ToolDispatcher = OwnershipGuard(
            lead_dispatcher(settings, gate, allow_egress=allow_egress),
            ownership,
            writers=(run.writer,),
            lease_tool=lease is not None,
        )
        if lease is not None:
            guarded = with_lease_tool(guarded, lease)
        implementer = Implementer(
            model, with_decision_tool(guarded, buffer), max_turns=settings.max_turns_per_cycle
        )
        with agent_span("implement", mission_id=mission_id, item=run.item.id):
            result = await implementer.run(
                objective=objective,
                ctx=ToolContext(mission_id=mission_id, session=session),
                extra_context=extra,
            )
        run.brief = result.brief
        run.tool_calls = result.tool_calls
        run.decisions = list(buffer.records)
        if on_summary is not None:
            on_summary(result.brief)
        run.advance(TicketStatus.AWAITING_VERIFY)
        checks, witness_errors = item_checks(run.item, mission_checks, trusted)
        verification = await lead_verifier(str(worktree), settings).verify(session, checks)
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
        repo=workdir,
    )
    paths = await asyncio.to_thread(changed_paths, worktree, base, run.head)
    run.violations = ownership.violations(writer=run.writer, paths=paths)


@dataclass
class IntegrationReport:
    """What ``integrate_run`` did with one implementer's branch."""

    merged: bool
    reason: str
    head: str  # the checkpoint commit
    before: str  # the mission branch's HEAD before the merge
    status: str  # the item's status after the checkpoint ("split" when split)
    verdict: str
    split_into: list[str]
    checklist: Checklist


#: ``(checklist, blocked item) -> child ids``: splits a newly blocked item (``[]``: no split).
SplitFn = Callable[[Checklist, ChecklistItem], Awaitable[list[str]]]


async def integrate_run(
    run: ImplementerRun,
    *,
    anchor: GitMissionAnchor,
    integrator: BranchIntegrator,
    workdir: str,
    base: str,
    checks: list[Check],
    split: SplitFn | None = None,
    events: list[EventRecord] | None = None,
) -> IntegrationReport:
    """Offer one implementer branch to the integrator and commit the checkpoint.

    ``checks``: the gate for the merged workspace (the mission checks plus the item's
    witnesses). A merged branch is recorded ``done`` and the checkpoint IS the merge commit; the
    item's lease (and those of other finished items) is released in the same commit. Otherwise
    the failed attempt is recorded (``blocked`` after ``MAX_CONSECUTIVE_FAILURES``, then
    ``split`` if ``split`` returns children). ``events`` are added to the checkpoint.
    """
    item = run.item
    own = run.verification
    verified = own is not None and own.all_green
    before = await asyncio.to_thread(git_ops.head_sha, workdir)
    post: VerificationResult | None = None
    if run.error:
        reason = f"implementer failed: {run.error}"
    elif not verified:
        reason = own.failure_report() if own is not None else "the branch was not verified"
    else:
        run.advance(TicketStatus.AWAITING_MERGE)
        integration = await integrator.integrate(
            branch=run.branch,
            branch_head=run.head,
            base=base,
            verified=True,
            violations=run.violations,
            checks=checks,
        )
        post = integration.verification if integration.merged else None
        reason = integration.reason

    checklist = await anchor.read_checklist()
    merged = post is not None
    if post is not None:
        run.advance(TicketStatus.DONE, f"merged {run.branch}")
        checklist.record_success(item.id, [r.name for r in post.results if r.gating and r.passed])
        # The slice is finished: its lease ends in the same commit that merges it (as do those
        # of any other finished item not yet released).
        persisted = await anchor.read_ownership()
        done = [writer_for_item(i.id) for i in checklist.items if i.status == "done"]
        released = effective_ownership(persisted, done)
        if released != persisted:
            anchor.stage_ownership(released)
    else:
        run.advance(TicketStatus.FAILED, reason)
        checklist.record_failure(item.id, reason, max_consecutive_failures=MAX_CONSECUTIVE_FAILURES)
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
        if status == "blocked" and current is not None and split is not None:
            split_into = await split(checklist, current)
            if split_into:
                verb, status = "split", "split"
                note += f"; split into {', '.join(split_into)}"
    shown = post if post is not None else own
    head = await anchor.commit_checkpoint(
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
                *(events or []),
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
                        "leases": [
                            {"path": d.path, "granted": d.granted, "why": d.why} for d in run.leases
                        ],
                    },
                ),
            ],
            commit_message=f"lha: {verb} {item.id} ({item.description})"
            + (f" [merged {run.branch}]" if merged and run.head != base else ""),
        )
    )
    await asyncio.to_thread(integrator.abort)  # never leave a half-finished merge
    return IntegrationReport(
        merged=merged,
        reason=reason,
        head=head,
        before=before,
        status=status,
        verdict=verdict,
        split_into=split_into,
        checklist=checklist,
    )


async def maybe_split(
    checklist: Checklist,
    item: ChecklistItem,
    *,
    settings: Settings,
    model: ModelProvider,
    mission_text: str,
) -> list[str]:
    """Split a newly blocked item with the replanner, within the mission's replan budget
    (the same bounds as the Lead's ``AgentLoop``); returns the child ids ([] if not split)."""
    if settings.max_replans <= 0:
        return []
    if sum(1 for i in checklist.items if i.status == "split") >= settings.max_replans:
        return []
    if item.id.count(".") >= settings.max_split_depth:
        return []
    drafts = await Replanner(model).split(mission_text=mission_text, item=item)
    if len(drafts) < 2:
        return []
    return [child.id for child in checklist.split(item.id, drafts)]


def reopen_for_review(
    checklist: Checklist, item_id: str, review: ReviewResult, *, block: bool = False
) -> ChecklistItem | None:
    """Put a verified-but-review-blocked item back to ``todo`` (``blocked`` when ``block``) with
    the review notes; ``None`` if the item is gone."""
    item = checklist.get(item_id)
    if item is None:
        return None
    item.status = "blocked" if block else "todo"
    item.verified_by = []
    item.notes = review.notes()
    item.last_failure = "reviewer blocked: " + "; ".join(review.blocking_issues or ["unparsed"])
    return item
