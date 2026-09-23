"""The hash-chained decision log as the mission anchor's ``.lha/decisions.ndjson``.

Covers: the chain format with a legacy (pre-chain) prefix, the anchor appending through the chain
and verifying it on read, tampering stopping a mission, the ``record_decision`` tool in the agent
loop (committed by the cycle checkpoint and shown to the next cycle), and ``lha decisions``.
"""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

import pytest
from typer.testing import CliRunner

from lha.agent.assembly import build_lead_loop
from lha.agent.prompt import render_decisions
from lha.agent.runner import DECISION_CHAIN_STOP, run_mission_local
from lha.cli import main as cli
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, DecisionRecord
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.coordination.decision_log import (
    GENESIS_HASH,
    DecisionChainError,
    DecisionLog,
    _chain_hash,
    encode_link,
    parse_chain,
    verify_chain,
)
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import (
    RECORD_DECISION,
    DecisionBuffer,
    DecisionToolDispatcher,
    default_local_tools,
    with_decision_tool,
)
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

PASS = Check(name="always_green", command=[sys.executable, "-c", "pass"])
runner = CliRunner()


def _legacy(decision: str) -> str:
    return DecisionRecord(decision=decision, rationale="r").model_dump_json()


def _checklist() -> Checklist:
    return Checklist(items=[ChecklistItem(id="01", description="one")])


def _commit_decisions(workdir: Path, text: str, message: str = "edit decisions") -> None:
    (workdir / ".lha" / "decisions.ndjson").write_text(text, encoding="utf-8")
    git_ops.run_git(workdir, "add", "-f", ".lha/decisions.ndjson")
    git_ops.run_git(workdir, "commit", "-q", "-m", message)


# --- the chain format --------------------------------------------------------------------------
def test_legacy_prefix_is_folded_into_the_chain_and_sealed() -> None:
    legacy = [_legacy("old one"), _legacy("old two")]
    running = GENESIS_HASH
    for line in legacy:
        running = _chain_hash(running, json.loads(line))
    chained, _digest = encode_link(running, DecisionRecord(decision="new", rationale="r"))
    data = ("\n".join([*legacy, chained]) + "\n").encode()

    check = verify_chain(data)
    assert check.ok and check.checked == 3 and check.legacy == 2
    contents = parse_chain(data)
    assert [r.decision for r in contents.records] == ["old one", "old two", "new"]
    assert contents.legacy == 2

    # Editing a legacy line after the chain began breaks the first chained line's prev.
    edited = [_legacy("rewritten"), legacy[1], chained]
    check = verify_chain(("\n".join(edited) + "\n").encode())
    assert not check.ok and "line 3: prev-hash mismatch" in check.problem

    # A legacy (unchained) line inserted after the chain began is tampering.
    inserted = [*legacy, chained, _legacy("sneaky")]
    check = verify_chain(("\n".join(inserted) + "\n").encode())
    assert not check.ok and "unchained record after the chain began" in check.problem


def test_legacy_only_log_verifies_but_reports_no_protection() -> None:
    check = verify_chain((_legacy("a") + "\n" + _legacy("b") + "\n").encode())
    assert check.ok and check.legacy == 2 and check.checked == 2


def test_decision_log_appends_onto_a_legacy_file(tmp_path: Path) -> None:
    path = tmp_path / "d.ndjson"
    path.write_text(_legacy("old") + "\n", encoding="utf-8")
    log = DecisionLog(str(path))
    log.append(DecisionRecord(decision="new", rationale="r"))
    assert [r.decision for r in log.read()] == ["old", "new"]
    check = log.verify()
    assert check.ok and check.legacy == 1 and check.checked == 2


