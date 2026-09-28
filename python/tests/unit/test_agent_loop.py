"""Integration test for the agent loop (model + tools + sandbox + verifier + anchor).

Uses a scripted StubModel emitting JSON actions, so it's deterministic and offline — but it
exercises the REAL tool dispatch, real file writes in the sandbox, the real deterministic
verifier, and a real git checkpoint.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from lha.agent.loop import AgentLoop, parse_action
from lha.contracts.model import ModelMessage, ToolCall, TurnResult
from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.verifier import DeterministicVerifier

PY = sys.executable
FILE_HAS_HELLO = Check(
    name="out_has_hello",
    command=[PY, "-c", "import sys; sys.exit(open('out.txt').read() != 'hello')"],
)


class RecordingModel(StubModel):
    """A scripted stub that also records every message list it was sent."""

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


async def _setup(tmp_path: Path, items: Checklist) -> tuple[GitMissionAnchor, SandboxSession]:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=items)
    session = await LocalSandbox().open(workdir=str(tmp_path))
    return anchor, session


def _loop(
    model: StubModel,
    anchor: GitMissionAnchor,
    *,
    max_turns: int = 8,
    max_consecutive_failures: int = 3,
) -> AgentLoop:
    return AgentLoop(
        model=model,
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        verifier=DeterministicVerifier(default_timeout_s=60),
        anchor=anchor,
        max_turns=max_turns,
        max_consecutive_failures=max_consecutive_failures,
    )


def _write(path: str, content: str) -> TurnResult:
    return TurnResult(
        text=f'{{"tool": "write_file", "arguments": {{"path": "{path}", "content": "{content}"}}}}'
    )


DONE = TurnResult(text='{"done": true, "summary": "ok"}')


def test_parse_action_variants() -> None:
    tool = parse_action('{"tool": "read_file", "arguments": {"path": "x"}}', [])
    assert tool.tool == "read_file"
    assert tool.arguments == {"path": "x"}
    assert not tool.done and tool.is_valid

    done = parse_action('{"done": true, "summary": "ok"}', [])
    assert done.done
    assert done.summary == "ok"

    native = parse_action("ignored", [ToolCall(id="1", name="grep", arguments={"pattern": "x"})])
    assert native.tool == "grep"
    assert native.arguments == {"pattern": "x"}


def test_parse_action_unparseable_is_never_done() -> None:
    prose = parse_action("I am finished.", [])
    assert not prose.done and not prose.is_valid and prose.tool is None

    no_action = parse_action('{"summary": "hi"}', [])
    assert not no_action.done and not no_action.is_valid

    truncated = parse_action('{"done": true}', [], stop_reason="max_tokens")
    assert not truncated.done and "truncated" in truncated.error


@pytest.mark.asyncio
async def test_agent_loop_uses_tool_then_completes(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="write a file")])
    )
    loop = _loop(StubModel(script=[_write("out.txt", "hello"), DONE]), anchor)
    outcome = await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        anchor_text="Mission: write a file.",
        checks=[FILE_HAS_HELLO],
    )

    assert outcome.advanced
    assert outcome.verified and outcome.verdict == "passed"
    assert outcome.is_complete  # the only item is now done
    assert outcome.tool_calls == 1
    assert (tmp_path / "out.txt").read_text(encoding="utf-8") == "hello"
    assert any("lha: complete 01" in line for line in git_ops.log_oneline(tmp_path))
    item = (await anchor.read_checklist()).items[0]
    assert item.status == "done" and item.verified_by == ["out_has_hello"]


@pytest.mark.asyncio
async def test_zero_checks_is_unverified_never_done(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    loop = _loop(StubModel(script=[DONE]), anchor)
    outcome = await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session), mission_id="m1", cycle_id="c1"
    )
    assert not outcome.verified
    assert outcome.verdict == "unverified"
    assert not outcome.is_complete
    item = (await anchor.read_checklist()).items[0]
    assert item.status == "in_progress" and "UNVERIFIED" in item.last_failure


@pytest.mark.asyncio
async def test_advisory_only_checks_are_unverified(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    loop = _loop(StubModel(script=[DONE]), anchor)
    outcome = await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[Check(name="adv", command=[PY, "-c", "pass"], gating=False)],
    )
    assert outcome.verdict == "unverified" and not outcome.is_complete


@pytest.mark.asyncio
async def test_agent_loop_blocks_on_failed_verification(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    loop = _loop(StubModel(script=[DONE]), anchor, max_turns=1)
    outcome = await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        anchor_text="m",
        checks=[Check(name="fail", command=[PY, "-c", "print('boom'); raise SystemExit(1)"])],
    )

    assert outcome.advanced
    assert not outcome.verified  # the gating check failed
    assert not outcome.is_complete  # item is not done
    assert any("lha: attempt 01" in line for line in git_ops.log_oneline(tmp_path))
    item = (await anchor.read_checklist()).items[0]
    assert "boom" in item.last_failure  # the check's output tail reaches the next attempt


@pytest.mark.asyncio
async def test_repeated_failure_blocks_item_and_independent_items_proceed(tmp_path: Path) -> None:
    items = Checklist(
        items=[
            ChecklistItem(id="01", description="impossible"),
            ChecklistItem(id="02", description="depends on 01", depends_on=["01"]),
            ChecklistItem(id="03", description="independent"),
        ]
    )
    anchor, session = await _setup(tmp_path, items)
    loop = _loop(StubModel(script=[DONE]), anchor, max_turns=1, max_consecutive_failures=2)
    ctx = ToolContext(mission_id="m1", session=session)
    fail = [Check(name="fail", command=[PY, "-c", "raise SystemExit(1)"])]

    first = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c1", checks=fail)
    second = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c2", checks=fail)
    assert first.item_id == second.item_id == "01"
    assert second.item_blocked and not second.is_deadlocked

    third = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c3", checks=fail)
    assert third.item_id == "03"  # the blocked item is not re-picked

    await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c4", checks=fail)
    idle = await loop.run_cycle(ctx=ctx, mission_id="m1", cycle_id="c5", checks=fail)
    assert idle.item_id is None and not idle.advanced
    assert idle.is_deadlocked and not idle.is_complete  # never reported as complete
    assert "blocked" in idle.reason


@pytest.mark.asyncio
async def test_unparseable_reply_gets_corrective_turn(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    model = RecordingModel(
        script=[
            TurnResult(text="Sure, I'll do that now."),
            TurnResult(text='{"done": true', stop_reason="max_tokens"),
            _write("out.txt", "hello"),
            DONE,
        ]
    )
    loop = _loop(model, anchor)
    outcome = await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[FILE_HAS_HELLO],
    )
    assert outcome.verified and outcome.turns == 4
    corrective = [m.content for m in model.calls[1] if m.role == "user"][-1]
    assert "not a valid action" in corrective
    assert "truncated" in [m.content for m in model.calls[2] if m.role == "user"][-1]


@pytest.mark.asyncio
async def test_failed_done_is_fed_back_and_fixed_within_cycle(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    model = RecordingModel(
        script=[_write("out.txt", "nope"), DONE, _write("out.txt", "hello"), DONE]
    )
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[FILE_HAS_HELLO],
    )
    assert outcome.verified and outcome.turns == 4
    assert "VERIFICATION FAILED" in model.calls[2][-1].content


@pytest.mark.asyncio
async def test_native_tool_calls_all_executed_and_paired(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    model = RecordingModel(
        script=[
            TurnResult(
                text="writing two files",
                tool_calls=[
                    ToolCall(
                        id="t1", name="write_file", arguments={"path": "a.txt", "content": "1"}
                    ),
                    ToolCall(
                        id="t2",
                        name="write_file",
                        arguments={"path": "out.txt", "content": "hello"},
                    ),
                ],
            ),
            DONE,
        ]
    )
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[FILE_HAS_HELLO],
    )
    assert outcome.tool_calls == 2 and outcome.verified
    assert (tmp_path / "a.txt").exists()
    # Real native pairing: the assistant turn carries both tool_use calls and each result is a
    # role="tool" message answering its call by id (what providers map to tool_result blocks).
    assistant, first, second = model.calls[1][-3:]
    assert assistant.role == "assistant" and [c.id for c in assistant.tool_calls] == ["t1", "t2"]
    assert assistant.content == "writing two files"
    assert (first.role, first.tool_call_id) == ("tool", "t1")
    assert (second.role, second.tool_call_id) == ("tool", "t2")


class _TamperingModel(StubModel):
    """Simulates an agent that edits harness files directly (e.g. via run_command)."""

    def __init__(self, workdir: Path, tamper: dict[str, str | None]) -> None:
        super().__init__(script=[DONE])
        self._workdir = workdir
        self._tamper = tamper

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        for rel, content in self._tamper.items():
            path = self._workdir / rel
            if content is None:
                path.unlink()
            else:
                path.write_text(content, encoding="utf-8")
        return await super().complete(messages)


@pytest.mark.asyncio
async def test_agent_cannot_write_its_own_verdict(tmp_path: Path) -> None:
    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    forged = Checklist(items=[ChecklistItem(id="01", description="x", status="done")])
    model = _TamperingModel(
        tmp_path,
        {".lha/checklist.json": forged.model_dump_json(), ".lha/mission.json": '{"title": "evil"}'},
    )
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[Check(name="fail", command=[PY, "-c", "raise SystemExit(1)"])],
    )
    assert not outcome.verified and not outcome.is_complete
    committed = await GitMissionAnchor(tmp_path).read_checklist()
    assert committed.items[0].status != "done"
    on_disk = Checklist.model_validate_json((tmp_path / ".lha/checklist.json").read_text())
    assert on_disk.items[0].status != "done"  # the forged working-tree file was overwritten
    mission = await anchor.read_mission()
    assert mission is not None and mission.title == "T"


async def _setup_with_tests(
    tmp_path: Path, item: ChecklistItem
) -> tuple[GitMissionAnchor, SandboxSession]:
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "test_core.py").write_text("assert 1 == 2\n", encoding="utf-8")
    return await _setup(tmp_path, Checklist(items=[item]))


@pytest.mark.asyncio
async def test_harness_tampering_fails_and_is_reverted(tmp_path: Path) -> None:
    anchor, session = await _setup_with_tests(tmp_path, ChecklistItem(id="01", description="x"))
    model = _TamperingModel(
        tmp_path, {"tests/test_core.py": "assert True\n", "tests/test_new.py": "x = 1\n"}
    )
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[Check(name="ok", command=[PY, "-c", "pass"])],
    )
    assert not outcome.verified and outcome.verdict == "failed"
    item = (await anchor.read_checklist()).items[0]
    assert "harness_integrity" in item.last_failure and "tests/test_core.py" in item.last_failure
    assert (tmp_path / "tests/test_core.py").read_text(encoding="utf-8") == "assert 1 == 2\n"
    # New tests are not a harness violation, but the failed attempt is rolled back as a whole:
    # its new file is kept on the attempt ref, not in the checkout.
    assert not (tmp_path / "tests/test_new.py").exists()
    saved = git_ops.run_git(tmp_path, "show", "refs/lha/attempts/m1/c1:tests/test_new.py")
    assert saved == "x = 1"


@pytest.mark.asyncio
async def test_harness_deletion_detected(tmp_path: Path) -> None:
    anchor, session = await _setup_with_tests(tmp_path, ChecklistItem(id="01", description="x"))
    model = _TamperingModel(tmp_path, {"tests/test_core.py": None})
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[Check(name="ok", command=[PY, "-c", "pass"])],
    )
    assert not outcome.verified
    assert (tmp_path / "tests/test_core.py").exists()  # restored from HEAD


@pytest.mark.asyncio
async def test_harness_edits_allowed_when_item_opts_in(tmp_path: Path) -> None:
    anchor, session = await _setup_with_tests(
        tmp_path, ChecklistItem(id="01", description="fix test", allow_harness_edits=True)
    )
    model = _TamperingModel(tmp_path, {"tests/test_core.py": "assert True\n"})
    outcome = await _loop(model, anchor).run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[Check(name="ok", command=[PY, "-c", "pass"])],
    )
    assert outcome.verified


@pytest.mark.asyncio
async def test_mission_anchor_survives_many_cycles(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(
        title="Build the Frobnicator",
        description="It must frobnicate widgets.",
        items=Checklist(items=[ChecklistItem(id="01", description="a")]),
    )
    session = await LocalSandbox().open(workdir=str(tmp_path))
    model = RecordingModel(script=[DONE])
    loop = _loop(model, anchor, max_turns=1, max_consecutive_failures=10)
    fail = [Check(name="fail", command=[PY, "-c", "raise SystemExit(1)"])]
    for n in range(3):
        await loop.run_cycle(
            ctx=ToolContext(mission_id="m1", session=session),
            mission_id="m1",
            cycle_id=f"c{n}",
            checks=fail,
        )
    system = model.calls[-1][0].content
    assert "Build the Frobnicator" in system and "It must frobnicate widgets." in system
    progress = (tmp_path / ".lha/progress.md").read_text(encoding="utf-8")
    assert "Build the Frobnicator" in progress
    assert "c0 [01]" in progress and "c2 [01]" in progress  # appended, not overwritten


@pytest.mark.asyncio
async def test_running_out_of_turns_is_recorded(tmp_path: Path) -> None:
    from lha.obs.events import TraceRecorder

    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="write a file")])
    )
    read = TurnResult(text='{"tool": "list_files", "arguments": {}}')
    recorder = TraceRecorder()

    async def cycle(script: list[TurnResult], cycle_id: str) -> list[str]:
        loop = AgentLoop(
            model=StubModel(script=script),
            dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
            verifier=DeterministicVerifier(default_timeout_s=60),
            anchor=anchor,
            max_turns=2,
            recorder=recorder,
        )
        await loop.run_cycle(
            ctx=ToolContext(mission_id="m1", session=session),
            mission_id="m1",
            cycle_id=cycle_id,
            checks=[FILE_HAS_HELLO],
        )
        return [e.kind for e in recorder.events if e.cycle_id == cycle_id]

    assert "turns_exhausted" in await cycle([read, read], "c1")
    exhausted = [e for e in recorder.events if e.kind == "turns_exhausted"]
    assert exhausted[0].data == {"max_turns": 2, "tool_calls": 2}
    assert "turns_exhausted" not in await cycle([_write("out.txt", "hello"), DONE], "c2")


@pytest.mark.asyncio
async def test_a_failed_tool_call_records_why(tmp_path: Path) -> None:
    import json

    from lha.agent.loop import TOOL_ERROR_TAIL, tool_error_detail
    from lha.contracts.tools import ToolResult
    from lha.obs.events import TraceRecorder

    anchor, session = await _setup(
        tmp_path, Checklist(items=[ChecklistItem(id="01", description="write a file")])
    )
    argv = [
        PY,
        "-c",
        "import sys; sys.stderr.write('boom token=sk-live-abcdefghijklmnop'); sys.exit(3)",
    ]
    fail = TurnResult(text=json.dumps({"tool": "run_command", "arguments": {"argv": argv}}))
    ok = TurnResult(text='{"tool": "list_files", "arguments": {}}')
    recorder = TraceRecorder()
    loop = AgentLoop(
        model=StubModel(script=[fail, ok]),
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        verifier=DeterministicVerifier(default_timeout_s=60),
        anchor=anchor,
        max_turns=2,
        recorder=recorder,
    )
    await loop.run_cycle(
        ctx=ToolContext(mission_id="m1", session=session),
        mission_id="m1",
        cycle_id="c1",
        checks=[FILE_HAS_HELLO],
    )
    failed, passed = [e.data for e in recorder.events if e.kind == "tool_call"]
    error = str(failed["error"])
    assert failed["ok"] is False and error.startswith("exit 3\nexit_code=3") and "boom" in error
    assert "sk-live-abcdefghijklmnop" not in error  # the recorder redacts it
    assert passed == {"tool": "list_files", "ok": True}
    long = tool_error_detail(ToolResult(ok=False, error="e", content="x" * 1000 + "END"))
    assert len(long) == TOOL_ERROR_TAIL and long.endswith("END")
