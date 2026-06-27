"""The integrated inner agent loop: gather → act (tool loop) → verify → checkpoint.

This is where the model, tools, sandbox, verifier, and durable anchor come together for ONE
cycle of work on ONE checklist item. It is deliberately decoupled from Temporal (so it's unit
testable on its own); the durable spine calls it from inside an activity.

Harness truth (the agent can never write its own verdict):
  * The checklist is loaded from the committed anchor at cycle start and owned IN MEMORY by the
    loop; ``commit_checkpoint`` rewrites ``.lha/`` from that state, discarding agent edits.
  * An item is marked ``done`` ONLY when the deterministic verifier returns ``passed`` — which
    requires at least one gating check. Zero gating checks => ``unverified`` => not done.
  * Pre-existing test-harness files are hashed at cycle start; modifying/deleting them adds a
    failing ``harness_integrity`` check (and the files are reverted) unless the item allows it.
  * The model's "done" only ends the acting phase. Unparseable or truncated (``max_tokens``)
    replies get a corrective turn — they never count as done.
  * A failed attempt keeps the item ``in_progress``; after ``max_consecutive_failures`` in a row it
    becomes ``blocked`` (not re-picked), so independent items proceed and the mission can't burn
    forever on one item. No actionable item + not complete => ``is_deadlocked``.
  * An item's own ``witnesses`` gate it on top of the mission checks (``lha.verify.witnesses``); an
    unresolvable witness is a failing check, never a skipped one.
  * With a ``replanner``, a newly blocked item is split into smaller children (bounded by
    ``max_replans`` per mission and ``max_split_depth``); the parent's witnesses gate the last one.
  * A FAILED attempt's code is never committed to the mission's branch: its work tree is saved as
    ``refs/lha/attempts/<mission>/<cycle>`` and the checkout returns to the last verified state,
    so the next attempt starts clean (it is told what was rolled back and where it is kept). Only
    the anchor (checklist, progress, events: the failure record) is committed.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from lha.agent.prompt import build_messages, corrective_message
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, EventRecord
from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult
from lha.contracts.verify import (
    Check,
    CheckResult,
    VerificationResult,
    Verifier,
    ensure_unique_check_names,
)
from lha.obs.events import TraceRecorder
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.harness_integrity import (
    HarnessSnapshot,
    harness_violations,
    integrity_result,
    snapshot_harness,
    violated_paths,
)
from lha.verify.trusted import candidate_commit
from lha.verify.witnesses import parse_witness

if TYPE_CHECKING:
    from lha.agent.claude_code_engine import ClaudeCodeEngine
    from lha.agents.replanner import Replanner
    from lha.memory.service import CycleMemory

_OBSERVATION_CAP = 4000
_TRUNCATED_STOP_REASONS = frozenset({"max_tokens", "length", "model_length"})


@dataclass
class Action:
    """A parsed model action: a tool call, a done signal, or an invalid reply (``error`` set)."""

    done: bool = False
    tool: str | None = None
    arguments: dict[str, object] = field(default_factory=dict)
    summary: str = ""
    # Non-empty when the reply could not be interpreted; the loop sends a corrective turn.
    error: str = ""

    @property
    def is_valid(self) -> bool:
        return not self.error


@dataclass
class CycleOutcome:
    """The result of one agent cycle.

    ``is_complete`` means every item is verified done. ``is_deadlocked`` means nothing is
    actionable but the mission is NOT complete (blocked items / unsatisfiable deps) — callers
    must stop (or escalate to a human) rather than report completion.
    """

    item_id: str | None
    advanced: bool
    verified: bool
    is_complete: bool
    head_sha: str
    tool_calls: int
    turns: int
    verdict: str = ""  # "passed" | "failed" | "unverified" | "" (no item worked)
    is_deadlocked: bool = False
    item_blocked: bool = False  # this cycle's failure pushed the item to ``blocked``
    item_split: bool = False  # ...and the replanner replaced it with smaller child items
    reason: str = ""  # deadlock reason / failure summary
    items_done: int = 0
    items_total: int = 0


def _extract_json(text: str) -> dict[str, object] | None:
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end == -1 or end < start:
        return None
    try:
        parsed = json.loads(text[start : end + 1])
    except ValueError:
        return None
    return parsed if isinstance(parsed, dict) else None


def is_truncated(stop_reason: str | None) -> bool:
    """True when the provider stopped because it ran out of output tokens."""
    return (stop_reason or "").lower() in _TRUNCATED_STOP_REASONS


def parse_action(
    text: str, native_tool_calls: list[ToolCall], *, stop_reason: str | None = None
) -> Action:
    """Interpret a model turn as an Action (native tool-calls take precedence over JSON text).

    Unparseable replies and replies cut off at ``max_tokens`` are NOT done: they come back with
    ``error`` set (and ``done=False``) so the caller can ask for a well-formed action.
    """
    if is_truncated(stop_reason):
        return Action(error="reply was truncated at the output-token limit", summary=text[:200])
    if native_tool_calls:
        first = native_tool_calls[0]
        return Action(tool=first.name, arguments=dict(first.arguments))
    obj = _extract_json(text)
    if obj is None:
        return Action(error="no JSON object found in reply", summary=text[:200])
    if obj.get("done") is True:
        return Action(done=True, summary=str(obj.get("summary", "")))
    tool = obj.get("tool")
    if isinstance(tool, str) and tool:
        raw_args = obj.get("arguments", {})
        arguments: dict[str, object] = raw_args if isinstance(raw_args, dict) else {}
        return Action(tool=tool, arguments=arguments)
    return Action(error='JSON has neither "tool" nor "done": true', summary=text[:200])


@dataclass
class _Acting:
    """What the acting phase of a cycle did (built-in turns or a Claude Code session)."""

    tool_calls: int = 0
    tools_used: list[str] = field(default_factory=list)
    done_summary: str = ""
    turns: int = 0
    core: VerificationResult | None = None  # verifier verdict, before harness integrity
    dirty: bool = True  # workspace changed since the last verification (or none ran yet)


class AgentLoop:
    """Runs one verified cycle of work using a model + tools + sandbox + verifier + anchor.

    Budget: pass a metered provider (``CostMeter.wrap(...)``) as ``model`` so every call is
    budget-checked and recorded; a refusal raises ``BudgetExceeded`` out of ``run_cycle`` before
    anything is checkpointed.
    """

    def __init__(
        self,
        *,
        model: ModelProvider,
        dispatcher: ToolDispatcher,
        verifier: Verifier,
        anchor: GitMissionAnchor,
        recorder: TraceRecorder | None = None,
        max_turns: int = 8,
        max_consecutive_failures: int = 3,
        verify_on_done: bool = True,
        trusted_checks: Mapping[str, list[str]] | None = None,
        harness_globs: tuple[str, ...] = (),
        replanner: Replanner | None = None,
        max_replans: int = 0,
        max_split_depth: int = 2,
        memory: CycleMemory | None = None,
        engine: ClaudeCodeEngine | None = None,
    ) -> None:
        self._model = model
        self._dispatcher = dispatcher
        self._verifier = verifier
        self._anchor = anchor
        self._recorder = recorder
        self._max_turns = max_turns
        self._max_failures = max_consecutive_failures
        self._verify_on_done = verify_on_done
        self._trusted = dict(trusted_checks or {})
        self._harness_globs = harness_globs
        self._replanner = replanner
        self._max_replans = max_replans
        self._max_split_depth = max_split_depth
        # Optional tiered memory (``lha.memory.service``): recalled into the task message before
        # the first turn, fed the committed outcome after the checkpoint. Never fails a cycle.
        self._memory = memory
        # Optional ``claude_code`` lead engine: the acting phase runs as one ``claude -p``
        # session instead of ``model`` turns (``model`` still meters it and serves the
        # replanner). Everything before and after acting is the same.
        self._engine = engine

    async def run_cycle(
        self,
        *,
        ctx: ToolContext,
        mission_id: str,
        cycle_id: str,
        anchor_text: str | None = None,
        checks: list[Check] | None = None,
    ) -> CycleOutcome:
        checklist = await self._anchor.read_checklist()  # committed truth, owned in memory
        snapshot = await self._anchor.read_situational_awareness()
        mission = await self._anchor.read_mission()

        if checklist.is_complete or checklist.is_deadlocked:
            return self._idle_outcome(checklist, snapshot.head_sha)
        item = checklist.next_actionable()
        if item is None:  # unreachable: not complete and not deadlocked implies an item
            return self._idle_outcome(checklist, snapshot.head_sha)
        item = checklist.start(item.id)
        witness_checks, witness_errors = self._witness_checks(item)
        gate = ensure_unique_check_names([*(checks or []), *witness_checks])

        workdir = str(self._anchor.workdir)  # the host checkout (in Docker, not /workspace)
        harness_before: HarnessSnapshot | None = None
        tampered: list[str] = []  # sticky for the whole cycle, even after files are reverted
        if not item.allow_harness_edits:
            harness_before = await asyncio.to_thread(snapshot_harness, workdir, self._harness_globs)

        memory_text = ""
        if self._memory is not None:
            memory_text = await self._memory.recall(
                mission_id=mission_id, cycle_id=cycle_id, item=item, snapshot=snapshot
            )
        messages = build_messages(
            anchor_text=anchor_text or "",
            mission_text=mission.render_anchor() if mission else snapshot.anchor_text(),
            snapshot=snapshot,
            item=item,
            specs=self._dispatcher.specs(),
            memory_text=memory_text,
            engine=self._engine is not None,
        )
        self._emit("cycle_started", mission_id, cycle_id, item_id=item.id)

        acting = _Acting()
        if self._engine is not None:
            await self._engine_session(
                acting,
                messages,
                ctx,
                mission_id,
                cycle_id,
                gate,
                witness_errors,
                harness_before,
                tampered,
            )
        else:
            await self._model_turns(
                acting,
                messages,
                ctx,
                mission_id,
                cycle_id,
                gate,
                witness_errors,
                harness_before,
                tampered,
            )
        tool_calls, tools_used, done_summary, turns = (
            acting.tool_calls,
            acting.tools_used,
            acting.done_summary,
            acting.turns,
        )
        core, dirty = acting.core, acting.dirty

        if core is None or dirty:
            core = self._with_errors(await self._verifier.verify(ctx.session, gate), witness_errors)
        # Always re-check the harness right before committing (tampering needs no tool call).
        verification = await self._with_integrity(core, ctx, harness_before, tampered)

        mission_text = mission.render_anchor() if mission else snapshot.anchor_text()
        head = await self._checkpoint(
            checklist, item, cycle_id, verification, tool_calls, mission_text, mission_id
        )
        self._emit(
            "checkpoint",
            mission_id,
            cycle_id,
            head_sha=head,
            verified=verification.all_green,
            verdict=verification.verdict,
        )
        if self._memory is not None:
            from lha.memory.service import CycleObservation

            await self._memory.observe_cycle(
                CycleObservation(
                    mission_id=mission_id,
                    cycle_id=cycle_id,
                    item_id=item.id,
                    item_description=item.description,
                    verdict=verification.verdict,
                    verified=verification.all_green,
                    status=item.status,
                    attempts=item.attempts,
                    head_sha=head,
                    before_head=snapshot.head_sha,
                    failure="" if verification.all_green else verification.failure_report(),
                    done_summary=done_summary,
                    tools=tools_used,
                )
            )
        return CycleOutcome(
            item_id=item.id,
            advanced=True,
            verified=verification.all_green,
            is_complete=checklist.is_complete,
            head_sha=head,
            tool_calls=tool_calls,
            turns=turns,
            verdict=verification.verdict,
            is_deadlocked=checklist.is_deadlocked,
            item_blocked=item.status == "blocked",
            item_split=item.status == "split",
            reason=checklist.deadlock_reason() or item.last_failure[:500],
            items_done=checklist.items_done,
            items_total=checklist.items_total,
        )

    # --- internals ---------------------------------------------------------------------
    async def _model_turns(
        self,
        acting: _Acting,
        messages: list[ModelMessage],
        ctx: ToolContext,
        mission_id: str,
        cycle_id: str,
        gate: list[Check],
        witness_errors: list[CheckResult],
        harness_before: HarnessSnapshot | None,
        tampered: list[str],
    ) -> None:
        """The built-in lead: model turns, tool calls and verify-on-done, up to ``max_turns``."""
        for turns in range(1, self._max_turns + 1):
            acting.turns = turns
            result = await self._model.complete(messages)
            self._trace_turn(result, mission_id, cycle_id)

            if result.tool_calls and not is_truncated(result.stop_reason):
                calls = [
                    c if c.id else c.model_copy(update={"id": f"{cycle_id}-{turns}-{i}"})
                    for i, c in enumerate(result.tool_calls)
                ]
                # Native round trip: the assistant turn carries its tool_use blocks and each
                # result answers its call by id, so providers see real tool_use/tool_result pairs.
                messages.append(
                    ModelMessage(role="assistant", content=result.text, tool_calls=calls)
                )
                for call in calls:  # execute EVERY requested tool call, in order
                    observation = await self._dispatch(call, ctx, mission_id, cycle_id)
                    messages.append(
                        ModelMessage(role="tool", content=observation, tool_call_id=call.id)
                    )
                    acting.tool_calls += 1
                    acting.tools_used.append(call.name)
                acting.dirty = True
                continue

            action = parse_action(result.text, [], stop_reason=result.stop_reason)
            messages.append(ModelMessage(role="assistant", content=result.text))
            if not action.is_valid:
                self._emit("invalid_reply", mission_id, cycle_id, reason=action.error)
                messages.append(corrective_message(action.error))
                continue
            if action.done:
                acting.done_summary = action.summary
                if not self._verify_on_done or turns == self._max_turns:
                    return
                acting.core = self._with_errors(
                    await self._verifier.verify(ctx.session, gate), witness_errors
                )
                acting.dirty = False
                verification = await self._with_integrity(
                    acting.core, ctx, harness_before, tampered
                )
                if verification.verdict != "failed":
                    return  # green, or unverified (more turns can't create a gate)
                messages.append(
                    ModelMessage(
                        role="user",
                        content=(
                            "VERIFICATION FAILED — the item is not done yet. Fix the cause and "
                            f"signal done again.\n{verification.failure_report()}"
                        ),
                    )
                )
                continue

            call = ToolCall(
                id=f"{cycle_id}-{turns}", name=action.tool or "", arguments=action.arguments
            )
            messages.append(
                ModelMessage(
                    role="user", content=await self._dispatch(call, ctx, mission_id, cycle_id)
                )
            )
            acting.tool_calls += 1
            acting.tools_used.append(call.name)
            acting.dirty = True

    async def _engine_session(
        self,
        acting: _Acting,
        messages: list[ModelMessage],
        ctx: ToolContext,
        mission_id: str,
        cycle_id: str,
        gate: list[Check],
        witness_errors: list[CheckResult],
        harness_before: HarnessSnapshot | None,
        tampered: list[str],
    ) -> None:
        """The ``claude_code`` lead: one Claude Code session using LHA's tools and ``verify``."""
        engine = self._engine
        assert engine is not None

        async def dispatch(call: ToolCall) -> ToolResult:
            result = await self._dispatch_result(call, ctx, mission_id, cycle_id)
            acting.dirty = True
            return result

        async def verify() -> VerificationResult:
            acting.core = self._with_errors(
                await self._verifier.verify(ctx.session, gate), witness_errors
            )
            acting.dirty = engine.native  # native edits bypass the dispatcher: re-verify after
            return await self._with_integrity(acting.core, ctx, harness_before, tampered)

        run = await engine.run(
            messages=messages,
            cwd=str(self._anchor.workdir),
            cycle_id=cycle_id,
            specs=self._dispatcher.specs(),
            dispatch=dispatch,
            verify=verify,
            meter=self._model,
        )
        acting.tool_calls = run.tool_calls
        acting.tools_used = run.tools_used
        acting.turns = run.turns
        acting.done_summary = run.summary[:2000]
        if run.edits_untracked:
            acting.dirty = True
        self._emit(
            "claude_code_session",
            mission_id,
            cycle_id,
            turns=run.turns,
            tool_calls=run.tool_calls,
            session_id=run.session_id or "",
            stopped=run.stopped or "",
        )

    def _idle_outcome(self, checklist: Checklist, head_sha: str) -> CycleOutcome:
        return CycleOutcome(
            item_id=None,
            advanced=False,
            verified=False,
            is_complete=checklist.is_complete,
            head_sha=head_sha,
            tool_calls=0,
            turns=0,
            is_deadlocked=checklist.is_deadlocked,
            reason=checklist.deadlock_reason(),
            items_done=checklist.items_done,
            items_total=checklist.items_total,
        )

    def _trace_turn(self, result: TurnResult, mission_id: str, cycle_id: str) -> None:
        # Spend is recorded by the metered provider (``lha.governor.metering``), not here.
        self._emit(
            "llm_turn",
            mission_id,
            cycle_id,
            model=result.usage.model,
            output_tokens=result.usage.output_tokens,
            stop_reason=result.stop_reason or "",
        )

    async def _dispatch_result(
        self, call: ToolCall, ctx: ToolContext, mission_id: str, cycle_id: str
    ) -> ToolResult:
        tool_result = await self._dispatcher.dispatch(call, ctx)
        self._emit("tool_call", mission_id, cycle_id, tool=call.name, ok=tool_result.ok)
        return tool_result

    async def _dispatch(
        self, call: ToolCall, ctx: ToolContext, mission_id: str, cycle_id: str
    ) -> str:
        tool_result = await self._dispatch_result(call, ctx, mission_id, cycle_id)
        observation = (tool_result.content or tool_result.error or "")[:_OBSERVATION_CAP]
        return f"OBSERVATION ({call.name}, tool_use_id={call.id}): {observation}"

    def _witness_checks(self, item: ChecklistItem) -> tuple[list[Check], list[CheckResult]]:
        """The item's witnesses as checks, plus a failing result for each unusable witness."""
        checks: list[Check] = []
        errors: list[CheckResult] = []
        for witness in item.witnesses:
            try:
                checks.append(parse_witness(witness, self._trusted))
            except ValueError as exc:
                errors.append(
                    CheckResult(
                        name=witness,
                        passed=False,
                        exit_code=2,
                        output_tail=f"invalid witness: {exc}",
                    )
                )
        return checks, errors

    @staticmethod
    def _with_errors(
        verification: VerificationResult, errors: list[CheckResult]
    ) -> VerificationResult:
        return verification.with_results(errors) if errors else verification

    async def _with_integrity(
        self,
        verification: VerificationResult,
        ctx: ToolContext,
        harness_before: HarnessSnapshot | None,
        tampered: list[str],
    ) -> VerificationResult:
        """Add a failing ``harness_integrity`` result if pre-existing harness files changed."""
        if harness_before is None:
            return verification
        after = await asyncio.to_thread(
            snapshot_harness, str(self._anchor.workdir), self._harness_globs
        )
        violations = harness_violations(harness_before, after)
        if violations:
            # Revert the tampering so it is never committed (nor the next cycle's baseline).
            await self._anchor.restore_from_head(violated_paths(violations))
            tampered.extend(v for v in violations if v not in tampered)
        if not tampered:
            return verification
        return verification.with_results([integrity_result(tampered)])

    async def _checkpoint(
        self,
        checklist: Checklist,
        item: ChecklistItem,
        cycle_id: str,
        verification: VerificationResult,
        tool_calls: int,
        mission_text: str = "",
        mission_id: str = "",
    ) -> str:
        split_into: list[str] = []
        rolled_back: list[str] = []
        if verification.all_green:
            checklist.record_success(
                item.id, [r.name for r in verification.results if r.gating and r.passed]
            )
            verb, note = "complete", "verified"
        else:
            ref = f"refs/lha/attempts/{mission_id or 'mission'}/{cycle_id}"
            rolled_back = await asyncio.to_thread(self._roll_back_attempt, ref)
            report = verification.failure_report()
            if rolled_back:
                shown = ", ".join(rolled_back[:20]) + (" ..." if len(rolled_back) > 20 else "")
                report += (
                    "\n\nThis attempt's changes were rolled back to the last verified state "
                    f"(kept at {ref}): {shown}. Start again from the committed code."
                )
            checklist.record_failure(item.id, report, max_consecutive_failures=self._max_failures)
            verb = "block" if item.status == "blocked" else "attempt"
            note = f"{verification.verdict} (attempt {item.attempts}, status {item.status})"
            if item.status == "blocked":
                split_into = await self._maybe_split(checklist, item, mission_text)
                if split_into:
                    verb = "split"
                    note += f"; split into {', '.join(split_into)}"
        return await self._anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=cycle_id,
                progress_summary=f"- {cycle_id} [{item.id}] {item.description}: {note}",
                checklist=checklist,
                events=[
                    *self._gate_events(cycle_id),
                    EventRecord(
                        kind="cycle",
                        cycle_id=cycle_id,
                        payload={
                            "item_id": item.id,
                            "verified": verification.all_green,
                            "verdict": verification.verdict,
                            "status": item.status,
                            "tool_calls": tool_calls,
                            "split_into": split_into,
                            "rolled_back": rolled_back,
                            "checks": [
                                {
                                    "name": r.name,
                                    "passed": r.passed,
                                    "gating": r.gating,
                                    "exit_code": r.exit_code,
                                    "duration_s": round(r.duration_s, 3),
                                }
                                for r in verification.results
                            ],
                        },
                    ),
                ],
                commit_message=f"lha: {verb} {item.id} ({item.description})",
            )
        )

    def _roll_back_attempt(self, ref: str) -> list[str]:
        """Save the work tree under ``ref`` and restore HEAD; return the files that changed.

        Anchor files are excluded from the list (the checkpoint rewrites them anyway).
        """
        workdir = self._anchor.workdir
        snapshot = candidate_commit(workdir, message=f"lha: failed attempt ({ref})")
        changed = [
            path
            for path in git_ops.run_git(
                workdir, "diff", "--name-only", "HEAD", snapshot
            ).splitlines()
            if path and not path.startswith(".lha/")
        ]
        if not changed:
            return []
        git_ops.run_git(workdir, "update-ref", ref, snapshot)
        git_ops.discard_changes(workdir)
        return changed

    async def _maybe_split(
        self, checklist: Checklist, item: ChecklistItem, mission_text: str
    ) -> list[str]:
        """Split a newly blocked item via the replanner, within the mission's replan budget."""
        if self._replanner is None or self._max_replans <= 0:
            return []
        if sum(1 for i in checklist.items if i.status == "split") >= self._max_replans:
            return []
        if item.id.count(".") >= self._max_split_depth:
            return []
        drafts = await self._replanner.split(mission_text=mission_text, item=item)
        if len(drafts) < 2:
            return []
        return [child.id for child in checklist.split(item.id, drafts)]

    def _gate_events(self, cycle_id: str) -> list[EventRecord]:
        """Human-gate answers/reminders the dispatcher kept this cycle (committed with it)."""
        drain = getattr(self._dispatcher, "drain_events", None)
        if not callable(drain):
            return []
        events: list[EventRecord] = drain()
        return [e if e.cycle_id else e.model_copy(update={"cycle_id": cycle_id}) for e in events]

    def _emit(self, kind: str, mission_id: str, cycle_id: str, **data: object) -> None:
        if self._recorder is not None:
            self._recorder.record(kind, mission_id=mission_id, cycle_id=cycle_id, **data)
