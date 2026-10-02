"""File ownership wired end to end: Planner assignment, the tool-level guard, the git-layer check,
the ownership map in the anchor, and ``orchestrate``'s parallel waves with the Integrator.

The models are offline fakes that act on what the orchestrator actually sends them; the tools,
worktrees, merges, verification and checkpoints are all real.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

import pytest

from lha.agents.integrator import (
    BranchIntegrator,
    add_worktree,
    commit_worktree,
    prune_worktrees,
    worktree_root,
)
from lha.agents.orchestrator import Orchestrator, parallel_batch
from lha.agents.planner import Planner, assign_ownership, parse_plan
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.coordination.enforcement import OwnershipGuard, changed_paths, effective_ownership
from lha.coordination.ownership import LEAD, FileOwnershipMap, writer_for_item
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.verifier import DeterministicVerifier

PY = sys.executable
PASS = Check(name="always_green", command=[PY, "-c", "pass"])
_DONE = TurnResult(text='{"done": true, "summary": "done"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
_WRITE_SET = re.compile(r"Your write-set \(the ONLY files you may create or modify\): (.*)")


def _settings(**update: object) -> Settings:
    base = Settings(
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=100.0,
        max_cycles=20,
    )
    return base.model_copy(update=update)


# --- Planner -------------------------------------------------------------------------------------
def test_planner_assigns_disjoint_write_sets_and_serializes_the_rest() -> None:
    text = json.dumps(
        [
            {"description": "models", "files": ["src/models.py", "src/Models.py"]},
            {"description": "api", "files": ["src/api.py"]},
            {"description": "deps", "files": ["pyproject.toml", "src/deps.py"]},
            {"description": "more models", "files": ["src/models.py", "src/extra.py"]},
            {"description": "bad", "files": ["../escape.py"]},
            {"description": "anchor", "files": [".lha/checklist.json"]},
            {"description": "no files"},
            {"description": "uses deps file", "files": ["src/deps.py"], "depends_on": [3]},
        ]
    )
    items, files = parse_plan(text)
    assert files["01"] == ["src/models.py", "src/Models.py"]
    ownership = assign_ownership(items, files)
    by_id = {i.id: i for i in items}
    assert ownership.write_set(writer_for_item("01")) == ["src/models.py"]  # case-folded, once
    assert ownership.write_set(writer_for_item("02")) == ["src/api.py"]
    assert ownership.write_set(writer_for_item("03")) == []  # shared file => serial
    assert "serial: touches shared files ['pyproject.toml']" in by_id["03"].notes
    assert ownership.write_set(writer_for_item("04")) == []  # overlaps 01 => serial after it
    assert by_id["04"].depends_on == ["01"]
    assert "serial after 01" in by_id["04"].notes
    assert "invalid file paths" in by_id["05"].notes
    assert "invalid file paths" in by_id["06"].notes
    assert by_id["07"].notes == ""
    # 08 overlaps the serial item 03 (its non-shared file): serial, already depends on 03.
    assert by_id["08"].depends_on == ["03"]
    assert ownership.write_set(writer_for_item("08")) == []
    assert Checklist(items=items).dependency_errors() == []


@pytest.mark.asyncio
async def test_plan_mission_returns_checklist_and_ownership() -> None:
    model = StubModel(
        script=[
            TurnResult(
                text='[{"description": "a", "files": ["a.py"]}, {"description": "b", '
                '"files": ["b.py"]}]'
            )
        ]
    )
    plan = await Planner(model).plan_mission(title="T", description="D")
    assert [i.id for i in plan.checklist.items] == ["01", "02"]
    assert plan.ownership.owner_of("a.py") == "implementer-01"
    assert plan.files == {"01": ["a.py"], "02": ["b.py"]}
    fallback = await Planner(StubModel(script=[TurnResult(text="nope")])).plan_mission(
        title="T", description="build it"
    )
    assert fallback.ownership.owners == {}
    assert fallback.checklist.items[0].description == "build it"


# --- the map, the guard and the git layer -------------------------------------------------------
def test_release_write_set_and_effective_ownership() -> None:
    owners = FileOwnershipMap()
    owners.assign("a.py", "implementer-01")
    owners.assign("b.py", "implementer-02")
    assert owners.permits_any(writers=(LEAD, "implementer-01"), path="a.py")
    assert not owners.permits_any(writers=(LEAD, "implementer-01"), path="b.py")
    [violation] = owners.violations_any(writers=(LEAD, "implementer-01"), paths=["a.py", "b.py"])
    assert violation.path == "b.py" and violation.owner == "implementer-02"
    effective = effective_ownership(owners, ["implementer-01", LEAD])
    assert effective.owners == {"b.py": "implementer-02"}
    assert owners.owners == {"a.py": "implementer-01", "b.py": "implementer-02"}  # untouched
    assert owners.release("implementer-02") == ["b.py"]
    assert owners.release("nobody") == []


@pytest.mark.asyncio
async def test_ownership_guard_refuses_foreign_writes_with_a_clear_message(
    tmp_path: Path,
) -> None:
    owners = FileOwnershipMap()
    owners.assign("mine.py", "implementer-01")
    owners.assign("theirs.py", "implementer-02")
    inner = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    guard = OwnershipGuard(inner, owners, writers=("implementer-01",))
    session = await LocalSandbox().open(workdir=str(tmp_path))
    ctx = ToolContext(mission_id="m", session=session)

    async def write(path: str) -> tuple[bool, str]:
        result = await guard.dispatch(
            ToolCall(id="1", name="write_file", arguments={"path": path, "content": "x"}), ctx
        )
        return result.ok, result.error or ""

    assert (await write("mine.py"))[0]
    ok, error = await write("./theirs.py")
    assert not ok and "owned by 'implementer-02'" in error and "lease" in error
    ok, error = await write("pyproject.toml")
    assert not ok and "shared file" in error
    ok, error = await write("new.py")
    assert not ok and "outside your write-set" in error
    ok, error = await write("../x.py")
    assert not ok and "escapes the repository root" in error
    assert not (tmp_path / "theirs.py").exists()
    # Reads and argv-only tools are not path-checked here (the git layer covers the shell).
    read = await guard.dispatch(
        ToolCall(id="2", name="read_file", arguments={"path": "mine.py"}), ctx
    )
    assert read.ok
    assert guard.specs() == inner.specs()

    guard.update(ownership=owners, writers=(LEAD, "implementer-02"))
    assert guard.writers == (LEAD, "implementer-02")
    assert (await write("theirs.py"))[0] and (await write("new.py"))[0]
    assert not (await write("mine.py"))[0]


@pytest.mark.asyncio
async def test_worktree_branch_changes_and_integrator(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    base = await anchor.initialize(
        title="T", description="D", items=Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    path = add_worktree(tmp_path, branch="lha/implementer-01/c1", base=base)
    assert path.is_relative_to(worktree_root(tmp_path))
    (path / "a.py").write_text("print('a')\n", encoding="utf-8")
    (path / ".lha" / "checklist.json").write_text("{}", encoding="utf-8")  # never committed
    head = commit_worktree(path, "lha: implementer-01 01")
    assert changed_paths(path, base, head) == ["a.py"]
    assert changed_paths(path, base, base) == []

    session = await LocalSandbox().open(workdir=str(tmp_path))
    fails_with_a = Check(
        name="no_a", command=[PY, "-c", "import os,sys; sys.exit(os.path.exists('a.py'))"]
    )
    integrator = BranchIntegrator(
        workdir=tmp_path, session=session, verifier=DeterministicVerifier(), checks=[fails_with_a]
    )
    refused = await integrator.integrate(
        branch="lha/implementer-01/c1", branch_head=head, base=base, verified=False, violations=[]
    )
    assert not refused.merged and "failed verification" in refused.reason
    violation = FileOwnershipMap().violations(writer="implementer-01", paths=["a.py"])
    refused = await integrator.integrate(
        branch="lha/implementer-01/c1",
        branch_head=head,
        base=base,
        verified=True,
        violations=violation,
    )
    assert not refused.merged and "ownership violation" in refused.reason
    # Verified in the worktree, but red once merged: the merge is aborted, nothing changes.
    red = await integrator.integrate(
        branch="lha/implementer-01/c1", branch_head=head, base=base, verified=True, violations=[]
    )
    assert not red.merged and "merged mission branch" in red.reason
    assert not (tmp_path / "a.py").exists()
    assert git_ops.head_sha(tmp_path) == base

    prune_worktrees(tmp_path)
    assert not path.exists()
    assert not [b for b in git_ops.list_branches(tmp_path) if b.startswith("lha/")]


@pytest.mark.asyncio
async def test_anchor_persists_the_ownership_map(tmp_path: Path) -> None:
    owners = FileOwnershipMap()
    owners.assign("a.py", "implementer-01")
    checklist = Checklist(items=[ChecklistItem(id="01", description="x")])
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=checklist, ownership=owners)
    assert (await GitMissionAnchor(tmp_path).read_ownership()) == owners

    # The agent editing ownership.json is discarded; a staged map is what gets committed.
    (tmp_path / ".lha" / "ownership.json").write_text('{"owners": {"a.py": "lead"}}', "utf-8")
    anchor.stage_ownership(FileOwnershipMap())
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=checklist)
    )
    assert (await GitMissionAnchor(tmp_path).read_ownership()).owners == {}
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c2", progress_summary="- c2", checklist=checklist)
    )
    assert (await anchor.read_ownership()).owners == {}  # the staging is not sticky

    # Re-initializing without a map drops the old file; anchors without one read as empty.
    await anchor.initialize(title="T", description="D", items=checklist)
    assert not (tmp_path / ".lha" / "ownership.json").exists()
    assert (await anchor.read_ownership()).owners == {}


def test_parallel_batch() -> None:
    owners = FileOwnershipMap()
    for n in ("01", "02", "03"):
        owners.assign(f"{n}.py", writer_for_item(n))
    items = Checklist(
        items=[
            ChecklistItem(id="01", description="a"),
            ChecklistItem(id="02", description="b"),
            ChecklistItem(id="03", description="c", depends_on=["01"]),
            ChecklistItem(id="04", description="d"),  # no write-set: serial
        ]
    )
    assert [i.id for i in parallel_batch(items, owners, 3)] == ["01", "02"]
    assert parallel_batch(items, owners, 1) == []
    items.items[1].status = "blocked"
    assert parallel_batch(items, owners, 3) == []  # one candidate is not a wave


# --- orchestrate: parallel waves --------------------------------------------------------------
class _Implementers(StubModel):
    """Plays every implementer: records a decision, writes its write-set, then says done.

    ``extra`` maps an item id to additional actions (e.g. a sneaky shell write).
    """

    def __init__(self, extra: dict[str, list[dict[str, object]]] | None = None) -> None:
        super().__init__(model_name="implementers")
        self.extra = extra or {}
        self.observations: list[str] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        objective = messages[1].content
        item = re.search(r"Checklist item \[(\w+)\]", objective)
        write_set = _WRITE_SET.search(objective)
        assert item is not None and write_set is not None
        paths = [p.strip() for p in write_set.group(1).split(",") if p.strip()]
        actions: list[dict[str, object]] = [
            {
                "tool": "record_decision",
                "arguments": {"decision": f"item {item.group(1)} layout", "rationale": "r"},
            },
            *({"tool": "write_file", "arguments": {"path": p, "content": "x\n"}} for p in paths),
            *self.extra.get(item.group(1), []),
        ]
        self.observations.extend(m.content for m in messages if m.content.startswith("OBSERVATION"))
        turn = sum(1 for m in messages if m.role == "assistant")
        if turn < len(actions):
            return TurnResult(text=json.dumps(actions[turn]))
        return _DONE


def _two_items() -> tuple[Checklist, FileOwnershipMap]:
    checklist = Checklist(
        items=[
            ChecklistItem(id="01", description="write a"),
            ChecklistItem(id="02", description="write b"),
        ]
    )
    owners = FileOwnershipMap()
    owners.assign("a.py", writer_for_item("01"))
    owners.assign("b.py", writer_for_item("02"))
    return checklist, owners


def _events(workdir: Path) -> list[dict[str, object]]:
    text = (workdir / ".lha" / "events.ndjson").read_text(encoding="utf-8")
    return [json.loads(line) for line in text.splitlines() if line.strip()]


@pytest.mark.asyncio
async def test_parallel_wave_merges_verified_branches(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    wrote_code = Check(
        name="wrote_code",
        command=[PY, "-c", "import glob,sys; sys.exit(not glob.glob('*.py'))"],
    )
    orchestrator = Orchestrator(
        _settings(),
        research_per_item=1,
        do_review=True,
        models={
            "implementer": _Implementers(),
            "lead": StubModel(script=[_DONE]),
            "researcher": StubModel(script=[_DONE]),
            "reviewer": StubModel(script=[_APPROVE]),
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Parallel",
        description="two disjoint items",
        checklist=checklist,
        checks=[PASS, wrote_code],
        ownership=owners,
    )
    assert summary.completed and summary.stopped_reason == "complete", summary.stopped_reason
    assert summary.cycles == 2
    assert (tmp_path / "a.py").read_text() == "x\n" and (tmp_path / "b.py").exists()

    log = git_ops.log_oneline(tmp_path, 20)
    merges = [line for line in log if "[merged lha/implementer-" in line]
    assert len(merges) == 2 and all("lha: complete" in line for line in merges)
    # The checkpoint IS the merge commit (the approved review's anchor commit follows it).
    parents = git_ops.run_git(
        tmp_path, "log", "-1", "--pretty=%P", "--grep=lha: complete 02"
    ).split()
    assert len(parents) == 2
    assert log[0].endswith("lha: review approved 02")

    events = _events(tmp_path)
    tickets = [e for e in events if e["kind"] == "ticket"]
    assert [t["payload"]["status"] for t in tickets] == ["done", "done"]  # type: ignore[index]
    history = [h["status"] for h in tickets[0]["payload"]["history"]]  # type: ignore[index]
    assert history == ["created", "in_progress", "awaiting_verify", "awaiting_merge", "done"]
    assert tickets[0]["payload"]["write_set"] == ["a.py"]  # type: ignore[index]

    anchor = GitMissionAnchor(tmp_path)
    decisions = await anchor.read_decisions()
    assert sorted(d.decision for d in decisions) == ["item 01 layout", "item 02 layout"]
    assert (await anchor.verify_decisions()).ok
    # Finished items released their files (committed with the later checkpoints).
    assert (await anchor.read_ownership()).owners == {}
    root = worktree_root(tmp_path)
    assert not root.exists() or not any(root.iterdir())
    assert not [b for b in git_ops.list_branches(tmp_path) if b.startswith("lha/")]
    trace = [json.loads(line)["kind"] for line in summary.trace_jsonl.splitlines()]
    assert "parallel_wave" in trace and trace.count("integration") == 2


@pytest.mark.asyncio
async def test_foreign_writes_are_refused_by_the_guard_and_the_git_layer(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    sneaky = {
        "01": [
            {"tool": "write_file", "arguments": {"path": "b.py", "content": "mine now"}},
            {
                "tool": "run_command",
                "arguments": {"argv": [PY, "-c", "open('b.py', 'w').write('shell')"]},
            },
        ]
    }
    implementers = _Implementers(extra=sneaky)
    orchestrator = Orchestrator(
        _settings(max_cycles=2),
        research_per_item=0,
        do_review=False,
        models={"implementer": implementers, "lead": StubModel(script=[_DONE])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Guard",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.cycles == 2
    refused = [o for o in implementers.observations if "ownership:" in o]
    assert refused and "owned by 'implementer-02'" in refused[0]

    final = await GitMissionAnchor(tmp_path).read_checklist()
    one, two = final.items
    assert one.status == "in_progress"
    assert "ownership violation" in one.last_failure and "b.py" in one.last_failure
    assert two.status == "done"
    assert (tmp_path / "b.py").read_text() == "x\n"  # item 02's content, not the shell write
    assert not (tmp_path / "a.py").exists()  # item 01 was never merged
    tickets = [e for e in _events(tmp_path) if e["kind"] == "ticket"]
    assert tickets[0]["payload"]["status"] == "failed"  # type: ignore[index]
    assert tickets[0]["payload"]["ownership_violations"] == ["b.py"]  # type: ignore[index]
    # Unmerged work's decisions are dropped with it.
    decisions = await GitMissionAnchor(tmp_path).read_decisions()
    assert [d.decision for d in decisions] == ["item 02 layout"]


@pytest.mark.asyncio
async def test_integration_reverifies_the_merged_branch(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    not_both = Check(
        name="not_both",
        command=[
            PY,
            "-c",
            "import os,sys; sys.exit(os.path.exists('a.py') and os.path.exists('b.py'))",
        ],
    )
    orchestrator = Orchestrator(
        _settings(max_cycles=2),
        research_per_item=0,
        do_review=False,
        models={"implementer": _Implementers(), "lead": StubModel(script=[_DONE])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Merge gate",
        description="d",
        checklist=checklist,
        checks=[not_both],
        ownership=owners,
    )
    assert summary.stopped_reason == "max_cycles"
    final = await GitMissionAnchor(tmp_path).read_checklist()
    assert final.items[0].status == "done"
    assert final.items[1].status == "in_progress"
    assert "verification failed on the merged mission branch" in final.items[1].last_failure
    assert (tmp_path / "a.py").exists() and not (tmp_path / "b.py").exists()
    assert not (git_ops.git_dir(tmp_path) / "MERGE_HEAD").exists()


class _Broken(StubModel):
    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        raise RuntimeError("model fell over")


@pytest.mark.asyncio
async def test_a_failing_implementer_is_a_failed_attempt(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    orchestrator = Orchestrator(
        _settings(max_cycles=2),
        research_per_item=0,
        do_review=False,
        models={"implementer": _Broken(), "lead": StubModel(script=[_DONE])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Broken",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.cycles == 2 and not summary.completed
    final = await GitMissionAnchor(tmp_path).read_checklist()
    assert all("implementer failed: RuntimeError" in i.last_failure for i in final.items)
    assert not [b for b in git_ops.list_branches(tmp_path) if b.startswith("lha/")]


class _Scripted(StubModel):
    """A scripted stub that also keeps every message it was shown."""

    def __init__(self, script: list[TurnResult]) -> None:
        super().__init__(script=script)
        self.seen: list[str] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.seen.extend(m.content for m in messages)
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


def _act(tool: str, **arguments: object) -> TurnResult:
    return TurnResult(text=json.dumps({"tool": tool, "arguments": arguments}))


def _trace(trace: str) -> list[dict[str, object]]:
    events = [json.loads(line) for line in trace.splitlines() if line.strip()]
    return [{"kind": e["kind"], **e["data"]} for e in events]


@pytest.mark.asyncio
async def test_serial_lead_is_guarded_audited_and_releases_leases(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    lead = _Scripted(
        [
            _act("write_file", path="b.py", content="lead"),  # refused: 02's leased file
            _act("run_command", argv=[PY, "-c", "open('b.py', 'w').write('shell')"]),
            _DONE,
        ]
    )
    orchestrator = Orchestrator(
        _settings(max_cycles=2),
        research_per_item=0,
        do_review=False,
        max_parallel=1,  # no parallel waves: the Lead works both items serially
        models={"lead": lead},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Serial",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.completed
    assert any("may not write 'b.py'" in text and "implementer-02" in text for text in lead.seen)
    events = _trace(summary.trace_jsonl)
    [audit] = [e for e in events if e["kind"] == "ownership_violation"]
    assert audit["paths"] == ["b.py"] and audit["writer"] == "lead+implementer-01"
    [released] = [e for e in events if e["kind"] == "ownership_released"]
    assert released["paths"] == ["a.py"]
    # 01's lease ended with the next checkpoint; 02's would end with the one after it.
    owners_now = (await GitMissionAnchor(tmp_path).read_ownership()).owners
    assert owners_now == {"b.py": "implementer-02"}


@pytest.mark.asyncio
async def test_parallel_items_are_reviewed_and_can_be_reopened(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    block = TurnResult(
        text='{"done": true, "verdict": "block", "blocking_issues": ["no docstring"]}'
    )
    orchestrator = Orchestrator(
        _settings(max_cycles=3),
        research_per_item=0,
        do_review=True,
        models={
            "implementer": _Implementers(),
            "lead": StubModel(script=[_DONE]),
            "reviewer": StubModel(script=[block, _APPROVE]),
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Review",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    events = _trace(summary.trace_jsonl)
    assert [e["item"] for e in events if e["kind"] == "review_reopened"] == ["01"]
    assert any("lha: review reopened 01" in line for line in git_ops.log_oneline(tmp_path, 20))
    # The reopened item's lease was released when it merged, so the Lead redoes it serially.
    assert summary.completed and summary.cycles == 3


class _TamperingImplementers(_Implementers):
    """Commits an altered decision log while the wave is running."""

    def __init__(self, workdir: Path) -> None:
        super().__init__()
        self._workdir = workdir
        self._done = False

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        if not self._done:
            self._done = True
            bad = {
                "prev": "0" * 64,
                "hash": "f" * 64,
                "record": {"decision": "x", "rationale": "y"},
            }
            path = self._workdir / ".lha" / "decisions.ndjson"
            path.write_text(json.dumps(bad) + "\n", encoding="utf-8")
            git_ops.run_git(self._workdir, "add", "-f", ".lha/decisions.ndjson")
            git_ops.run_git(self._workdir, "commit", "-q", "-m", "forge")
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


@pytest.mark.asyncio
async def test_orchestrator_stops_on_a_tampered_decision_chain(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    orchestrator = Orchestrator(
        _settings(),
        research_per_item=0,
        do_review=False,
        models={
            "implementer": _TamperingImplementers(tmp_path),
            "lead": StubModel(script=[_DONE]),
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Tamper",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.stopped_reason.startswith("decision log failed verification")
    assert not summary.completed
    assert "decision_chain_invalid" in summary.trace_jsonl
    assert not [b for b in git_ops.list_branches(tmp_path) if b.startswith("lha/")]
    assert not (git_ops.git_dir(tmp_path) / "MERGE_HEAD").exists()


def test_cli_orchestrate_passes_the_planned_ownership(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from typer.testing import CliRunner

    from lha.agent.runner import MissionSummary
    from lha.cli import main as cli

    captured: dict[str, object] = {}

    async def fake_run_mission(self: Orchestrator, **kwargs: object) -> MissionSummary:
        captured.update(kwargs)
        return MissionSummary(
            mission_id="m",
            completed=True,
            cycles=1,
            items_done=2,
            items_total=2,
            total_usd=0.0,
            head_sha="abc",
            stopped_reason="complete",
            trace_jsonl="",
        )

    plan = '[{"description": "a", "files": ["a.py"]}, {"description": "b", "files": ["b.py"]}]'
    monkeypatch.setattr(Orchestrator, "run_mission", fake_run_mission)
    monkeypatch.setattr(
        "lha.model.build_provider", lambda *a, **k: StubModel(script=[TurnResult(text=plan)])
    )
    result = CliRunner().invoke(
        cli.app,
        [
            "orchestrate",
            "--task",
            "t",
            "--workdir",
            str(tmp_path),
            "--sandbox",
            "local",
            "--unsafe-local",
            "--no-default-checks",
            "--check",
            "python -c pass",
        ],
    )
    assert result.exit_code == 0, result.output
    ownership = captured["ownership"]
    assert isinstance(ownership, FileOwnershipMap)
    assert ownership.owner_of("b.py") == "implementer-02"
    checklist = captured["checklist"]
    assert isinstance(checklist, Checklist) and len(checklist.items) == 2


@pytest.mark.asyncio
async def test_parallel_items_are_gated_by_witnesses_and_split_when_blocked(
    tmp_path: Path,
) -> None:
    checklist, owners = _two_items()
    for item in checklist.items:
        item.witnesses = ["cmd:test -f never.txt", "go:"]  # a failing and an invalid witness
    split = TurnResult(text='[{"description": "first half"}, {"description": "second half"}]')
    orchestrator = Orchestrator(
        _settings(max_cycles=6),  # three waves of two
        research_per_item=0,
        do_review=False,
        models={"implementer": _Implementers(), "lead": StubModel(script=[split])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Witnessed",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.cycles == 6 and not summary.completed
    final = await GitMissionAnchor(tmp_path).read_checklist()
    by_id = {i.id: i for i in final.items}
    assert by_id["01"].status == "split" and by_id["02"].status == "split"
    assert {"01.1", "01.2", "02.1", "02.2"} <= set(by_id)
    assert "cmd:test -f never.txt" in by_id["01"].last_failure
    assert "invalid witness" in by_id["01"].last_failure
    cycles = [e for e in _events(tmp_path) if e["kind"] == "cycle"]
    assert cycles[-1]["payload"]["split_into"] in (["01.1", "01.2"], ["02.1", "02.2"])  # type: ignore[index]
    assert not (tmp_path / "a.py").exists()  # never merged


def test_planner_witnesses_are_kept_validated_and_never_trusted() -> None:
    """The Planner may propose pytest:/go:/cmd: witnesses; bad syntax and trusted: are dropped
    into the notes (spec/agent/prompts.json pins the exact rows)."""
    items, _files = parse_plan(
        '[{"description": "a", "witnesses": ["pytest:tests/test_a.py::test_x", " go:TestA ", '
        '"pytest:tests/test_a.py::test_x"]}, '
        '{"description": "b", "witnesses": ["trusted:e2e", "go:not an identifier", "cmd:"]}]'
    )
    assert items[0].witnesses == ["pytest:tests/test_a.py::test_x", "go:TestA"]
    assert items[0].notes == ""
    assert items[1].witnesses == []
    assert items[1].notes.startswith("planner dropped invalid witnesses: [")
    assert "trusted:e2e: only the operator may name a trusted check" in items[1].notes
    assert "go:not an identifier: " in items[1].notes and "cmd:: " in items[1].notes