# --- the anchor --------------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_anchor_chains_decisions_and_stamps_the_cycle(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    assert anchor.record_decision(DecisionRecord(decision="use sqlite", rationale="1 node")) == 1
    assert [d.decision for d in anchor.pending_decisions] == ["use sqlite"]
    await anchor.commit_checkpoint(
        Checkpoint(
            cycle_id="c1",
            progress_summary="- c1",
            checklist=_checklist(),
            decisions=[DecisionRecord(decision="pin deps", rationale="r", cycle_id="c0")],
        )
    )
    assert anchor.pending_decisions == []
    lines = (tmp_path / ".lha" / "decisions.ndjson").read_text(encoding="utf-8").splitlines()
    envelopes = [json.loads(line) for line in lines]
    assert [e["record"]["decision"] for e in envelopes] == ["use sqlite", "pin deps"]
    assert envelopes[0]["prev"] == GENESIS_HASH
    assert envelopes[1]["prev"] == envelopes[0]["hash"]
    assert [e["record"]["cycle_id"] for e in envelopes] == ["c1", "c0"]  # stamped / kept

    fresh = GitMissionAnchor(tmp_path)  # a restart
    snap = await fresh.read_situational_awareness()
    assert [d.decision for d in snap.last_decisions] == ["use sqlite", "pin deps"]
    assert [d.decision for d in await fresh.read_decisions()] == ["use sqlite", "pin deps"]
    check = await fresh.verify_decisions()
    assert check.ok and check.checked == 2 and check.legacy == 0


@pytest.mark.asyncio
async def test_snapshot_carries_only_the_newest_five(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    for n in range(7):
        anchor.record_decision(DecisionRecord(decision=f"d{n}", rationale="r"))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
    )
    snap = await anchor.read_situational_awareness()
    assert [d.decision for d in snap.last_decisions] == ["d2", "d3", "d4", "d5", "d6"]


@pytest.mark.asyncio
async def test_legacy_anchor_still_loads_and_gets_chained(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    # What an anchor written before the chain existed looks like: bare records.
    _commit_decisions(tmp_path, _legacy("legacy a") + "\n" + _legacy("legacy b") + "\n")

    snap = await GitMissionAnchor(tmp_path).read_situational_awareness()
    assert [d.decision for d in snap.last_decisions] == ["legacy a", "legacy b"]

    anchor = GitMissionAnchor(tmp_path)
    anchor.record_decision(DecisionRecord(decision="chained", rationale="r"))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c2", progress_summary="- c2", checklist=_checklist())
    )
    raw = (tmp_path / ".lha" / "decisions.ndjson").read_text(encoding="utf-8").splitlines()
    assert len(raw) == 3 and "prev" in json.loads(raw[2])
    check = await anchor.verify_decisions()
    assert check.ok and check.legacy == 2 and check.checked == 3

    # The first chained line now seals the legacy prefix.
    _commit_decisions(tmp_path, "\n".join([_legacy("forged"), raw[1], raw[2]]) + "\n")
    with pytest.raises(DecisionChainError, match="prev-hash mismatch"):
        await GitMissionAnchor(tmp_path).read_situational_awareness()


@pytest.mark.asyncio
async def test_tampered_chain_refuses_reads_and_checkpoints(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    anchor.record_decision(DecisionRecord(decision="original", rationale="r"))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
    )
    envelope = json.loads((tmp_path / ".lha" / "decisions.ndjson").read_text(encoding="utf-8"))
    envelope["record"]["decision"] = "rewritten history"
    _commit_decisions(tmp_path, json.dumps(envelope) + "\n")

    check = await anchor.verify_decisions()
    assert not check.ok and "hash mismatch" in check.problem
    with pytest.raises(DecisionChainError, match="refuses to continue"):
        await anchor.read_situational_awareness()
    with pytest.raises(DecisionChainError):
        await anchor.read_decisions()
    head = git_ops.head_sha(tmp_path)
    with pytest.raises(DecisionChainError):
        await anchor.commit_checkpoint(
            Checkpoint(cycle_id="c2", progress_summary="- c2", checklist=_checklist())
        )
    assert git_ops.head_sha(tmp_path) == head  # nothing was committed on top of it


