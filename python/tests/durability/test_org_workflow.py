"""The multi-agent organization on the durable path (``MissionWorkflow`` + ``org_round``).

Against Temporal's time-skipping test server, with real git, worktrees, sandboxed checks and
checkpoints; the models are scripted. They prove that a mission that opts in gets:

1. research fan-out as child workflows before a round, with failures surfaced (gate log +
   committed ``research`` event), and the briefs in the Lead's prompt;
2. parallel implementer waves: one activity per implementer in its own worktree, integrated one
   branch at a time by the ``BranchIntegrator``; every integration commit is a checkpoint (a
   merge commit) with ``cycle`` / ``ticket`` events;
3. independent review after every verified item; a blocking review reopens the item;
4. the options are validated, and with all of them off nothing changes (the replay tests pin
   that);

and that the org activities are retry-safe: a repeated implementer attempt returns the committed
branch, a repeated integration or review commits nothing twice, and the third blocking review in
a row blocks the item instead of reopening it forever.
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

import pytest
from temporalio import activity
from temporalio.client import WorkflowFailureError
from temporalio.exceptions import ApplicationError
from temporalio.testing import WorkflowEnvironment
from temporalio.worker import Worker

from lha.config import Settings
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, SituationSnapshot
from lha.coordination.ownership import FileOwnershipMap, writer_for_item
from lha.durable.activities import make_cycle_activity
from lha.durable.org_activities import (
    _integrate_branch,
    _plan_round,
    _review_cycle,
    _run_implementer,
    make_org_activities,
)
from lha.durable.org_round import PATCH_ORG, org_config_error, org_enabled
from lha.durable.signals import QUERY_GATE_LOG
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.types import (
    ImplementerInput,
    IntegrateInput,
    MissionInput,
    ReviewInput,
    RoundInput,
    SubAgentInput,
    SubAgentOutput,
)
from lha.durable.workflows import MissionWorkflow
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from tests.durability._support import CHECK_COMMANDS, SETTINGS, working_model, write_turn
from tests.durability.test_durable_spine import (
    ROW_ACTIVITY,
    _healthy,
    _snapshot_activity,
    _unblock_activity,
)

_TIMEOUT_S = 40
_DONE = TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn")
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
_BLOCK = TurnResult(text='{"done": true, "verdict": "block", "blocking_issues": ["no tests"]}')


async def init_org_mission(
    workdir: Path, n: int, *, owned: bool | int = True, **fields: object
) -> MissionInput:
    """``n`` independent items; the first ``owned`` (all with ``True``) each own
    ``work/<id>.txt`` (wave candidates), the rest have no write-set (serial)."""
    items = [ChecklistItem(id=f"{i:02d}", description=f"task {i}") for i in range(1, n + 1)]
    ownership = FileOwnershipMap()
    count = n if owned is True else int(owned)
    for item in items[:count]:
        ownership.assign(f"work/{item.id}.txt", writer_for_item(item.id))
    await GitMissionAnchor(workdir).initialize(
        title="Org mission",
        description="durable organization",
        items=Checklist(items=items),
        ownership=ownership if count else None,
    )
    base: dict[str, object] = {
        "mission_id": f"org-{workdir.name}",
        "workdir": str(workdir),
        "max_cycles": 20,
        "check_commands": CHECK_COMMANDS,
    }
    base.update(fields)
    return MissionInput(**base)  # type: ignore[arg-type]


_research_calls: list[SubAgentInput] = []


@activity.defn(name="run_subagent")
async def fake_researcher(inp: SubAgentInput) -> SubAgentOutput:
    _research_calls.append(inp)
    if inp.objective.startswith("Find the tests"):
        raise ApplicationError("search backend down (401)", non_retryable=True)
    return SubAgentOutput(
        role=inp.role_name, brief=f"BRIEF[{inp.objective}]", tool_calls=0, turns=1
    )


class _Recorder(StubModel):
    """A scripted model that keeps every prompt it was sent."""

    def __init__(self, script: list[TurnResult], seen: list[str]) -> None:
        super().__init__(script=script)
        self.seen = seen

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.seen.extend(m.content for m in messages)
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


def _reviewers(verdicts: list[TurnResult]):
    """A reviewer factory handing out one scripted verdict per review."""
    queue = list(verdicts)

    def factory(_settings: Settings, _snapshot: SituationSnapshot) -> ModelProvider:
        return StubModel(script=[queue.pop(0) if queue else _APPROVE])

    return factory


def org_worker(env: WorkflowEnvironment, queue: str, **factories: object) -> Worker:
    lead = factories.get("lead", working_model)
    return Worker(
        env.client,
        task_queue=queue,
        workflows=[MissionWorkflow, SubAgentWorkflow],
        activities=[
            make_cycle_activity(settings=SETTINGS, model_factory=lead),  # type: ignore[arg-type]
            *make_org_activities(
                settings=SETTINGS,
                implementer_factory=factories.get("implementer", working_model),  # type: ignore[arg-type]
                reviewer_factory=factories.get("reviewer", _reviewers([])),  # type: ignore[arg-type]
                lead_factory=working_model,
            ),
            fake_researcher,
            _healthy,
            _unblock_activity,
            _snapshot_activity,
            ROW_ACTIVITY,
        ],
    )


def _events(workdir: Path, kind: str) -> list[dict[str, object]]:
    raw = git_ops.show_at_head(workdir, ".lha/events.ndjson")
    events = [json.loads(line) for line in raw.splitlines() if line.strip()]
    return [e for e in events if e["kind"] == kind]


async def _run(env: WorkflowEnvironment, queue: str, inp: MissionInput):
    handle = await env.client.start_workflow(
        MissionWorkflow.run, inp, id=f"mission:{inp.mission_id}", task_queue=queue
    )
    result = await asyncio.wait_for(handle.result(), _TIMEOUT_S)
    return handle, result


@pytest.mark.asyncio
async def test_parallel_wave_with_research_and_review_completes(tmp_path: Path) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 2, research_per_item=1, review=True, max_parallel=2)
    _research_calls.clear()
    async with await WorkflowEnvironment.start_time_skipping() as env, org_worker(env, "org-wave"):
        handle, result = await _run(env, "org-wave", inp)
        gate_log = await handle.query(QUERY_GATE_LOG)
    assert result.completed and result.cycles == 2 and result.items_done == 2
    assert (work / "work" / "01.txt").exists() and (work / "work" / "02.txt").exists()
    merges = [line for line in git_ops.log_oneline(work, 30) if "[merged lha/implementer-" in line]
    assert len(merges) == 2  # each integration commit is a checkpoint
    tickets = _events(work, "ticket")
    assert [t["payload"]["status"] for t in tickets] == ["done", "done"]  # type: ignore[index]
    history = [h["status"] for h in tickets[0]["payload"]["history"]]  # type: ignore[index]
    assert history == ["created", "in_progress", "awaiting_verify", "awaiting_merge", "done"]
    reviews = _events(work, "review")
    assert [r["payload"]["verdict"] for r in reviews] == ["approve", "approve"]  # type: ignore[index]
    research = _events(work, "research")
    assert sorted(r["payload"]["item"] for r in research) == ["01", "02"]  # type: ignore[index]
    assert {c.role_name for c in _research_calls} == {"researcher"}
    assert len(_research_calls) == 2 and all(c.budget_usd is None for c in _research_calls)
    assert any("parallel wave 01, 02" in line for line in gate_log)
    # Worktrees and branches are gone; the ownership map was released as items finished.
    assert not [b for b in git_ops.list_branches(work) if b.startswith("lha/")]
    assert (await GitMissionAnchor(work).read_ownership()).owners == {}


@pytest.mark.asyncio
async def test_serial_round_research_failures_surface_and_review_reopens(tmp_path: Path) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 1, owned=False, research_per_item=3, review=True)
    seen: list[str] = []

    def lead(_settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
        assert snapshot.active_item is not None
        return _Recorder([write_turn(f"work/{len(seen)}.txt"), _DONE], seen)

    _research_calls.clear()
    reviewer = _reviewers([_BLOCK, _APPROVE])
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        org_worker(env, "org-serial", lead=lead, reviewer=reviewer),
    ):
        handle, result = await _run(env, "org-serial", inp)
        gate_log = await handle.query(QUERY_GATE_LOG)
    assert result.completed and result.cycles == 2
    assert len(_research_calls) == 6  # 3 per round, 2 rounds
    assert any("research for 01 failed" in line and "401" in line for line in gate_log)
    assert any("review of 01 (c1) reopened it" in line for line in gate_log)
    research = _events(work, "research")
    assert [r["payload"]["failed"] for r in research] == [1, 1]  # type: ignore[index]
    assert "search backend down" in research[0]["payload"]["failures"][0]  # type: ignore[index]
    assert any("BRIEF[Find context relevant to: task 1]" in text for text in seen)
    reviews = _events(work, "review")
    assert [r["payload"]["reopened"] for r in reviews] == [True, False]  # type: ignore[index]
    item = (await GitMissionAnchor(work).read_checklist()).items[0]
    assert item.status == "done" and "reviewer blocked" not in item.last_failure


@pytest.mark.asyncio
async def test_org_options_are_validated(tmp_path: Path) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 1, research_per_item=9)
    assert "research_per_item" in org_config_error(inp)
    assert org_enabled(inp) and not org_enabled(MissionInput(mission_id="m", workdir="."))
    assert org_config_error(MissionInput(mission_id="m", workdir=".", max_parallel=99))
    assert PATCH_ORG == "lha-durable-org-v1"
    async with await WorkflowEnvironment.start_time_skipping() as env, org_worker(env, "org-bad"):
        with pytest.raises(WorkflowFailureError):
            await _run(env, "org-bad", inp)


def _implementer_failing(item_id: str, error: type[Exception], times: int):
    """Implementer factory whose model cannot be built for ``item_id`` ``times`` times."""
    left = [times]

    def factory(settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
        assert snapshot.active_item is not None
        if snapshot.active_item.id == item_id and left[0] > 0:
            left[0] -= 1
            raise error("model endpoint unavailable")
        return working_model(settings, snapshot)

    return factory


@pytest.mark.asyncio
async def test_a_failed_implementer_is_a_failed_attempt_and_the_item_goes_serial(
    tmp_path: Path,
) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 2, max_parallel=2)
    failing = _implementer_failing("02", ValueError, 1)  # ValueError: a non-retryable config error
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        org_worker(env, "org-fail", implementer=failing),
    ):
        _handle, result = await _run(env, "org-fail", inp)
    assert result.completed and result.cycles == 3  # 01 merged, 02 failed, 02 by the Lead
    tickets = _events(work, "ticket")
    assert [t["payload"]["status"] for t in tickets] == ["done", "failed"]  # type: ignore[index]
    cycles = _events(work, "cycle")
    assert [c["payload"]["item_id"] for c in cycles] == ["01", "02", "02"]  # type: ignore[index]
    assert (work / "work" / "02.txt").exists()


@pytest.mark.asyncio
async def test_an_implementer_outage_parks_the_mission_after_integrating_the_rest(
    tmp_path: Path,
) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 2, max_parallel=2, park_initial_seconds=30)
    outage = _implementer_failing("02", RuntimeError, 5)  # every retry of the first round fails
    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        org_worker(env, "org-outage", implementer=outage),
    ):
        handle, result = await _run(env, "org-outage", inp)
        history = await handle.fetch_history()
    assert result.completed and result.cycles == 2
    scheduled = [
        e.activity_task_scheduled_event_attributes.activity_type.name
        for e in history.events
        if e.HasField("activity_task_scheduled_event_attributes")
    ]
    assert scheduled.count("integrate_branch") == 1  # 01 integrated before parking
    assert "check_mission_health" in scheduled  # parked on the outage
    assert scheduled.index("integrate_branch") < scheduled.index("check_mission_health")
    assert [c["payload"]["item_id"] for c in _events(work, "cycle")] == ["01", "02"]  # type: ignore[index]


@pytest.mark.asyncio
async def test_a_review_that_cannot_run_leaves_the_item_done(tmp_path: Path) -> None:
    work = tmp_path / "work"
    inp = await init_org_mission(work, 1, owned=False, review=True)

    def broken(_settings: Settings, _snapshot: SituationSnapshot) -> ModelProvider:
        raise ValueError("no reviewer model configured")

    async with (
        await WorkflowEnvironment.start_time_skipping() as env,
        org_worker(env, "org-noreview", reviewer=broken),
    ):
        handle, result = await _run(env, "org-noreview", inp)
        gate_log = await handle.query(QUERY_GATE_LOG)
    assert result.completed and result.cycles == 1
    assert any("review of 01 (c1) failed" in line and "unreviewed" in line for line in gate_log)
    assert _events(work, "review") == []


# --- the activities, called directly (retry safety) --------------------------------------------
@pytest.mark.asyncio
async def test_plan_round_picks_a_wave_or_the_next_item(tmp_path: Path) -> None:
    inp = await init_org_mission(tmp_path / "w", 3)
    plan = await _plan_round(RoundInput(mission_id="m", workdir=inp.workdir, max_parallel=2))
    assert plan.parallel and [i.item_id for i in plan.items] == ["01", "02"]
    serial = await _plan_round(RoundInput(mission_id="m", workdir=inp.workdir, max_parallel=0))
    assert not serial.parallel and [i.item_id for i in serial.items] == ["01"]
    assert serial.head_sha == git_ops.head_sha(inp.workdir)


def _lease_then_write(extra_path: str):
    """Implementer model: asks for a lease on ``extra_path``, writes it and its own file."""

    def factory(_settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
        assert snapshot.active_item is not None
        own = f"work/{snapshot.active_item.id}.txt"
        lease = TurnResult(
            tool_calls=[
                ToolCall(
                    id="l1",
                    name="request_lease",
                    arguments={"path": extra_path, "reason": "the item needs a helper"},
                )
            ],
            stop_reason="tool_use",
        )
        return StubModel(script=[lease, write_turn(extra_path, "helper"), write_turn(own), _DONE])

    return factory


@pytest.mark.asyncio
async def test_implementer_and_integration_are_retry_safe_and_honour_leases(
    tmp_path: Path,
) -> None:
    work = tmp_path / "w"
    await init_org_mission(work, 2)
    base = git_ops.head_sha(work)
    impl = ImplementerInput(
        mission_id="m",
        workdir=str(work),
        cycle_id="c1",
        item_id="01",
        base_sha=base,
        check_commands=CHECK_COMMANDS,
    )
    factory = _lease_then_write("lib/helper.txt")
    out = await _run_implementer(impl, settings=SETTINGS, model_factory=factory)
    assert not out.error and out.branch == "lha/implementer-01/c1" and out.head
    assert json.loads(out.verification_json)["all_green"]
    assert [json.loads(d)["granted"] for d in out.leases] == [True]
    # The grant is committed at once (ownership + a lease event), only .lha/ in that commit.
    assert (await GitMissionAnchor(work).read_ownership()).owner_of("lib/helper.txt") == (
        "implementer-01"
    )
    lease_commit = git_ops.run_git(work, "show", "--name-only", "--pretty=format:", "HEAD")
    assert set(lease_commit.split()) <= {
        ".lha/ownership.json",
        ".lha/events.ndjson",
        ".lha/progress.md",
    }
    assert _events(work, "lease")[0]["payload"]["path"] == "lib/helper.txt"  # type: ignore[index]
    # A retried attempt (e.g. after a crash before reporting) returns the committed branch.
    again = await _run_implementer(impl, settings=SETTINGS, model_factory=working_model)
    assert again == out
    # Nothing of the implementer's work is in the mission branch until it is integrated.
    assert not (work / "lib" / "helper.txt").exists()

    integrate = IntegrateInput(
        mission_id="m",
        workdir=str(work),
        cycle_id="c1",
        item_id="01",
        base_sha=base,
        output=out,
        check_commands=CHECK_COMMANDS,
    )
    first = await _integrate_branch(integrate, settings=SETTINGS, model_factory=working_model)
    assert first.advanced and first.verdict == "passed" and first.items_done == 1
    assert (work / "lib" / "helper.txt").read_text() == "helper"  # the leased file merged
    head = git_ops.head_sha(work)
    repeat = await _integrate_branch(integrate, settings=SETTINGS, model_factory=working_model)
    assert repeat.note == "already integrated by a previous attempt"
    assert git_ops.head_sha(work) == head  # nothing committed twice
    # The finished writer's files (including the lease) are released.
    owners = (await GitMissionAnchor(work).read_ownership()).owners
    assert owners == {"work/02.txt": "implementer-02"}


@pytest.mark.asyncio
async def test_a_lease_owned_by_an_open_item_is_refused(tmp_path: Path) -> None:
    work = tmp_path / "w"
    await init_org_mission(work, 2)
    impl = ImplementerInput(
        mission_id="m",
        workdir=str(work),
        cycle_id="c1",
        item_id="01",
        base_sha=git_ops.head_sha(work),
        check_commands=CHECK_COMMANDS,
    )
    out = await _run_implementer(
        impl, settings=SETTINGS, model_factory=_lease_then_write("work/02.txt")
    )
    [decision] = [json.loads(d) for d in out.leases]
    assert not decision["granted"] and "implementer-02" in decision["why"]
    # It wrote the file anyway (the guard refused write_file; nothing reached the branch).
    changed = git_ops.run_git(work, "diff", "--name-only", f"{impl.base_sha}..{out.head}")
    assert "work/02.txt" not in changed.split()
    refused = _events(work, "lease")[0]["payload"]
    assert refused["granted"] is False  # type: ignore[index]


@pytest.mark.asyncio
async def test_failed_implementer_is_recorded_as_a_failed_attempt(tmp_path: Path) -> None:
    work = tmp_path / "w"
    await init_org_mission(work, 2)
    result = await _integrate_branch(
        IntegrateInput(
            mission_id="m",
            workdir=str(work),
            cycle_id="c1",
            item_id="01",
            base_sha=git_ops.head_sha(work),
            error="MissionConfigError: cannot open the sandbox",
            check_commands=CHECK_COMMANDS,
        ),
        settings=SETTINGS,
        model_factory=working_model,
    )
    assert result.advanced and result.verdict == "failed"
    item = (await GitMissionAnchor(work).read_checklist()).items[0]
    assert item.status == "in_progress" and "cannot open the sandbox" in item.last_failure
    [ticket] = _events(work, "ticket")
    assert ticket["payload"]["status"] == "failed"  # type: ignore[index]


@pytest.mark.asyncio
async def test_review_is_exactly_once_and_repeated_blocks_block_the_item(tmp_path: Path) -> None:
    work = tmp_path / "w"
    await init_org_mission(work, 1, owned=False)
    anchor = GitMissionAnchor(work)
    for n in range(1, 4):
        checklist = await anchor.read_checklist()
        checklist.record_success("01", ["check"])
        (work / f"f{n}.txt").write_text("x")
        head = await anchor.commit_checkpoint(
            Checkpoint(cycle_id=f"c{n}", progress_summary=f"- c{n}", checklist=checklist)
        )
        review = ReviewInput(
            mission_id="m", workdir=str(work), cycle_id=f"c{n}", item_id="01", head_sha=head
        )
        result = await _review_cycle(review, settings=SETTINGS, model_factory=_reviewers([_BLOCK]))
        assert result.verdict == "review_blocked"
        assert result.item_blocked == (n == 3)
        again = await _review_cycle(review, settings=SETTINGS, model_factory=_reviewers([]))
        assert again.note == "already reviewed by a previous attempt"
    item = (await anchor.read_checklist()).items[0]
    assert item.status == "blocked" and "no tests" in item.last_failure
    assert [r["payload"]["blocked"] for r in _events(work, "review")] == [False, False, True]  # type: ignore[index]
