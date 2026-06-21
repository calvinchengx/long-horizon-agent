"""Tests for the tool dispatcher safety gates + real fs tools (through the dispatcher).

Proves the gates are enforced in code (not prompt): default-deny egress, allow-list, mutating
policy, arg validation — and that a real write→read round-trips through the sandbox.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from lha.contracts.hitl import GateDecision, GateRequest, RiskTier
from lha.contracts.model import ToolCall
from lha.contracts.tools import ToolContext
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.execution.tools.web import FetchUrlTool, WebSearchTool
from lha.hitl.gate import AutoPolicyGate, CallbackGate
from lha.safety.egress import EgressPolicy
from lha.safety.rule_of_two import Capability, RuleOfTwoViolation


async def _ctx(tmp_path: Path) -> ToolContext:
    session = await LocalSandbox().open(workdir=str(tmp_path))
    return ToolContext(mission_id="m1", session=session)


@pytest.mark.asyncio
async def test_write_then_read_round_trips(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    ctx = await _ctx(tmp_path)

    wrote = await dispatcher.dispatch(
        ToolCall(id="1", name="write_file", arguments={"path": "a/b.txt", "content": "hello"}), ctx
    )
    assert wrote.ok

    read = await dispatcher.dispatch(
        ToolCall(id="2", name="read_file", arguments={"path": "a/b.txt"}), ctx
    )
    assert read.ok
    assert read.content == "hello"


@pytest.mark.asyncio
async def test_egress_is_default_denied(tmp_path: Path) -> None:
    # Web search is egress=True; with default policy it must be blocked BEFORE any network call.
    dispatcher = AllowListDispatcher.for_tools(
        [*default_local_tools(), WebSearchTool(api_key="unused")], allow_mutating=True
    )
    ctx = await _ctx(tmp_path)

    result = await dispatcher.dispatch(
        ToolCall(id="3", name="web_search", arguments={"query": "anything"}), ctx
    )
    assert not result.ok
    assert result.error is not None
    assert "egress" in result.error


@pytest.mark.asyncio
async def test_egress_allowed_when_enabled(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(
        [*default_local_tools(), WebSearchTool(api_key="unused")],
        allow_mutating=True,
        allow_egress=True,
    )
    # Permitted now, so it reaches the tool; the spec is advertised.
    assert any(spec.name == "web_search" for spec in dispatcher.specs())


@pytest.mark.asyncio
async def test_unknown_tool_and_missing_args(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    ctx = await _ctx(tmp_path)

    unknown = await dispatcher.dispatch(ToolCall(id="4", name="nope", arguments={}), ctx)
    assert not unknown.ok
    assert unknown.error is not None and "unknown tool" in unknown.error

    missing = await dispatcher.dispatch(ToolCall(id="5", name="read_file", arguments={}), ctx)
    assert not missing.ok
    assert missing.error is not None and "missing required" in missing.error


@pytest.mark.asyncio
async def test_allow_list_and_mutating_policy(tmp_path: Path) -> None:
    # Only read_file allowed; write_file (mutating) excluded from the allow-list.
    dispatcher = AllowListDispatcher(
        default_local_tools(), allow={"read_file"}, allow_mutating=True
    )
    ctx = await _ctx(tmp_path)

    blocked = await dispatcher.dispatch(
        ToolCall(id="6", name="write_file", arguments={"path": "x", "content": "y"}), ctx
    )
    assert not blocked.ok
    assert blocked.error is not None and "not allowed" in blocked.error

    # And with mutating disabled, even an allow-listed mutating tool is refused.
    no_mutate = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=False)
    blocked2 = await no_mutate.dispatch(
        ToolCall(id="7", name="write_file", arguments={"path": "x", "content": "y"}), ctx
    )
    assert not blocked2.ok
    assert blocked2.error is not None and "mutating" in blocked2.error


@pytest.mark.asyncio
async def test_argument_types_are_validated(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    ctx = await _ctx(tmp_path)
    bad_calls = [
        ToolCall(id="1", name="read_file", arguments={"path": 123}),
        ToolCall(id="2", name="write_file", arguments={"path": "a", "content": ["x"]}),
        ToolCall(id="3", name="run_command", arguments={"argv": "rm -rf /"}),
        ToolCall(id="4", name="run_command", arguments={"argv": ["ls", 3]}),
        ToolCall(id="5", name="run_command", arguments={"argv": ["ls"], "timeout_s": True}),
        ToolCall(id="6", name="run_command", arguments={"argv": ["ls"], "timeout_s": 0}),
        ToolCall(id="7", name="grep", arguments={"pattern": "x", "regex": "yes"}),
    ]
    for call in bad_calls:
        result = await dispatcher.dispatch(call, ctx)
        assert not result.ok, call
        assert result.error is not None and "invalid args" in result.error


@pytest.mark.asyncio
async def test_writes_to_harness_dirs_are_denied(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    ctx = await _ctx(tmp_path)
    for path in (".lha/checklist.json", ".git/hooks/pre-commit", "./.LHA/x", "a/../.git/config"):
        result = await dispatcher.dispatch(
            ToolCall(id="w", name="write_file", arguments={"path": path, "content": "x"}), ctx
        )
        assert not result.ok, path
        assert result.error is not None and "harness-owned" in result.error
    assert not (tmp_path / ".lha").exists()
    # Reading harness state is fine.
    (tmp_path / ".lha").mkdir()
    (tmp_path / ".lha" / "n.txt").write_text("hi", encoding="utf-8")
    read = await dispatcher.dispatch(
        ToolCall(id="r", name="read_file", arguments={"path": ".lha/n.txt"}), ctx
    )
    assert read.ok


@pytest.mark.skipif(sys.platform == "win32", reason="symlinks need privileges on Windows")
@pytest.mark.asyncio
async def test_writes_through_symlink_to_harness_dir_are_denied(tmp_path: Path) -> None:
    (tmp_path / ".git").mkdir()
    (tmp_path / "innocent").symlink_to(tmp_path / ".git", target_is_directory=True)
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    result = await dispatcher.dispatch(
        ToolCall(id="w", name="write_file", arguments={"path": "innocent/config", "content": "x"}),
        await _ctx(tmp_path),
    )
    assert not result.ok
    assert not (tmp_path / ".git" / "config").exists()


@pytest.mark.asyncio
async def test_irreversible_commands_denied_without_gate(tmp_path: Path) -> None:
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    result = await dispatcher.dispatch(
        ToolCall(id="p", name="run_command", arguments={"argv": ["git", "push", "origin"]}),
        await _ctx(tmp_path),
    )
    assert not result.ok
    assert result.error is not None and "no human gate" in result.error


@pytest.mark.asyncio
async def test_irreversible_commands_route_through_gate(tmp_path: Path) -> None:
    asked: list[GateRequest] = []

    async def approve(req: GateRequest) -> GateDecision:
        asked.append(req)
        return GateDecision.APPROVE

    ctx = await _ctx(tmp_path)
    call = ToolCall(
        id="p", name="run_command", arguments={"argv": [sys.executable, "-m", "twine", "upload"]}
    )
    approved = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=CallbackGate(approve)
    )
    result = await approved.dispatch(call, ctx)
    assert len(asked) == 1 and asked[0].risk is RiskTier.IRREVERSIBLE
    assert "twine" in asked[0].context["arguments"]
    assert result.error is None or "denied" not in result.error  # it ran (twine may be absent)

    # The unattended policy gate never approves an irreversible action.
    auto = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=AutoPolicyGate()
    )
    denied = await auto.dispatch(call, ctx)
    assert not denied.ok and denied.error is not None and "denied" in denied.error

    # Benign commands never hit the gate.
    asked.clear()
    ok = await approved.dispatch(
        ToolCall(id="b", name="run_command", arguments={"argv": [sys.executable, "-c", "1"]}), ctx
    )
    assert ok.ok and asked == []


def test_rule_of_two_refuses_trifecta_without_gate() -> None:
    tools = [*default_local_tools(), FetchUrlTool(egress_policy=EgressPolicy())]
    # untrusted web content + egress, no private data: allowed.
    dispatcher = AllowListDispatcher.for_tools(tools, allow_mutating=True, allow_egress=True)
    assert dispatcher.capabilities == {Capability.UNTRUSTED_CONTENT, Capability.EXTERNAL_COMMS}
    # ... plus declared private data = the lethal trifecta: refused unless gated.
    with pytest.raises(RuleOfTwoViolation):
        AllowListDispatcher.for_tools(
            tools, allow_mutating=True, allow_egress=True, capabilities={Capability.PRIVATE_DATA}
        )
    gated = AllowListDispatcher.for_tools(
        tools,
        allow_mutating=True,
        allow_egress=True,
        capabilities={Capability.PRIVATE_DATA},
        gate=AutoPolicyGate(),
    )
    assert len(gated.capabilities) == 3
    # Egress disabled => no external comms => fine without a gate.
    AllowListDispatcher.for_tools(
        tools, allow_mutating=True, capabilities={Capability.PRIVATE_DATA}
    )


@pytest.mark.asyncio
async def test_trifecta_with_gate_gates_every_egress_call(tmp_path: Path) -> None:
    tools = [*default_local_tools(), FetchUrlTool(egress_policy=EgressPolicy())]
    dispatcher = AllowListDispatcher.for_tools(
        tools,
        allow_mutating=True,
        allow_egress=True,
        capabilities={Capability.PRIVATE_DATA},
        gate=AutoPolicyGate(),
    )
    result = await dispatcher.dispatch(
        ToolCall(id="f", name="fetch_url", arguments={"url": "https://example.com"}),
        await _ctx(tmp_path),
    )
    assert not result.ok and result.error is not None and "denied" in result.error


@pytest.mark.asyncio
async def test_grep_is_literal_by_default_and_guards_regex(tmp_path: Path) -> None:
    (tmp_path / "f.txt").write_text("a.b\naxb\n", encoding="utf-8")
    dispatcher = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    ctx = await _ctx(tmp_path)
    literal = await dispatcher.dispatch(
        ToolCall(id="g", name="grep", arguments={"pattern": "a.b"}), ctx
    )
    assert literal.ok and "a.b" in literal.content and "axb" not in literal.content
    regex = await dispatcher.dispatch(
        ToolCall(id="g", name="grep", arguments={"pattern": "a.b", "regex": True}), ctx
    )
    assert regex.ok and "axb" in regex.content
    for evil in ["(a+)+$", "(a|aa)*b", "(x*)*y", r"(a)\1", "a" * 300]:
        result = await dispatcher.dispatch(
            ToolCall(id="g", name="grep", arguments={"pattern": evil, "regex": True}), ctx
        )
        assert not result.ok, evil


@pytest.mark.asyncio
async def test_dispatcher_is_fail_closed_by_default(tmp_path: Path) -> None:
    ctx = await _ctx(tmp_path)
    (tmp_path / "f.txt").write_text("x", encoding="utf-8")
    closed = AllowListDispatcher(default_local_tools())
    assert closed.specs() == []
    denied = await closed.dispatch(
        ToolCall(id="z1", name="read_file", arguments={"path": "f.txt"}), ctx
    )
    assert not denied.ok and denied.error is not None and "not allowed" in denied.error

    # Naming tools is not enough to mutate: allow_mutating defaults to False.
    named = AllowListDispatcher(default_local_tools(), allow={"read_file", "write_file"})
    assert (
        await named.dispatch(ToolCall(id="z2", name="read_file", arguments={"path": "f.txt"}), ctx)
    ).ok
    write = await named.dispatch(
        ToolCall(id="z3", name="write_file", arguments={"path": "g.txt", "content": "y"}), ctx
    )
    assert not write.ok and write.error is not None and "mutating" in write.error
    assert not (tmp_path / "g.txt").exists()
    with pytest.raises(TypeError):
        AllowListDispatcher(default_local_tools(), allow="read_file")