@pytest.mark.asyncio
async def test_committed_torn_line_is_treated_as_tampering(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    line, _ = encode_link(GENESIS_HASH, DecisionRecord(decision="d", rationale="r"))
    _commit_decisions(tmp_path, line)  # no trailing newline
    check = await anchor.verify_decisions()
    assert not check.ok and check.torn_tail and "incomplete" in check.problem


@pytest.mark.asyncio
async def test_agent_edits_to_the_log_are_discarded(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=_checklist())
    # The agent scribbles into the working tree; the checkpoint rebuilds from HEAD.
    (tmp_path / ".lha" / "decisions.ndjson").write_text(_legacy("injected") + "\n", "utf-8")
    anchor.record_decision(DecisionRecord(decision="real", rationale="r"))
    await anchor.commit_checkpoint(
        Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
    )
    assert [d.decision for d in await anchor.read_decisions()] == ["real"]


# --- the record_decision tool ------------------------------------------------------------------
@pytest.mark.asyncio
async def test_record_decision_tool_validates_and_queues(tmp_path: Path) -> None:
    session = await LocalSandbox().open(workdir=str(tmp_path))
    ctx = ToolContext(mission_id="m", session=session)
    inner = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=False)
    buffer = DecisionBuffer()
    dispatcher = with_decision_tool(inner, buffer)
    assert isinstance(dispatcher, DecisionToolDispatcher)
    assert dispatcher.inner is inner
    assert with_decision_tool(dispatcher, DecisionBuffer()) is dispatcher  # idempotent
    assert RECORD_DECISION in {s.name for s in dispatcher.specs()}

    ok = await dispatcher.dispatch(
        ToolCall(
            id="1",
            name=RECORD_DECISION,
            arguments={
                "decision": "Store dates as UTC ISO-8601",
                "rationale": "one format everywhere",
                "alternatives_rejected": "epoch ints",
                "affected": ["models.py", " "],
            },
        ),
        ctx,
    )
    assert ok.ok and "committed with this cycle's checkpoint" in ok.content
    assert buffer.records == [
        DecisionRecord(
            decision="Store dates as UTC ISO-8601",
            rationale="one format everywhere",
            alternatives_rejected="epoch ints",
            affected=["models.py"],
        )
    ]
    empty = await dispatcher.dispatch(
        ToolCall(id="2", name=RECORD_DECISION, arguments={"decision": "x", "rationale": " "}),
        ctx,
    )
    assert not empty.ok and "non-empty" in (empty.error or "")
    typed = await dispatcher.dispatch(
        ToolCall(id="3", name=RECORD_DECISION, arguments={"decision": "x", "rationale": 3}),
        ctx,
    )
    assert not typed.ok and "invalid args" in (typed.error or "")
    long = await dispatcher.dispatch(
        ToolCall(id="4", name=RECORD_DECISION, arguments={"decision": "x" * 600, "rationale": "r"}),
        ctx,
    )
    assert not long.ok and "too long" in (long.error or "")
    # Everything else is delegated (here: the read-only inner refuses a write).
    other = await dispatcher.dispatch(
        ToolCall(id="5", name="write_file", arguments={"path": "a", "content": "b"}), ctx
    )
    assert not other.ok and "mutating tools are disabled" in (other.error or "")
    assert len(buffer.records) == 1


class _Recording(StubModel):
    def __init__(self, script: list[TurnResult]) -> None:
        super().__init__(script=script)
        self.calls: list[list[ModelMessage]] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls.append([m.model_copy() for m in messages])
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


_RECORD = TurnResult(
    text=json.dumps(
        {
            "tool": "record_decision",
            "arguments": {"decision": "Use argparse", "rationale": "stdlib only"},
        }
    )
)
_DONE = TurnResult(text='{"done": true, "summary": "ok"}')


@pytest.mark.asyncio
async def test_agent_loop_commits_recorded_decisions_and_shows_them_next_cycle(
    tmp_path: Path,
) -> None:
    items = Checklist(
        items=[ChecklistItem(id="01", description="one"), ChecklistItem(id="02", description="two")]
    )
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=items)
    session = await LocalSandbox().open(workdir=str(tmp_path))
    model = _Recording([_RECORD, _DONE, _DONE])
    # The same assembly every run path (run-local, orchestrate, the Temporal activity) uses.
    loop = build_lead_loop(
        Settings(sandbox="local", allow_unsafe_local=True, model_backend="stub"),
        model=model,
        anchor=anchor,
        workdir=str(tmp_path),
    )
    ctx = ToolContext(mission_id="m", session=session)
    first = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    assert first.verified
    assert "record_decision" in model.calls[0][0].content  # offered to the Lead

    committed = await anchor.read_decisions()
    assert [(d.decision, d.cycle_id) for d in committed] == [("Use argparse", "c1")]
    shown = git_ops.run_git(tmp_path, "show", "--stat", "HEAD")
    assert ".lha/decisions.ndjson" in shown  # in the cycle's own checkpoint commit

    await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c2", checks=[PASS])
    second_prompt = model.calls[-1][1].content
    assert "Design decisions already recorded" in second_prompt
    assert "[c1] Use argparse — stdlib only" in second_prompt


