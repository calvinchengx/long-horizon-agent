"""Large-mission capabilities: witnesses, trusted checks, protected paths, replanning, approvals.

Everything runs through the REAL loop, dispatcher, verifier, sandbox (local) and git anchor, with a
scripted model, so the assertions are about what the harness actually does.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import httpx
import pytest

from lha.agent.assembly import build_lead_loop, lead_dispatcher, lead_tools
from lha.agent.loop import AgentLoop
from lha.agents.replanner import Replanner
from lha.config import Settings
from lha.contracts.hitl import GateDecision, GateRequest
from lha.contracts.model import ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.hitl.approvals import DeferredApprovalGate, action_fingerprint
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.state.vendor import MANIFEST, VendorError, vendor_urls
from lha.verify.verifier import DeterministicVerifier

PY = sys.executable
PASS = Check(name="always", command=[PY, "-c", "pass"])
DONE = TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn")


def _settings(**overrides: object) -> Settings:
    return Settings(_env_file=None, **overrides)  # type: ignore[call-arg]


async def _setup(
    tmp_path: Path, items: list[ChecklistItem]
) -> tuple[GitMissionAnchor, ToolContext]:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=Checklist(items=items))
    session = await LocalSandbox().open(workdir=str(tmp_path))
    return anchor, ToolContext(mission_id="m", session=session)


def _loop(model: StubModel, anchor: GitMissionAnchor, **kwargs: object) -> AgentLoop:
    return AgentLoop(
        model=model,
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        verifier=DeterministicVerifier(default_timeout_s=60),
        anchor=anchor,
        max_turns=2,
        **kwargs,  # type: ignore[arg-type]
    )


# --- witnesses ------------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_item_witness_gates_on_top_of_mission_checks(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="needs its witness", witnesses=["cmd:exit 3"])]
    anchor, ctx = await _setup(tmp_path, items)
    outcome = await _loop(StubModel(script=[DONE, DONE]), anchor).run_cycle(
        ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS]
    )
    assert not outcome.verified and outcome.verdict == "failed"
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and "cmd:exit 3 FAILED" in item.last_failure


@pytest.mark.asyncio
async def test_passing_witness_marks_item_done_and_is_recorded(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="ok", witnesses=["cmd:true"])]
    anchor, ctx = await _setup(tmp_path, items)
    outcome = await _loop(StubModel(script=[DONE]), anchor).run_cycle(
        ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS]
    )
    assert outcome.verified
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and item.verified_by == ["always", "cmd:true"]


@pytest.mark.asyncio
async def test_witness_alone_can_gate_an_item(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="ok", witnesses=["cmd:true"])]
    anchor, ctx = await _setup(tmp_path, items)
    outcome = await _loop(StubModel(script=[DONE]), anchor).run_cycle(
        ctx=ctx, mission_id="m", cycle_id="c1", checks=[]
    )
    assert outcome.verified


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("witness", "expected"),
    [("bogus:x", "invalid witness"), ("trusted:e2e", "invalid witness")],
)
async def test_unusable_witness_fails_never_skips(
    tmp_path: Path, witness: str, expected: str
) -> None:
    items = [ChecklistItem(id="01", description="x", witnesses=[witness])]
    anchor, ctx = await _setup(tmp_path, items)
    outcome = await _loop(StubModel(script=[DONE, DONE]), anchor).run_cycle(
        ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS]
    )
    assert not outcome.verified
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and expected in item.last_failure


@pytest.mark.asyncio
async def test_trusted_witness_runs_outside_the_sandbox(tmp_path: Path) -> None:
    marker = tmp_path.parent / f"{tmp_path.name}-trusted-ran"
    trusted = {"e2e": ["sh", "-c", f'test -f hello.txt && touch "{marker}"']}
    items = [ChecklistItem(id="01", description="x", witnesses=["trusted:e2e"])]
    anchor, ctx = await _setup(tmp_path, items)
    write = TurnResult(
        tool_calls=[
            ToolCall(id="w", name="write_file", arguments={"path": "hello.txt", "content": "hi"})
        ],
        stop_reason="tool_use",
    )
    loop = build_lead_loop(
        _settings(trusted_checks=json.dumps(trusted)),
        model=StubModel(script=[write, DONE]),
        anchor=anchor,
        workdir=str(tmp_path),
    )
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    # The trusted runner saw the agent's UNCOMMITTED file in a clean worktree of the candidate.
    assert outcome.verified and marker.exists()


@pytest.mark.asyncio
async def test_trusted_check_is_refused_by_the_plain_sandbox_verifier(tmp_path: Path) -> None:
    session = await LocalSandbox().open(workdir=str(tmp_path))
    result = await DeterministicVerifier().verify(
        session, [Check(name="e2e", command=["true"], where="trusted")]
    )
    assert not result.all_green and "needs a trusted runner" in result.results[0].output_tail


# --- operator-protected paths ---------------------------------------------------------------


@pytest.mark.asyncio
async def test_protected_glob_blocks_editing_the_gate_definition(tmp_path: Path) -> None:
    (tmp_path / "Makefile").write_text("e2e:\n\t./run-e2e\n")
    items = [ChecklistItem(id="01", description="x")]
    anchor, ctx = await _setup(tmp_path, items)  # commits the Makefile with the anchor
    rewrite = TurnResult(
        tool_calls=[
            ToolCall(
                id="w",
                name="write_file",
                arguments={"path": "Makefile", "content": "e2e:\n\ttrue\n"},
            )
        ],
        stop_reason="tool_use",
    )
    loop = _loop(StubModel(script=[rewrite, DONE]), anchor, harness_globs=("Makefile",))
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    assert not outcome.verified
    assert (tmp_path / "Makefile").read_text() == "e2e:\n\t./run-e2e\n"  # reverted


# --- replanning -----------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_blocked_item_is_split_instead_of_deadlocking(tmp_path: Path) -> None:
    items = [
        ChecklistItem(id="01", description="coarse", witnesses=["cmd:exit 1"]),
        ChecklistItem(id="02", description="after", depends_on=["01"]),
    ]
    anchor, ctx = await _setup(tmp_path, items)
    plan = TurnResult(text='[{"description": "part A"}, {"description": "part B"}]')
    model = StubModel(script=[DONE, DONE, plan])  # the lead's model also does the replanning
    loop = _loop(
        model,
        anchor,
        max_consecutive_failures=1,
        replanner=Replanner(model),
        max_replans=5,
    )
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    assert outcome.item_split and not outcome.item_blocked and not outcome.is_deadlocked
    checklist = await anchor.read_checklist()
    assert [(i.id, i.status) for i in checklist.items] == [
        ("01", "split"),
        ("02", "todo"),
        ("01.1", "todo"),  # children join the back of the queue
        ("01.2", "todo"),
    ]
    assert checklist.get("01.2").witnesses == ["cmd:exit 1"]  # type: ignore[union-attr]
    assert checklist.get("02").depends_on == ["01.2"]  # type: ignore[union-attr]
    assert "lha: split 01" in git_ops.log_oneline(tmp_path, 1)[0]


@pytest.mark.asyncio
async def test_a_split_item_does_not_starve_independent_items(tmp_path: Path) -> None:
    # 01 keeps failing and splits; 02 is independent and was already waiting, so it gets the
    # next cycle instead of queueing behind 01's children.
    items = [
        ChecklistItem(id="01", description="hard", witnesses=["cmd:exit 1"]),
        ChecklistItem(id="02", description="easy, independent"),
    ]
    anchor, ctx = await _setup(tmp_path, items)
    plan = TurnResult(text='[{"description": "part A"}, {"description": "part B"}]')
    model = StubModel(script=[DONE, DONE, plan, DONE, DONE])
    loop = _loop(
        model, anchor, max_consecutive_failures=1, replanner=Replanner(model), max_replans=5
    )
    first = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    second = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c2", checks=[PASS])
    assert first.item_id == "01" and first.item_split
    assert second.item_id == "02" and second.verified  # not 01.1


@pytest.mark.asyncio
async def test_replanning_respects_the_budget_and_depth(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01.1.1", description="deep", witnesses=["cmd:exit 1"])]
    anchor, ctx = await _setup(tmp_path, items)
    loop = _loop(
        StubModel(script=[DONE, DONE]),
        anchor,
        max_consecutive_failures=1,
        replanner=Replanner(StubModel(script=[])),  # would fail if it were called
        max_replans=5,
        max_split_depth=2,
    )
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    assert outcome.item_blocked and outcome.is_deadlocked


@pytest.mark.asyncio
async def test_unusable_split_leaves_the_item_blocked(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="x", witnesses=["cmd:exit 1"])]
    anchor, ctx = await _setup(tmp_path, items)
    model = StubModel(script=[DONE, DONE, TurnResult(text="no idea")])
    loop = _loop(
        model, anchor, max_consecutive_failures=1, replanner=Replanner(model), max_replans=5
    )
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[PASS])
    assert outcome.item_blocked and not outcome.item_split


# --- approvals ------------------------------------------------------------------------------


def _gate_request(fingerprint: str) -> GateRequest:
    return GateRequest(
        gate_id="g",
        question="?",
        default_action=GateDecision.REJECT,
        context={"fingerprint": fingerprint, "tool": "run_command", "reason": "git push"},
    )


@pytest.mark.asyncio
async def test_deferred_gate_queues_then_allows_an_approved_action_once() -> None:
    gate = DeferredApprovalGate()
    first = await gate.request(_gate_request("f1"))
    assert first.decision is GateDecision.REJECT and gate.pending[0]["fingerprint"] == "f1"
    approved = DeferredApprovalGate(approved=["f1"])
    assert (await approved.request(_gate_request("f1"))).decision is GateDecision.APPROVE
    assert (await approved.request(_gate_request("f1"))).decision is GateDecision.REJECT
    assert approved.used == ["f1"]


def test_fingerprint_covers_the_exact_arguments() -> None:
    push = action_fingerprint("run_command", {"argv": ["git", "push", "origin", "main"]})
    assert push == action_fingerprint("run_command", {"argv": ["git", "push", "origin", "main"]})
    assert push != action_fingerprint("run_command", {"argv": ["git", "push", "--force"]})


@pytest.mark.asyncio
async def test_dispatcher_queues_a_gated_command_for_approval(tmp_path: Path) -> None:
    gate = DeferredApprovalGate()
    dispatcher = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=gate
    )
    ctx = ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))
    call = ToolCall(id="1", name="run_command", arguments={"argv": ["git", "push", "origin"]})
    result = await dispatcher.dispatch(call, ctx)
    assert not result.ok and "queued for human approval" in (result.error or "")
    assert gate.pending[0]["fingerprint"] == action_fingerprint(call.name, call.arguments)


@pytest.mark.asyncio
async def test_dispatcher_runs_the_approved_command(tmp_path: Path) -> None:
    call = ToolCall(id="1", name="run_command", arguments={"argv": ["git", "push", "nowhere"]})
    gate = DeferredApprovalGate(approved=[action_fingerprint(call.name, call.arguments)])
    dispatcher = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=gate
    )
    git_ops.init_repo(tmp_path)
    ctx = ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))
    result = await dispatcher.dispatch(call, ctx)
    # It really ran (and git failed, since there is no such remote) rather than being gated.
    assert "queued" not in (result.error or "") and gate.used


# --- assembly / references ------------------------------------------------------------------


def test_web_hosts_register_fetch_url_with_egress() -> None:
    names = {t.spec.name for t in lead_tools(_settings(web_allow_hosts="learn.microsoft.com"))}
    assert "fetch_url" in names
    assert "fetch_url" not in {t.spec.name for t in lead_tools(_settings())}
    specs = {s.name for s in lead_dispatcher(_settings(web_allow_hosts="a.example")).specs()}
    assert "fetch_url" in specs


@pytest.mark.asyncio
async def test_references_are_recited_in_the_mission_anchor(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(
        title="T",
        description="D",
        items=Checklist(items=[ChecklistItem(id="01", description="x")]),
        references=["reference/fabric-rest"],
    )
    mission = await anchor.read_mission()
    assert mission is not None and "reference/fabric-rest" in mission.render_anchor()


async def _public(host: str, port: int) -> list[str]:
    return ["93.184.216.34"]


@pytest.mark.asyncio
async def test_vendor_snapshots_pages_with_a_manifest(tmp_path: Path) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/moved":
            return httpx.Response(302, headers={"location": "/docs/items"})
        return httpx.Response(
            200, text="<html><p>Items API</p></html>", headers={"content-type": "text/html"}
        )

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    saved = await vendor_urls(
        ["https://docs.example.com/moved"], tmp_path / "ref", client=client, resolver=_public
    )
    assert saved[0].path == "docs.example.com/docs/items.html"
    assert (tmp_path / "ref" / saved[0].text_path).read_text().strip() == "Items API"
    manifest = (tmp_path / "ref" / MANIFEST).read_text()
    assert "docs.example.com/moved" in manifest and saved[0].sha256 in manifest


@pytest.mark.asyncio
async def test_vendor_refuses_redirects_off_the_named_hosts(tmp_path: Path) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(302, headers={"location": "https://evil.example/x"})

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    with pytest.raises(VendorError, match="egress allow-list"):
        await vendor_urls(
            ["https://docs.example.com/a"], tmp_path / "ref", client=client, resolver=_public
        )


@pytest.mark.asyncio
async def test_vendor_refuses_private_addresses(tmp_path: Path) -> None:
    async def private(host: str, port: int) -> list[str]:
        return ["10.0.0.5"]

    with pytest.raises(VendorError):
        await vendor_urls(["https://intranet.example/a"], tmp_path / "ref", resolver=private)


# --- failed attempts are rolled back ----------------------------------------------------------


def _write_turn(path: str, content: str) -> TurnResult:
    return TurnResult(
        tool_calls=[
            ToolCall(id="w", name="write_file", arguments={"path": path, "content": content})
        ],
        stop_reason="tool_use",
    )


@pytest.mark.asyncio
async def test_failed_attempt_is_rolled_back_and_kept_on_a_ref(tmp_path: Path) -> None:
    (tmp_path / "lib.py").write_text("GOOD = 1\n")
    (tmp_path / ".gitignore").write_text("cache/\n")
    items = [ChecklistItem(id="01", description="x", witnesses=["cmd:exit 1"])]
    anchor, ctx = await _setup(tmp_path, items)
    (tmp_path / "cache").mkdir()
    (tmp_path / "cache" / "keep.bin").write_text("ignored build output")
    script = [_write_turn("lib.py", "BROKEN = (\n"), _write_turn("new.py", "x = 1\n")]
    loop = _loop(StubModel(script=script), anchor)  # max_turns=2: both edits, then verify
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c1", checks=[PASS])

    assert not outcome.verified
    # The checkout is back at the last verified state...
    assert (tmp_path / "lib.py").read_text() == "GOOD = 1\n"
    assert not (tmp_path / "new.py").exists()
    assert (tmp_path / "cache" / "keep.bin").exists()  # ignored files are left alone
    # ...the failed work is kept on a ref, not on the branch...
    ref = "refs/lha/attempts/m1/c1"
    assert git_ops.run_git(tmp_path, "show", f"{ref}:lib.py") == "BROKEN = ("
    assert "lib.py" not in git_ops.run_git(tmp_path, "show", "--name-only", "--format=", "HEAD")
    # ...and the next attempt is told what happened.
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and f"rolled back to the last verified state (kept at {ref})" in (
        item.last_failure
    )
    assert "lib.py, new.py" in item.last_failure


@pytest.mark.asyncio
async def test_passing_attempt_is_committed_not_rolled_back(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="x", witnesses=["cmd:test -f new.py"])]
    anchor, ctx = await _setup(tmp_path, items)
    loop = _loop(StubModel(script=[_write_turn("new.py", "x = 1\n"), DONE]), anchor)
    outcome = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c1", checks=[PASS])
    assert outcome.verified and (tmp_path / "new.py").exists()
    assert "new.py" in git_ops.run_git(tmp_path, "show", "--name-only", "--format=", "HEAD")
    assert git_ops.run_git(tmp_path, "for-each-ref", "refs/lha/attempts") == ""


@pytest.mark.asyncio
async def test_failed_attempt_without_code_changes_creates_no_ref(tmp_path: Path) -> None:
    items = [ChecklistItem(id="01", description="x", witnesses=["cmd:exit 1"])]
    anchor, ctx = await _setup(tmp_path, items)
    outcome = await _loop(StubModel(script=[DONE, DONE]), anchor).run_cycle(
        ctx=ctx, mission_id="m1", cycle_id="c1", checks=[PASS]
    )
    assert not outcome.verified
    assert git_ops.run_git(tmp_path, "for-each-ref", "refs/lha/attempts") == ""
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and "rolled back" not in item.last_failure
