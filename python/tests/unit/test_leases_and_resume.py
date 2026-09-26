"""Lease granting (``LeaseBroker`` + ``request_lease``) and resuming ``lha orchestrate``.

Real git, worktrees, anchors and checks; the models are scripted fakes.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest
from typer.testing import CliRunner

from lha.agents.orchestrator import MissionResumeError, Orchestrator, anchor_exists
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint
from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.contracts.verify import Check
from lha.coordination.leases import (
    LeaseBroker,
    decide_lease,
    finished_writers,
    lease_handler,
)
from lha.coordination.ownership import LEAD, FileOwnershipMap, LeaseRequest, writer_for_item
from lha.execution.tools import REQUEST_LEASE, with_lease_tool
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from tests.unit.test_ownership_integration import _Implementers, _settings, _two_items

PY = sys.executable
PASS = Check(name="always_green", command=[PY, "-c", "pass"])
_DONE = TurnResult(text='{"done": true, "summary": "did it"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')


def _events(workdir: Path, kind: str) -> list[dict[str, Any]]:
    raw = git_ops.show_at_head(workdir, ".lha/events.ndjson")
    rows = [json.loads(line) for line in raw.splitlines() if line.strip()]
    return [r for r in rows if r["kind"] == kind]


# --- the decision -------------------------------------------------------------------------------
def test_decide_lease_grants_free_files_and_refuses_contended_ones() -> None:
    owners = FileOwnershipMap()
    owners.assign("a.py", "implementer-01")
    owners.assign("b.py", "implementer-02")

    def ask(writer: str, path: str, finished: tuple[str, ...] = ()) -> Any:
        return decide_lease(
            owners, LeaseRequest(writer=writer, path=path, reason=" why "), finished=finished
        )

    free = ask("implementer-01", "src/new.py")
    assert free.granted and free.previous_owner is None and free.why == "it was unassigned"
    assert free.reason == "why" and "lease granted" in free.message()
    assert ask("implementer-01", "./a.py").why == "you already own it"
    taken = ask("implementer-01", "b.py")
    assert not taken.granted and taken.previous_owner == "implementer-02"
    assert "still open" in taken.why and "lease refused" in taken.message()
    done = ask("implementer-01", "B.py", finished=("implementer-02",))
    assert done.granted and done.path == "B.py" and "has finished" in done.why
    assert not ask("implementer-01", "pyproject.toml").granted  # shared: lead only
    assert not ask("implementer-01", ".lha/ownership.json").granted
    assert not ask("implementer-01", ".git/config").granted
    assert "escapes" in ask("implementer-01", "../x.py").why
    assert not ask(LEAD, "c.py").granted
    checklist = Checklist(
        items=[
            ChecklistItem(id="01", description="a", status="done"),
            ChecklistItem(id="02", description="b", status="split"),
            ChecklistItem(id="03", description="c"),
        ]
    )
    assert finished_writers(checklist) == {"implementer-01", "implementer-02"}


# --- the broker: decisions committed to the anchor -----------------------------------------------
@pytest.mark.asyncio
async def test_broker_commits_each_decision_and_only_anchor_files(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=checklist, ownership=owners)
    (tmp_path / "work_in_progress.py").write_text("uncommitted\n")  # must stay uncommitted
    broker = LeaseBroker(anchor)
    granted = await broker.request(
        LeaseRequest(writer="implementer-01", path="lib/x.py", reason="helper"), cycle_id="c1"
    )
    refused = await broker.request(
        LeaseRequest(writer="implementer-01", path="b.py", reason="need it"), cycle_id="c1"
    )
    assert granted.granted and not refused.granted
    persisted = await GitMissionAnchor(tmp_path).read_ownership()
    assert persisted.owner_of("lib/x.py") == "implementer-01"
    assert persisted.owner_of("b.py") == "implementer-02"
    leases = _events(tmp_path, "lease")
    assert [(e["payload"]["path"], e["payload"]["granted"]) for e in leases] == [
        ("lib/x.py", True),
        ("b.py", False),
    ]
    log = git_ops.log_oneline(tmp_path, 5)
    assert "lease granted: lib/x.py (implementer-01)" in log[1]
    assert "lease refused: b.py (implementer-01)" in log[0]
    assert (tmp_path / "work_in_progress.py").exists()
    assert "work_in_progress.py" not in git_ops.run_git(tmp_path, "ls-files")

    # Once item 02 is done its files are free: a later request is granted.
    done = await anchor.read_checklist()
    done.record_success("02", ["check"])
    await anchor.commit_checkpoint(Checkpoint(cycle_id="c2", progress_summary="", checklist=done))
    later = await broker.request(
        LeaseRequest(writer="implementer-01", path="b.py", reason="now"), cycle_id="c3"
    )
    assert later.granted and later.previous_owner is None  # released with the finished item
    assert (await anchor.read_ownership()).owner_of("b.py") == "implementer-01"


@pytest.mark.asyncio
async def test_lease_handler_updates_the_live_map(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=checklist, ownership=owners)
    live = owners.model_copy(deep=True)
    log: list[Any] = []
    handle = lease_handler(
        LeaseBroker(anchor), writer="implementer-01", cycle_id="c1", ownership=live, log=log
    )
    ok, message = await handle("docs/a.md", "document a")
    assert ok and "granted" in message and live.owner_of("docs/a.md") == "implementer-01"
    ok, message = await handle("b.py", "steal")
    assert not ok and "refused" in message and live.owner_of("b.py") == "implementer-02"
    assert [d.granted for d in log] == [True, False]


class _Inner:
    def specs(self) -> list[ToolSpec]:
        return []

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        return ToolResult.success(f"inner:{call.name}")


@pytest.mark.asyncio
async def test_request_lease_tool(tmp_path: Path) -> None:
    calls: list[tuple[str, str]] = []

    async def handler(path: str, reason: str) -> tuple[bool, str]:
        calls.append((path, reason))
        if path == "boom":
            raise RuntimeError("anchor unavailable")
        return path == "ok.py", f"decided {path}"

    dispatcher = with_lease_tool(_Inner(), handler)
    assert [s.name for s in dispatcher.specs()] == [REQUEST_LEASE]
    assert with_lease_tool(dispatcher, handler) is dispatcher
    ctx = ToolContext(mission_id="m", session=None)  # type: ignore[arg-type]

    async def call(args: dict[str, object], name: str = REQUEST_LEASE) -> ToolResult:
        return await dispatcher.dispatch(ToolCall(id="1", name=name, arguments=args), ctx)

    assert (await call({"path": "ok.py", "reason": "r"})).ok
    refused = await call({"path": "no.py", "reason": "r"})
    assert not refused.ok and refused.error == "decided no.py"
    assert not (await call({"path": "x"})).ok  # no reason
    assert "invalid args" in ((await call({"path": 3, "reason": "r"})).error or "")
    assert "non-empty" in ((await call({"path": " ", "reason": "r"})).error or "")
    assert "anchor unavailable" in ((await call({"path": "boom", "reason": "r"})).error or "")
    assert (await call({}, name="read_file")).content == "inner:read_file"
    assert calls == [("ok.py", "r"), ("no.py", "r"), ("boom", "r")]


# --- a lease inside an orchestrate wave ------------------------------------------------------
@pytest.mark.asyncio
async def test_orchestrate_implementer_is_granted_a_lease_mid_wave(tmp_path: Path) -> None:
    checklist, owners = _two_items()
    lease = {
        "01": [
            {"tool": "request_lease", "arguments": {"path": "shared_util.py", "reason": "helper"}},
            {"tool": "write_file", "arguments": {"path": "shared_util.py", "content": "u\n"}},
            {"tool": "request_lease", "arguments": {"path": "b.py", "reason": "steal"}},
        ]
    }
    implementers = _Implementers(extra=lease)
    orchestrator = Orchestrator(
        _settings(),
        research_per_item=0,
        do_review=False,
        models={"implementer": implementers, "lead": StubModel(script=[_DONE])},
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Lease",
        description="d",
        checklist=checklist,
        checks=[PASS],
        ownership=owners,
    )
    assert summary.completed, summary.stopped_reason
    assert (tmp_path / "shared_util.py").read_text() == "u\n"  # leased, written, merged
    assert any("lease granted" in o for o in implementers.observations)
    assert any("lease refused" in o and "implementer-02" in o for o in implementers.observations)
    leases = [(e["payload"]["path"], e["payload"]["granted"]) for e in _events(tmp_path, "lease")]
    assert leases == [("shared_util.py", True), ("b.py", False)]
    tickets = _events(tmp_path, "ticket")
    first = next(t for t in tickets if t["payload"]["item_id"] == "01")
    assert first["payload"]["leases"][0]["path"] == "shared_util.py"
    trace = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    assert [t["data"]["granted"] for t in trace if t["kind"] == "lease"] == [True, False]
    assert (await GitMissionAnchor(tmp_path).read_ownership()).owners == {}  # all released


# --- resume --------------------------------------------------------------------------------------
class _Lead(StubModel):
    """Records a decision, then says done; keeps every prompt it sees."""

    def __init__(self, seen: list[str]) -> None:
        super().__init__(model_name="lead")
        self.seen = seen

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.seen.extend(m.content for m in messages)
        if not any(m.role == "assistant" for m in messages):
            return TurnResult(
                text=json.dumps(
                    {
                        "tool": "record_decision",
                        "arguments": {"decision": f"d{len(self.seen)}", "rationale": "r"},
                    }
                )
            )
        return _DONE


def _serial_org(settings: Settings, seen: list[str]) -> Orchestrator:
    return Orchestrator(
        settings,
        research_per_item=1,
        do_review=True,
        models={
            "lead": _Lead(seen),
            "researcher": StubModel(script=[TurnResult(text='{"done": true, "summary": "B1"}')]),
            "reviewer": StubModel(script=[_APPROVE]),
        },
    )


@pytest.mark.asyncio
async def test_orchestrate_resumes_the_same_mission(tmp_path: Path) -> None:
    checklist = Checklist(
        items=[
            ChecklistItem(id="01", description="first"),
            ChecklistItem(id="02", description="second"),
        ]
    )
    seen: list[str] = []
    first = await _serial_org(_settings(max_cycles=1), seen).run_mission(
        workdir=str(tmp_path),
        title="Resume",
        description="two steps",
        checklist=checklist,
        checks=[PASS],
    )
    assert not first.completed and first.cycles == 1 and first.items_done == 1
    assert anchor_exists(tmp_path)
    (tmp_path / "residue.txt").write_text("left by a crash\n")  # uncommitted, discarded
    decisions_before = await GitMissionAnchor(tmp_path).read_decisions()

    seen.clear()
    second = await _serial_org(_settings(), seen).run_mission(
        workdir=str(tmp_path), checks=[PASS], resume=True
    )
    assert second.completed and second.cycles == 1 and second.items_done == 2
    assert second.mission_id == first.mission_id  # the same mission, not a new one
    assert not (tmp_path / "residue.txt").exists()
    anchor = GitMissionAnchor(tmp_path)
    decisions = await anchor.read_decisions()
    assert decisions[: len(decisions_before)] == decisions_before and len(decisions) == 2
    assert (await anchor.verify_decisions()).ok
    cycles = [e["cycle_id"] for e in _events(tmp_path, "cycle")]
    assert cycles == ["c1", "c2"]  # numbering continues; nothing re-done
    runs = [e["payload"] for e in _events(tmp_path, "orchestrate")]
    assert [(r["run"], r["resumed"]) for r in runs] == [(1, False), (2, True)]
    # The blackboard of the first run (its research brief) reached the resumed Lead.
    assert any("Team board (earlier rounds)" in t and "[researcher:01] B1" in t for t in seen)
    trace = [json.loads(line) for line in second.trace_jsonl.splitlines()]
    assert any(t["kind"] == "resumed" and t["data"]["cycle_offset"] == 1 for t in trace)
    mission = await anchor.read_mission()
    assert mission is not None and mission.title == "Resume"

    with pytest.raises(MissionResumeError):
        await _serial_org(_settings(), seen).run_mission(
            workdir=str(tmp_path / "nowhere"), resume=True
        )
    with pytest.raises(ValueError, match="checklist"):
        await _serial_org(_settings(), seen).run_mission(workdir=str(tmp_path / "x"))


@pytest.mark.asyncio
async def test_resume_restores_reflections_for_open_items(tmp_path: Path) -> None:
    checklist = Checklist(items=[ChecklistItem(id="01", description="first")])
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=checklist)
    from lha.contracts.state import EventRecord

    await anchor.append_event(
        EventRecord(kind="reflection", cycle_id="c7", payload={"item": "01", "text": "REFLECT-01"})
    )
    await anchor.append_event(EventRecord(kind="reflection", payload={"item": "zz", "text": "no"}))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c7", progress_summary="", checklist=checklist)
    )
    seen: list[str] = []
    summary = await _serial_org(_settings(max_cycles=1), seen).run_mission(
        workdir=str(tmp_path), checks=[PASS], resume=True
    )
    assert summary.completed
    assert any("REFLECT-01" in text for text in seen)
    assert [e["cycle_id"] for e in _events(tmp_path, "cycle")] == ["c8"]


# --- the CLI ------------------------------------------------------------------------------------
def test_cli_orchestrate_resume_and_refusals(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from lha.agent.runner import MissionSummary
    from lha.cli import main as cli

    calls: list[dict[str, Any]] = []

    async def fake_run_mission(self: Orchestrator, **kwargs: Any) -> MissionSummary:
        calls.append(kwargs)
        return MissionSummary("m", True, 1, 1, 1, 0.0, "sha", "complete", "")

    monkeypatch.setattr(Orchestrator, "run_mission", fake_run_mission)
    runner = CliRunner()
    base = ["--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true"]
    work = tmp_path / "w"

    missing = runner.invoke(cli.app, ["orchestrate", "--resume", "--workdir", str(work), *base])
    assert missing.exit_code == 2 and "no mission anchor" in missing.output
    no_task = runner.invoke(cli.app, ["orchestrate", "--workdir", str(work), *base])
    assert no_task.exit_code == 2 and "--task" in no_task.output

    import asyncio

    asyncio.run(
        GitMissionAnchor(work).initialize(
            title="T",
            description="D",
            items=Checklist(items=[ChecklistItem(id="01", description="x")]),
        )
    )
    again = runner.invoke(cli.app, ["orchestrate", "--task", "t", "--workdir", str(work), *base])
    assert again.exit_code == 2 and "--resume" in again.output
    resumed = runner.invoke(cli.app, ["orchestrate", "--resume", "--workdir", str(work), *base])
    assert resumed.exit_code == 0, resumed.output
    assert calls[-1]["resume"] is True and calls[-1]["workdir"] == str(work)


def test_cli_mission_start_passes_the_org_options(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from lha.cli import main as cli

    settings = Settings(model_backend="stub")
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    plan = '[{"description": "a", "files": ["a.py"]}, {"description": "b", "files": ["b.py"]}]'
    monkeypatch.setattr(
        "lha.model.build_provider", lambda *a, **k: StubModel(script=[TurnResult(text=plan)])
    )
    started: list[Any] = []

    class _Client:
        async def start_workflow(self, _run: Any, inp: Any, **_kw: Any) -> None:
            started.append(inp)

    async def connect_client(_settings: Settings) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)
    runner = CliRunner()
    work = tmp_path / "w"
    result = runner.invoke(
        cli.app,
        [
            *["mission-start", "--task", "t", "--workdir", str(work)],
            *["--research", "2", "--review", "--max-parallel", "3"],
        ],
    )
    assert result.exit_code == 0, result.output
    inp = started[0]
    assert (inp.research_per_item, inp.review, inp.max_parallel) == (2, True, 3)
    owners = asyncio_run(GitMissionAnchor(work).read_ownership())
    assert owners.owner_of("b.py") == writer_for_item("02")  # waves need the planned ownership

    default = runner.invoke(
        cli.app, ["mission-start", "--task", "t", "--workdir", str(tmp_path / "d")]
    )
    assert default.exit_code == 0, default.output
    assert (started[1].research_per_item, started[1].review, started[1].max_parallel) == (
        0,
        False,
        0,
    )
    bad = runner.invoke(cli.app, ["mission-start", "--task", "t", "--research", "9"])
    assert bad.exit_code == 2

    checklist = tmp_path / "c.json"
    checklist.write_text(json.dumps({"items": [{"id": "01", "description": "x"}]}))
    imported = runner.invoke(
        cli.app,
        [
            *["mission-start", "--checklist", str(checklist), "--workdir", str(tmp_path / "i")],
            *["--max-parallel", "2"],
        ],
    )
    assert imported.exit_code == 0, imported.output
    assert "no parallel wave can run" in imported.output


def asyncio_run(coro: Any) -> Any:
    import asyncio

    return asyncio.run(coro)


def test_cli_orchestrate_runs_a_checklist_file(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from lha.agent.runner import MissionSummary
    from lha.cli import main as cli

    calls: list[dict[str, Any]] = []

    async def fake_run_mission(self: Orchestrator, **kwargs: Any) -> MissionSummary:
        calls.append(kwargs)
        return MissionSummary("m", True, 1, 1, 1, 0.0, "sha", "complete", "")

    def no_planning(*_a: Any, **_k: Any) -> Any:
        raise AssertionError("--checklist must not plan")

    monkeypatch.setattr(Orchestrator, "run_mission", fake_run_mission)
    monkeypatch.setattr("lha.model.build_provider", no_planning)
    roadmap = tmp_path / "roadmap.md"
    roadmap.write_text("# Greeter\n\n## Build\n\n- [ ] say hello\n", encoding="utf-8")
    base = ["--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true"]
    runner = CliRunner()
    work = tmp_path / "w"
    result = runner.invoke(
        cli.app, ["orchestrate", "--checklist", str(roadmap), "--workdir", str(work), *base]
    )
    assert result.exit_code == 0, result.output
    call = calls[-1]
    assert call["title"] == "Greeter" and call.get("ownership") is None
    assert [i.description for i in call["checklist"].items] == ["say hello"]
    asyncio_run(
        GitMissionAnchor(work).initialize(title="T", description="D", items=call["checklist"])
    )
    both = runner.invoke(
        cli.app,
        ["orchestrate", "--resume", "--checklist", str(roadmap), "--workdir", str(work), *base],
    )
    assert both.exit_code == 2 and "--checklist cannot be combined with --resume" in both.output