def test_render_decisions() -> None:
    assert render_decisions([]) == ""
    text = render_decisions(
        [
            DecisionRecord(
                decision="d",
                rationale="r",
                alternatives_rejected="x",
                affected=["a.py", "b.py"],
            )
        ]
    )
    assert "- d — r (rejected: x) (affects: a.py, b.py)" in text


class _Tamperer(StubModel):
    """On its first call, commits an altered decision log behind the harness's back."""

    def __init__(self, workdir: Path) -> None:
        super().__init__(script=[_DONE])
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
            bad = {"prev": GENESIS_HASH, "hash": "f" * 64, "record": {"decision": "x"}}
            bad["record"]["rationale"] = "forged"
            _commit_decisions(self._workdir, json.dumps(bad) + "\n", "forge")
        return await super().complete(messages, tools=tools, max_tokens=max_tokens)


@pytest.mark.asyncio
async def test_local_runner_stops_on_a_tampered_chain(tmp_path: Path) -> None:
    settings = Settings(
        sandbox="local", allow_unsafe_local=True, model_backend="stub", max_cycles=5
    )
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="T",
        description="D",
        checklist=_checklist(),
        checks=[PASS],
        settings=settings,
        model=_Tamperer(tmp_path),
    )
    assert summary.stopped_reason.startswith(DECISION_CHAIN_STOP)
    assert "hash mismatch" in summary.stopped_reason
    assert not summary.completed
    assert "decision_chain_invalid" in summary.trace_jsonl


# --- lha decisions -----------------------------------------------------------------------------
def test_cli_decisions_prints_and_verifies(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    asyncio.run(anchor.initialize(title="T", description="D", items=_checklist()))
    empty = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path)])
    assert empty.exit_code == 0 and "(no decisions recorded)" in empty.output

    anchor.record_decision(
        DecisionRecord(
            decision="first", rationale="why 1", alternatives_rejected="alt", affected=["a.py"]
        )
    )
    anchor.record_decision(DecisionRecord(decision="second", rationale="why 2"))
    asyncio.run(
        anchor.commit_checkpoint(
            Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=_checklist())
        )
    )
    listing = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path)])
    assert listing.exit_code == 0, listing.output
    assert "1. [c1] first" in listing.output
    assert "why: why 1" in listing.output
    assert "rejected: alt" in listing.output and "affects: a.py" in listing.output
    newest = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path), "--limit", "1"])
    assert "2. [c1] second" in newest.output and "first" not in newest.output

    ok = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path), "--verify"])
    assert ok.exit_code == 0 and "decision chain OK: 2 record(s), 2 chained" in ok.output

    lines = (tmp_path / ".lha" / "decisions.ndjson").read_text(encoding="utf-8").splitlines()
    _commit_decisions(tmp_path, lines[1] + "\n")  # delete the first record
    bad = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path), "--verify"])
    assert bad.exit_code == 1 and "decision chain BROKEN: line 1: prev-hash mismatch" in bad.output
    listing = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path)])
    assert listing.exit_code == 1 and "failed verification" in listing.output


def test_cli_decisions_reports_legacy_records(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    asyncio.run(anchor.initialize(title="T", description="D", items=_checklist()))
    _commit_decisions(tmp_path, _legacy("old") + "\n")
    out = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path), "--verify"])
    assert out.exit_code == 0
    assert "1 legacy (pre-chain) record(s), NOT protected until one is chained" in out.output


def test_cli_decisions_without_an_anchor(tmp_path: Path) -> None:
    out = runner.invoke(cli.app, ["decisions", "--workdir", str(tmp_path)])
    assert out.exit_code == 2 and "no mission anchor" in out.output
