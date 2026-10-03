"""Running LHA through Claude Code (``claude -p``): the model backend, the MCP bridge and the
``claude_code`` lead engine.

A fake ``claude`` executable stands in for the CLI. In ``mcp`` mode it is a real MCP client: it
reads the ``--mcp-config`` LHA passed, talks JSON-RPC over HTTP to LHA's bridge, calls LHA's
``write_file`` and ``verify`` tools and prints a result shaped like ``claude -p --output-format
json``. So the whole path (argv, bridge, dispatcher, verifier, checkpoint, ledger) is real; only
the model is scripted.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import httpx
import pytest

from lha.agent.assembly import lead_engine
from lha.agent.claude_code_engine import NATIVE_DENY, ClaudeCodeEngine
from lha.agent.mcp_bridge import McpBridge, bridge_tool
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import checks_from_commands
from lha.model import build_provider
from lha.model.claude_code import (
    ClaudeCodeError,
    ClaudeCodeModel,
    SessionProgress,
    call_budget_usd,
    parse_result,
)
from lha.model.pricing import ModelPrice
from lha.model.retry import is_retryable

FAKE_CLAUDE = r"""
import json, os, sys, urllib.request

args = sys.argv[1:]
if args == ["--version"]:
    print("9.9.9 (Claude Code)")
    sys.exit(0)
if args == ["auth", "status"]:
    print(json.dumps({"loggedIn": os.environ.get("FAKE_CLAUDE_LOGGED_IN", "1") == "1"}))
    sys.exit(0)
prompt = sys.stdin.read()
log = os.environ.get("FAKE_CLAUDE_LOG")
if log:
    with open(log, "a") as fh:
        fh.write(json.dumps({"argv": args, "stdin": prompt, "cwd": os.getcwd()}) + "\n")
mode = os.environ.get("FAKE_CLAUDE_MODE", "text")


STREAM = "stream-json" in args


# One streamed assistant turn: its usage, and a tool call when ``tool`` is set.
def turn(n, tool=None, model="claude-sonnet-4-6"):
    if not STREAM:
        return
    content = [{"type": "text", "text": "thinking"}]
    if tool:
        content.append({"type": "tool_use", "id": f"tu-{n}", "name": tool, "input": {}})
    message = {"id": f"msg-{n}", "model": model, "role": "assistant", "content": content,
               "usage": {"input_tokens": 100, "output_tokens": 1000,
                         "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
    print(json.dumps({"type": "assistant", "message": message, "session_id": "sess-1"}),
          flush=True)


def result(text, cost=0.01, **extra):
    out = {
        "type": "result", "subtype": "success", "is_error": False, "result": text,
        "num_turns": 3, "session_id": "sess-1", "stop_reason": "end_turn",
        "total_cost_usd": cost,
        "usage": {"input_tokens": 120, "output_tokens": 30, "cache_read_input_tokens": 1000,
                  "cache_creation_input_tokens": 200,
                  "cache_creation": {"ephemeral_1h_input_tokens": 50}},
        "modelUsage": {"claude-haiku-4-5": {"outputTokens": 2},
                       "claude-sonnet-4-6": {"outputTokens": 28}},
    }
    out.update(extra)
    print(json.dumps(out))


if STREAM:
    print(json.dumps({"type": "system", "subtype": "init", "session_id": "sess-1"}), flush=True)
if mode == "text":
    turn(1)
    result(os.environ.get("FAKE_CLAUDE_REPLY", '{"done": true, "summary": "ok"}'))
elif mode == "auth":
    result("Failed to authenticate: OAuth session expired", cost=0, is_error=True)
elif mode == "budget":
    result("", cost=0.9, subtype="error_max_budget_usd", is_error=True)
elif mode == "hang":  # streams two turns, then never finishes
    import time
    turn(1, tool="mcp__lha__read_file")
    turn(2, tool="mcp__lha__write_file", model=os.environ.get("FAKE_CLAUDE_MODEL", "claude-sonnet-4-6"))
    time.sleep(600)
elif mode == "mcp":
    config = json.loads(args[args.index("--mcp-config") + 1])["mcpServers"]["lha"]
    seq = [0]

    def rpc(method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method, "params": params or {}}
        if not notify:
            seq[0] += 1
            msg["id"] = seq[0]
        req = urllib.request.Request(
            config["url"], data=json.dumps(msg).encode(), method="POST",
            headers={**config["headers"], "Content-Type": "application/json",
                     "Accept": "application/json, text/event-stream"})
        with urllib.request.urlopen(req) as resp:
            body = resp.read()
            return json.loads(body) if body else None

    rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                       "clientInfo": {"name": "fake", "version": "0"}})
    rpc("notifications/initialized", notify=True)
    names = [t["name"] for t in rpc("tools/list")["result"]["tools"]]
    calls = json.loads(os.environ.get("FAKE_CLAUDE_CALLS", "[]"))
    texts = []
    for n, (name, arguments) in enumerate(calls, start=1):
        turn(n, tool=f"mcp__lha__{name}")
        reply = rpc("tools/call", {"name": name, "arguments": arguments})["result"]
        texts.append(reply["content"][0]["text"])
    result(json.dumps({"tools": names, "texts": texts}), cost=0.42)
