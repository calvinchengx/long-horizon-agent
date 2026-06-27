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
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass, field

from lha.agent.prompt import build_messages, corrective_message
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, EventRecord
from lha.contracts.tools import ToolContext, ToolDispatcher
from lha.contracts.verify import Check, VerificationResult, Verifier, ensure_unique_check_names
from lha.obs.events import TraceRecorder
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.harness_integrity import (
    HarnessSnapshot,
    harness_violations,
    integrity_result,
    snapshot_harness,
    violated_paths,
)

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
    ) -> None:
        self._model = model
        self._dispatcher = dispatcher
        self._verifier = verifier
        self._anchor = anchor
        self._recorder = recorder
        self._max_turns = max_turns
        self._max_failures = max_consecutive_failures
        self._verify_on_done = verify_on_done

    async def run_cycle(
        self,
        *,
        ctx: ToolContext,
        mission_id: str,
        cycle_id: str,
        anchor_text: str | None = None,
        checks: list[Check] | None = None,
    ) -> CycleOutcome:
        gate = ensure_unique_check_names(checks or [])
        checklist = await self._anchor.read_checklist()  # committed truth, owned in memory
        snapshot = await self._anchor.read_situational_awareness()
        mission = await self._anchor.read_mission()

        if checklist.is_complete or checklist.is_deadlocked:
            return self._idle_outcome(checklist, snapshot.head_sha)
        item = checklist.next_actionable()
        if item is None:  # unreachable: not complete and not deadlocked implies an item
            return self._idle_outcome(checklist, snapshot.head_sha)
        item = checklist.start(item.id)

        workdir = ctx.session.workdir
        harness_before: HarnessSnapshot | None = None
        tampered: list[str] = []  # sticky for the whole cycle, even after files are reverted
        if not item.allow_harness_edits:
            harness_before = await asyncio.to_thread(snapshot_harness, workdir)

        messages = build_messages(
            anchor_text=anchor_text or "",
            mission_text=mission.render_anchor() if mission else snapshot.anchor_text(),
            snapshot=snapshot,
            item=item,
            specs=self._dispatcher.specs(),
        )
        self._emit("cycle_started", mission_id, cycle_id, item_id=item.id)

        tool_calls = 0
        turns = 0
        core: VerificationResult | None = None  # verifier verdict, before harness integrity
        dirty = True  # workspace changed since the last verification (or none ran yet)
        for turns in range(1, self._max_turns + 1):
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
                    tool_calls += 1
                dirty = True
                continue

            action = parse_action(result.text, [], stop_reason=result.stop_reason)
            messages.append(ModelMessage(role="assistant", content=result.text))
            if not action.is_valid:
                self._emit("invalid_reply", mission_id, cycle_id, reason=action.error)
                messages.append(corrective_message(action.error))
                continue
            if action.done:
                if not self._verify_on_done or turns == self._max_turns:
                    break
                core = await self._verifier.verify(ctx.session, gate)
                dirty = False
                verification = await self._with_integrity(core, ctx, harness_before, tampered)
                if verification.verdict != "failed":
                    break  # green, or unverified (more turns can't create a gate)
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
            tool_calls += 1
            dirty = True

        if core is None or dirty:
            core = await self._verifier.verify(ctx.session, gate)
        # Always re-check the harness right before committing (tampering needs no tool call).
        verification = await self._with_integrity(core, ctx, harness_before, tampered)

        head = await self._checkpoint(checklist, item, cycle_id, verification, tool_calls)
        self._emit(
            "checkpoint",
            mission_id,
            cycle_id,
            head_sha=head,
            verified=verification.all_green,
            verdict=verification.verdict,
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
            reason=checklist.deadlock_reason() or item.last_failure[:500],
            items_done=checklist.items_done,
            items_total=len(checklist.items),
        )

    # --- internals ---------------------------------------------------------------------
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
            items_total=len(checklist.items),
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

    async def _dispatch(
        self, call: ToolCall, ctx: ToolContext, mission_id: str, cycle_id: str
    ) -> str:
        tool_result = await self._dispatcher.dispatch(call, ctx)
        self._emit("tool_call", mission_id, cycle_id, tool=call.name, ok=tool_result.ok)
        observation = (tool_result.content or tool_result.error or "")[:_OBSERVATION_CAP]
        return f"OBSERVATION ({call.name}, tool_use_id={call.id}): {observation}"

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
        after = await asyncio.to_thread(snapshot_harness, ctx.session.workdir)
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
    ) -> str:
        if verification.all_green:
            checklist.record_success(
                item.id, [r.name for r in verification.results if r.gating and r.passed]
            )
            verb, note = "complete", "verified"
        else:
            checklist.record_failure(
                item.id,
                verification.failure_report(),
                max_consecutive_failures=self._max_failures,
            )
            verb = "block" if item.status == "blocked" else "attempt"
            note = f"{verification.verdict} (attempt {item.attempts}, status {item.status})"
        return await self._anchor.commit_checkpoint(
            Checkpoint(
                cycle_id=cycle_id,
                progress_summary=f"- {cycle_id} [{item.id}] {item.description}: {note}",
                checklist=checklist,
                events=[
                    EventRecord(
                        kind="cycle",
                        cycle_id=cycle_id,
                        payload={
                            "item_id": item.id,
                            "verified": verification.all_green,
                            "verdict": verification.verdict,
                            "status": item.status,
                            "tool_calls": tool_calls,
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
                    )
                ],
                commit_message=f"lha: {verb} {item.id} ({item.description})",
            )
        )

    def _emit(self, kind: str, mission_id: str, cycle_id: str, **data: object) -> None:
        if self._recorder is not None:
            self._recorder.record(kind, mission_id=mission_id, cycle_id=cycle_id, **data)