"""


@pytest.fixture
def fake_claude(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    script = tmp_path / "fake-claude"
    script.write_text(f"#!{sys.executable}\n{FAKE_CLAUDE}")
    script.chmod(0o755)
    monkeypatch.setenv("FAKE_CLAUDE_LOG", str(tmp_path / "claude.log"))
    return script


def _calls(tmp_path: Path) -> list[dict[str, object]]:
    lines = (tmp_path / "claude.log").read_text().splitlines()
    return [json.loads(line) for line in lines]


def _checklist(*witnesses: str) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id="01", description="Create hello.txt", witnesses=list(witnesses))]
    )


# --- parsing the CLI result ----------------------------------------------------------------
def test_parse_result_reads_usage_cost_and_the_serving_model() -> None:
    out = json.dumps(
        {
            "type": "result",
            "subtype": "success",
            "is_error": False,
            "result": "hi",
            "num_turns": 2,
            "session_id": "s",
            "total_cost_usd": 0.0123,
            "usage": {"input_tokens": 5, "output_tokens": 7, "cache_read_input_tokens": 9},
            "modelUsage": {"small": {"outputTokens": 1}, "big": {"outputTokens": 6}},
        }
    )
    parsed = parse_result(out, provider="p", fallback_model="default")
    assert parsed.text == "hi" and parsed.session_id == "s" and parsed.num_turns == 2
    assert parsed.usage.model == "big"  # most of the output, not the helper model
    assert (parsed.usage.input_tokens, parsed.usage.output_tokens) == (5, 7)
    assert parsed.usage.cache_read_input_tokens == 9
    assert parsed.usage.reported_cost_usd == pytest.approx(0.0123)


def test_parse_result_classifies_errors() -> None:
    auth = json.dumps({"is_error": True, "subtype": "success", "result": "Failed to authenticate"})
    with pytest.raises(ClaudeCodeError) as err:
        parse_result(auth, provider="p", fallback_model="m")
    assert not is_retryable(err.value)  # a login problem is not fixed by retrying

    limited = json.dumps({"is_error": True, "result": "API error", "api_error_status": 429})
    with pytest.raises(ClaudeCodeError) as err:
        parse_result(limited, provider="p", fallback_model="m")
    assert is_retryable(err.value)

    capped = json.dumps({"subtype": "error_max_budget_usd", "total_cost_usd": 0.5})
    with pytest.raises(ClaudeCodeError) as err:
        parse_result(capped, provider="p", fallback_model="m")
    assert err.value.subtype == "error_max_budget_usd"
    assert err.value.usage is not None and err.value.usage.reported_cost_usd == 0.5

    with pytest.raises(ClaudeCodeError, match="no JSON"):
        parse_result("Error: unknown option", provider="p", fallback_model="m")


# --- the claude_code model backend -----------------------------------------------------------
@pytest.mark.asyncio
async def test_model_runs_a_tool_less_turn_through_the_cli(
    fake_claude: Path, tmp_path: Path
) -> None:
    model = ClaudeCodeModel(model_name="sonnet", binary=str(fake_claude), max_budget_usd=2.0)
    result = await model.complete(
        [
            ModelMessage(role="system", content="SYSTEM PROMPT"),
            ModelMessage(role="user", content="do the thing"),
            ModelMessage(
                role="assistant",
                content="",
                tool_calls=[ToolCall(id="t1", name="read_file", arguments={"path": "a"})],
            ),
            ModelMessage(role="tool", content="file a", tool_call_id="t1"),
        ]
    )
    assert result.text == '{"done": true, "summary": "ok"}'
    assert result.session_id == "sess-1"
    assert result.usage.reported_cost_usd == pytest.approx(0.01)
    assert model.estimate_cost_usd(result.usage) == pytest.approx(0.01)

    (call,) = _calls(tmp_path)
    argv = call["argv"]
    assert isinstance(argv, list)
    assert argv[:4] == ["-p", "--output-format", "stream-json", "--verbose"]
    assert argv[argv.index("--tools") + 1] == ""  # every built-in tool off
    assert argv[argv.index("--model") + 1] == "sonnet"
    assert argv[argv.index("--max-budget-usd") + 1] == "2.0000"
    assert argv[argv.index("--system-prompt") + 1] == "SYSTEM PROMPT"
    stdin = str(call["stdin"])
    assert "do the thing" in stdin and "file a" in stdin
    assert '{"tool": "read_file", "arguments": {"path": "a"}}' in stdin  # the prior action


@pytest.mark.asyncio
async def test_model_single_turn_prompt_is_sent_verbatim(fake_claude: Path, tmp_path: Path) -> None:
    model = ClaudeCodeModel(binary=str(fake_claude))
    await model.complete([ModelMessage(role="user", content="plan this")])
    (call,) = _calls(tmp_path)
    assert call["stdin"] == "plan this"
    assert "--model" not in call["argv"]  # the default model is Claude Code's own choice


@pytest.mark.asyncio
async def test_model_does_not_retry_an_expired_login(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "auth")
    model = ClaudeCodeModel(binary=str(fake_claude), retry_base_delay_s=0)
    with pytest.raises(ClaudeCodeError, match="authenticate"):
        await model.complete([ModelMessage(role="user", content="x")])
    assert len(_calls(tmp_path)) == 1


@pytest.mark.asyncio
async def test_model_reports_a_missing_binary() -> None:
    model = ClaudeCodeModel(binary="/nonexistent/claude", max_retries=0)
    with pytest.raises(ClaudeCodeError, match="LHA_CLAUDE_CODE_BIN"):
        await model.complete([ModelMessage(role="user", content="x")])
    health = await model.health_check(timeout_s=1)
    assert not health.ok and "not found" in health.detail


def test_worst_case_is_the_budget_cap_unless_the_model_is_priced() -> None:
    unpriced = ClaudeCodeModel(model_name="opus", max_budget_usd=3.0)
    big = Usage(input_tokens=10_000_000, output_tokens=10_000_000)
    assert unpriced.estimate_cost_usd(big) == 3.0
    priced = ClaudeCodeModel(model_name="x", max_budget_usd=3.0, price=ModelPrice(1.0, 1.0))
    assert priced.estimate_cost_usd(Usage(input_tokens=1_000_000)) == pytest.approx(1.0)
    assert priced.estimate_cost_usd(big) == 3.0  # never above the cap the CLI enforces


@pytest.mark.parametrize(
    ("configured", "remaining", "cap"),
    [(5.0, 10.0, 5.0), (5.0, 4.25, 4.25), (5.0, 0.01, 0.01), (5.0, 0.009, 5.0), (5.0, -1.0, 5.0)],
)
def test_a_call_is_capped_at_what_is_left_of_the_budget(
    configured: float, remaining: float, cap: float
) -> None:
    assert call_budget_usd(configured, remaining) == cap
    model = ClaudeCodeModel(model_name="opus", max_budget_usd=configured)
    capped = model.budget_capped(remaining)
    assert capped.estimate_cost_usd(Usage(input_tokens=10_000_000)) == cap  # unpriced: the cap
    assert model.estimate_cost_usd(Usage(input_tokens=10_000_000)) == configured  # unchanged


@pytest.mark.asyncio
async def test_a_metered_call_reserves_and_passes_the_same_lowered_cap(
    fake_claude: Path, tmp_path: Path
) -> None:
    from lha.governor import BudgetGovernor, CostLedger, CostMeter

    meter = CostMeter(ledger=CostLedger(), governor=BudgetGovernor(ceiling_usd=3.0, max_cycles=9))
    model = meter.wrap(ClaudeCodeModel(model_name="opus", binary=str(fake_claude)))  # $5 cap
    assert model.remaining_usd == 3.0
    await model.complete([ModelMessage(role="user", content="x")])  # allowed: capped at $3
    (call,) = _calls(tmp_path)
    argv = call["argv"]
    assert isinstance(argv, list)
    assert argv[argv.index("--max-budget-usd") + 1] == "3.0000"
    assert meter.remaining_usd == pytest.approx(3.0 - 0.01)  # the reported cost was charged


def test_build_provider_and_settings() -> None:
    model = build_provider(Settings(_env_file=None, model_backend="claude_code"))  # type: ignore[call-arg]
    assert isinstance(model, ClaudeCodeModel) and model.name == "claude_code:default"
    named = build_provider(
        Settings(_env_file=None, model_backend="claude_code", model_name="opus")  # type: ignore[call-arg]
    )
    assert named.name == "claude_code:opus"

    # The engine alone routes the other roles through claude -p too; an explicit backend wins.
    alone = Settings(_env_file=None, lead_engine="claude_code")  # type: ignore[call-arg]
    assert alone.model_backend == "claude_code"
    explicit = Settings(_env_file=None, lead_engine="claude_code", model_backend="stub")  # type: ignore[call-arg]
    assert explicit.model_backend == "stub"


def test_lead_engine_from_settings() -> None:
    assert lead_engine(Settings(_env_file=None)) is None  # type: ignore[call-arg]
    engine = lead_engine(Settings(_env_file=None, lead_engine="claude_code", model_name="haiku"))  # type: ignore[call-arg]
    assert isinstance(engine, ClaudeCodeEngine) and not engine.native
    assert engine.name == "claude_code_engine:haiku"

    native = {"lead_engine": "claude_code", "claude_code_tools": "native"}
    with pytest.raises(ValueError, match="no isolation"):
        lead_engine(Settings(_env_file=None, **native))  # type: ignore[call-arg]
    unsafe = Settings(_env_file=None, sandbox="local", allow_unsafe_local=True, **native)  # type: ignore[call-arg]
    engine = lead_engine(unsafe)
    assert engine is not None and engine.native
    with pytest.raises(ValueError, match="ownership guard"):
        lead_engine(unsafe, guarded=True)


# --- the MCP bridge ------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_bridge_speaks_json_rpc_over_http_and_checks_the_token() -> None:
    seen: list[dict[str, object]] = []

    async def echo(arguments: dict[str, object]) -> tuple[str, bool]:
        seen.append(arguments)
        return f"echo {arguments.get('x')}", False

    async def boom(_: dict[str, object]) -> tuple[str, bool]:
        raise RuntimeError("kaput")

    tools = [
        bridge_tool("echo", "Echo x", {"type": "object", "properties": {"x": {}}}, echo),
        bridge_tool("boom", "Fails", {}, boom),
    ]
    async with McpBridge(tools) as bridge, httpx.AsyncClient() as client:
        server = bridge.mcp_config()["mcpServers"]
        assert isinstance(server, dict)
        headers = server["lha"]["headers"]
        assert bridge.allowed_tools() == ["mcp__lha__echo", "mcp__lha__boom"]

        async def rpc(body: object, auth: dict[str, str] = headers) -> httpx.Response:
            return await client.post(bridge.url, json=body, headers=auth)

        init = (await rpc({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})).json()
        assert init["result"]["serverInfo"]["name"] == "lha"
        assert "tools" in init["result"]["capabilities"]
        note = await rpc({"jsonrpc": "2.0", "method": "notifications/initialized"})
        assert note.status_code == 202

        listed = (await rpc({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})).json()
        schemas = {t["name"]: t["inputSchema"] for t in listed["result"]["tools"]}
        assert schemas["boom"] == {"type": "object", "properties": {}}  # always an object

        call = {"name": "echo", "arguments": {"x": 7}}
        reply = (
            await rpc({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": call})
        ).json()
        assert reply["result"] == {
            "content": [{"type": "text", "text": "echo 7"}],
            "isError": False,
        }
        failed = (
            await rpc(
                {"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "boom"}}
            )
        ).json()
        assert failed["result"]["isError"] and "kaput" in failed["result"]["content"][0]["text"]
        unknown = (await rpc({"jsonrpc": "2.0", "id": 5, "method": "resources/list"})).json()
        assert unknown["error"]["code"] == -32601

        assert (await rpc({"jsonrpc": "2.0", "id": 6, "method": "ping"}, {})).status_code == 401
        wrong = {"Authorization": "Bearer nope"}
        assert (await rpc({"jsonrpc": "2.0", "id": 7, "method": "ping"}, wrong)).status_code == 401
        assert (await client.get(bridge.url, headers=headers)).status_code == 405
    assert seen == [{"x": 7}] and bridge.calls == 2


# --- the claude_code lead engine, end to end -------------------------------------------------
def _engine_settings(fake: Path, **overrides: object) -> Settings:
    values: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "lead_engine": "claude_code",
        "claude_code_bin": str(fake),
        "claude_code_max_budget_usd": 1.0,
        "budget_usd_ceiling": 5.0,
        "max_cycles": 3,
    }
    values.update(overrides)
    return Settings(_env_file=None, **values)  # type: ignore[call-arg]


@pytest.mark.asyncio
async def test_a_cycle_is_one_claude_session_using_lha_tools(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "mcp")
    calls = [["write_file", {"path": "hello.txt", "content": "hi\n"}], ["verify", {}]]
    monkeypatch.setenv("FAKE_CLAUDE_CALLS", json.dumps(calls))
    ws = tmp_path / "ws"
    ws.mkdir()

    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist("cmd:grep -qx hi hello.txt"),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_claude),
    )

    assert summary.completed, summary.stopped_reason
    assert summary.cycles == 1
    assert summary.total_usd == pytest.approx(0.42)  # the cost Claude Code reported
    (session,) = _calls(tmp_path)
    argv = session["argv"]
    assert isinstance(argv, list)
    assert session["cwd"] == str(ws.resolve()) or session["cwd"] == str(ws)
    assert argv[argv.index("--tools") + 1] == ""  # Claude Code's own tools are off
    assert "--strict-mcp-config" in argv
    allowed = argv[argv.index("--allowedTools") + 1 :]
    assert "mcp__lha__write_file" in allowed and "mcp__lha__verify" in allowed
    system = argv[argv.index("--append-system-prompt") + 1]
    assert "call the `verify` tool" in system and '{"tool"' not in system
    assert "Create hello.txt" in str(session["stdin"])
    assert "cmd:grep -qx hi hello.txt" in str(session["stdin"])  # the witness is in the task

    assert (ws / "hello.txt").read_text() == "hi\n"  # written through LHA's write_file
    assert "claude_code_session" in summary.trace_jsonl
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    (verified,) = [e["data"] for e in events if e["kind"] == "verify"]  # reused at the end
    assert verified["trigger"] == "tool" and verified["verdict"] == "passed"


@pytest.mark.asyncio
async def test_verify_reports_failures_and_the_harness_still_decides(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "mcp")
    # The session writes the wrong content and stops after a failing verify.
    calls = [["write_file", {"path": "hello.txt", "content": "bye\n"}], ["verify", {}]]
    monkeypatch.setenv("FAKE_CLAUDE_CALLS", json.dumps(calls))
    ws = tmp_path / "ws"
    ws.mkdir()

    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist("cmd:grep -qx hi hello.txt"),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_claude, max_replans=0),
    )

    assert not summary.completed
    assert summary.cycles == 3  # three failed attempts, then the item is blocked
    assert summary.stopped_reason.startswith("deadlocked")
    assert not (ws / "hello.txt").exists()  # every failed attempt was rolled back
    checklist = json.loads((ws / ".lha" / "checklist.json").read_text())
    assert checklist["items"][0]["status"] == "blocked"
    # The second session was told why the first failed.
    second = str(_calls(tmp_path)[1]["stdin"])
    assert "FAILED verification" in second and "grep -qx hi hello.txt" in second


@pytest.mark.asyncio
async def test_the_session_cap_is_lowered_to_what_is_left_of_the_budget(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "mcp")
    calls = [["write_file", {"path": "hello.txt", "content": "hi\n"}]]
    monkeypatch.setenv("FAKE_CLAUDE_CALLS", json.dumps(calls))
    ws = tmp_path / "ws"
    ws.mkdir()
    settings = _engine_settings(fake_claude, budget_usd_ceiling=0.5)  # below the $1 session cap
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=settings,
    )
    assert summary.completed, summary.stopped_reason  # not refused for its $1 cap
    argv = _calls(tmp_path)[0]["argv"]
    assert isinstance(argv, list)
    assert argv[argv.index("--max-budget-usd") + 1] == "0.5000"


@pytest.mark.asyncio
async def test_the_session_is_refused_when_almost_nothing_is_left(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "mcp")
    ws = tmp_path / "ws"
    ws.mkdir()
    settings = _engine_settings(fake_claude, budget_usd_ceiling=0.005)  # under a cent left
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=settings,
    )
    assert summary.stopped_reason.startswith("governor")
    assert not (tmp_path / "claude.log").exists()  # claude -p never ran


@pytest.mark.asyncio
async def test_a_capped_session_is_still_verified_and_charged(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "budget")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_claude),
    )
    assert summary.completed  # the work in the workdir passed, though the session hit its cap
    assert summary.total_usd == pytest.approx(0.9)
    assert "error_max_budget_usd" in summary.trace_jsonl  # the session event says why it stopped


# Each fake turn is 100 input and 1000 output tokens; at claude-sonnet-4-6 prices ($3/$15 per MTok)
# that is $0.0153.
_TURN_USD = 100 / 1e6 * 3.0 + 1000 / 1e6 * 15.0


@pytest.mark.asyncio
async def test_a_session_killed_at_its_timeout_is_charged_what_it_spent(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "hang")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_claude, claude_code_timeout_s=3),
    )
    assert summary.completed  # the work in the workdir is still verified
    assert summary.total_usd == pytest.approx(2 * _TURN_USD)  # its two turns, not the $1 cap
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    progress = [e["data"] for e in events if e["kind"] == "session_progress"]
    assert progress == [
        {"turns": 1, "tool_calls": 1, "tool": "mcp__lha__read_file", "spent_usd": 0.0153},
        {"turns": 2, "tool_calls": 2, "tool": "mcp__lha__write_file", "spent_usd": 0.0306},
    ]
    (session,) = [e["data"] for e in events if e["kind"] == "claude_code_session"]
    assert "did not finish within 3s" in str(session["stopped"])


@pytest.mark.asyncio
async def test_a_killed_session_on_an_unpriced_model_is_charged_its_cap(
    fake_claude: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_CLAUDE_MODE", "hang")
    monkeypatch.setenv("FAKE_CLAUDE_MODEL", "claude-unreleased-9")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_claude, claude_code_timeout_s=3),
    )
    assert summary.total_usd == pytest.approx(1.0)  # the session's cap: its spend has no price
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    progress = [e["data"] for e in events if e["kind"] == "session_progress"]
    assert [p["spent_usd"] for p in progress] == [0.0153, None]


def test_session_progress_counts_each_turn_and_tool_once() -> None:
    progress = SessionProgress()
    assert progress.spent_usd is None  # nothing streamed: unknown, so the cap is charged, not $0
    usage = {
        "input_tokens": 100,
        "output_tokens": 1000,
        "cache_creation_input_tokens": 2000,
        "cache_creation": {"ephemeral_1h_input_tokens": 2000},
    }
    text = {"type": "text", "text": "x"}
    tool = {"type": "tool_use", "id": "tu-1", "name": "Read"}

    def event(msg_id: str, block: dict[str, object]) -> dict[str, object]:
        message = {
            "id": msg_id,
            "model": "claude-sonnet-4-6-20260101",
            "usage": usage,
            "content": [block],
        }
        return {"type": "assistant", "message": message, "session_id": "s-9"}

    # Claude Code streams each content block of a message as its own event, usage repeated.
    assert progress.observe(event("m1", text)) is True
    assert progress.observe(event("m1", tool)) is True  # a tool call is news
    assert progress.observe(event("m1", tool)) is False  # the same block again is not
    assert progress.observe({"type": "system", "subtype": "init"}) is False
    assert (progress.turns, progress.tool_calls, progress.tool) == (1, 1, "Read")
    assert progress.session_id == "s-9"
    # 100 input + 2000 one-hour cache writes (x2) + 1000 output, at $3/$15 per MTok.
    assert progress.spent_usd == pytest.approx((100 + 4000) / 1e6 * 3 + 1000 / 1e6 * 15)
    total = progress.usage(provider="p", fallback_model="sonnet")
    assert (total.input_tokens, total.output_tokens, total.cache_creation_1h_input_tokens) == (
        100,
        1000,
        2000,
    )
    assert total.reported_cost_usd == progress.spent_usd and total.model.startswith("claude-")


def test_native_mode_denies_history_publishing_and_the_web() -> None:
    engine = ClaudeCodeEngine(tools="native")
    bridge = McpBridge([])
    args = engine._args(bridge, "sys")
    assert "--tools" not in args  # Claude Code's own tools stay on
    denied = args[args.index("--disallowedTools") + 1 :]
    assert denied == list(NATIVE_DENY)
    assert {"Bash(git commit:*)", "Bash(git push:*)", "WebFetch"} <= set(denied)


@pytest.mark.asyncio
async def test_health_requires_a_logged_in_cli(
    fake_claude: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("PATH", f"{fake_claude.parent}:{os.environ['PATH']}")
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    model = ClaudeCodeModel(binary=str(fake_claude))
    healthy = await model.health_check(timeout_s=10)
    assert healthy.ok and "9.9.9" in healthy.detail

    # Logged out: DOWN, so a parked mission does not resume just to fail on authentication.
    monkeypatch.setenv("FAKE_CLAUDE_LOGGED_IN", "0")
    down = await model.health_check(timeout_s=10)
    assert not down.ok and "claude auth login" in down.detail

    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")  # the key authenticates instead
    assert (await model.health_check(timeout_s=10)).ok
