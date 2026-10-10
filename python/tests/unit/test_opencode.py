"""Running LHA through OpenCode (``opencode run``): the model backend, the MCP bridge and the
``opencode`` lead engine.

A fake ``opencode`` executable stands in for the CLI. In ``mcp`` mode it is a real MCP client: it
reads the ``OPENCODE_CONFIG`` LHA injected, talks JSON-RPC over HTTP to LHA's bridge, calls LHA's
``write_file`` and ``verify`` tools and prints newline-delimited JSON events shaped like
``opencode run --format json``. So the whole path (argv, injected config, bridge, dispatcher,
verifier, checkpoint, ledger) is real; only the model is scripted.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

from lha.agent.assembly import lead_engine
from lha.agent.mcp_bridge import McpBridge
from lha.agent.opencode_engine import NATIVE_DENY, OpenCodeEngine
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import checks_from_commands
from lha.model import build_provider
from lha.model.opencode import (
    DEFAULT_AGENT,
    OpenCodeError,
    OpenCodeModel,
    OpenCodeTimeout,
    SessionProgress,
    base_args,
    run_opencode,
)
from lha.model.pricing import ModelPrice
from lha.model.retry import is_retryable

FAKE_OPENCODE = r"""
import json, os, sys, time, urllib.request

args = sys.argv[1:]
if args == ["--version"]:
    print("0.0.0-fake (OpenCode)")
    sys.exit(0)
prompt = sys.stdin.read()
log = os.environ.get("FAKE_OPENCODE_LOG")
cfg_path = os.environ.get("OPENCODE_CONFIG", "")
cfg = json.load(open(cfg_path)) if cfg_path and os.path.exists(cfg_path) else {}
if log:
    with open(log, "a") as fh:
        fh.write(json.dumps({"argv": args, "stdin": prompt, "cwd": os.getcwd(), "config": cfg,
                             "project_disabled": os.environ.get("OPENCODE_DISABLE_PROJECT_CONFIG")}) + "\n")
mode = os.environ.get("FAKE_OPENCODE_MODE", "text")
SID = "sess-oc-1"
seq = [0]


def emit(obj):
    print(json.dumps(obj), flush=True)


def start(mid):
    emit({"type": "step_start", "sessionID": SID, "part": {"messageID": mid, "type": "step-start"}})


def finish(mid, cost=0.0, tokens=None, reason="tool-calls"):
    part = {"messageID": mid, "type": "step-finish", "reason": reason}
    if cost is not None:
        part["cost"] = cost
    if tokens is not None:
        part["tokens"] = tokens
    emit({"type": "step_finish", "sessionID": SID, "part": part})


def text(msg):
    emit({"type": "text", "sessionID": SID, "part": {"text": msg}})


def rpc(url, headers, method, params=None, notify=False):
    msg = {"jsonrpc": "2.0", "method": method, "params": params or {}}
    if not notify:
        seq[0] += 1
        msg["id"] = seq[0]
    req = urllib.request.Request(
        url, data=json.dumps(msg).encode(), method="POST",
        headers={**headers, "Content-Type": "application/json",
                 "Accept": "application/json"})
    with urllib.request.urlopen(req) as resp:
        body = resp.read()
        return json.loads(body) if body else None


TOKENS = {"input": 100, "output": 1000, "cache": {"read": 0, "write": 0}}

if mode == "text":
    start("msg-1")
    finish("msg-1", cost=0.01, tokens={"input": 100, "output": 50, "cache": {"read": 0, "write": 0}}, reason="stop")
    text(os.environ.get("FAKE_OPENCODE_REPLY", '{"done": true, "summary": "ok"}'))
elif mode == "error":
    emit({"type": "error", "sessionID": SID, "error": {"message": "model overloaded 529"}})
elif mode == "hang":
    start("msg-1")
    finish("msg-1", cost=0.0153, tokens=TOKENS)
    time.sleep(600)
elif mode == "hang-unpriced":
    start("msg-1")
    finish("msg-1", cost=None, tokens=TOKENS)
    time.sleep(600)
elif mode == "budget":
    start("msg-1")
    finish("msg-1", cost=0.9, tokens=TOKENS)
    time.sleep(600)
elif mode == "mcp":
    server = cfg["mcp"]["servers"]["lha"]
    url, headers = server["url"], server["headers"]
    rpc(url, headers, "initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                     "clientInfo": {"name": "fake", "version": "0"}})
    rpc(url, headers, "notifications/initialized", notify=True)
    names = [t["name"] for t in rpc(url, headers, "tools/list")["result"]["tools"]]
    calls = json.loads(os.environ.get("FAKE_OPENCODE_CALLS", "[]"))
    texts = []
    for n, (name, arguments) in enumerate(calls, start=1):
        mid = f"msg-{n}"
        start(mid)
        emit({"type": "tool_use", "sessionID": SID,
              "part": {"id": f"tool-{n}", "messageID": mid, "tool": f"lha_{name}",
                       "state": {"status": "completed", "input": arguments}}})
        reply = rpc(url, headers, "tools/call", {"name": name, "arguments": arguments})["result"]
        texts.append(reply["content"][0]["text"])
        finish(mid, cost=0.21, tokens=TOKENS)
    final = f"msg-{len(calls) + 1}"
    start(final)
    text(json.dumps({"tools": names, "texts": texts}))
    finish(final, cost=0.21, tokens={"input": 10, "output": 10, "cache": {"read": 0, "write": 0}},
           reason="stop")
"""


@pytest.fixture
def fake_opencode(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    script = tmp_path / "fake-opencode"
    script.write_text(f"#!{sys.executable}\n{FAKE_OPENCODE}")
    script.chmod(0o755)
    monkeypatch.setenv("FAKE_OPENCODE_LOG", str(tmp_path / "opencode.log"))
    return script


def _calls(tmp_path: Path) -> list[dict[str, object]]:
    lines = (tmp_path / "opencode.log").read_text().splitlines()
    return [json.loads(line) for line in lines]


def _checklist(*witnesses: str) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id="01", description="Create hello.txt", witnesses=list(witnesses))]
    )


# --- the JSON event stream -----------------------------------------------------------------
def test_session_progress_counts_each_turn_and_tool_once() -> None:
    progress = SessionProgress()
    assert progress.spent_usd is None  # nothing streamed: unknown, so the cap is charged, not $0

    def step_start(mid: str) -> dict[str, object]:
        return {"type": "step_start", "sessionID": "s-9", "part": {"messageID": mid}}

    def step_finish(mid: str, cost: float | None) -> dict[str, object]:
        part: dict[str, object] = {
            "messageID": mid,
            "cost": cost,
            "tokens": {"input": 100, "output": 1000, "cache": {"read": 50, "write": 20}},
        }
        return {"type": "step_finish", "sessionID": "s-9", "part": part}

    def tool(call_id: str, name: str) -> dict[str, object]:
        return {"type": "tool_use", "sessionID": "s-9", "part": {"id": call_id, "tool": name}}

    assert progress.observe(step_start("m1")) is True
    assert progress.observe(step_start("m1")) is False  # the same step again is not news
    assert progress.observe(tool("t1", "lha_run_command")) is True
    assert progress.observe(tool("t1", "lha_run_command")) is False
    assert progress.observe({"type": "text", "part": {"text": "x"}}) is False
    assert progress.observe(step_finish("m1", 0.25)) is False  # already counted as a turn
    assert (progress.turns, progress.tool_calls, progress.tool) == (1, 1, "lha_run_command")
    assert progress.session_id == "s-9" and progress.spent_usd == pytest.approx(0.25)
    total = progress.usage(provider="p", fallback_model="default")
    assert (total.input_tokens, total.output_tokens) == (100, 1000)
    assert (total.cache_read_input_tokens, total.cache_creation_input_tokens) == (50, 20)
    assert total.reported_cost_usd == pytest.approx(0.25)

    # A step with no cost makes the whole session's spend unknown (never $0).
    progress.observe(step_start("m2"))
    progress.observe(step_finish("m2", None))
    assert progress.spent_usd is None


# --- parsing the CLI output and errors ------------------------------------------------------
@pytest.mark.asyncio
async def test_run_opencode_reads_text_usage_and_cost(fake_opencode: Path, tmp_path: Path) -> None:
    result = await run_opencode(
        base_args(model="", agent="a", standalone=False),
        prompt="hi",
        binary=str(fake_opencode),
        cwd=str(tmp_path),
        timeout_s=10,
        provider="opencode:x",
        fallback_model="",
    )
    assert result.text == '{"done": true, "summary": "ok"}'
    assert result.session_id == "sess-oc-1" and result.num_turns == 1
    assert result.usage.reported_cost_usd == pytest.approx(0.01)


@pytest.mark.asyncio
async def test_run_opencode_classifies_errors(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "error")
    with pytest.raises(OpenCodeError) as err:
        await run_opencode(
            base_args(model="", agent="a", standalone=False),
            prompt="hi",
            binary=str(fake_opencode),
            cwd=str(tmp_path),
            timeout_s=10,
            provider="p",
            fallback_model="",
        )
    assert is_retryable(err.value)  # 529 overload is transient

    with pytest.raises(OpenCodeError, match="LHA_OPENCODE_BIN"):
        await run_opencode(
            base_args(model="", agent="a", standalone=False),
            prompt="hi",
            binary="/nonexistent/opencode",
            cwd=str(tmp_path),
            timeout_s=10,
            provider="p",
            fallback_model="",
        )


@pytest.mark.asyncio
async def test_run_opencode_kills_a_session_that_reaches_its_cap(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "budget")
    with pytest.raises(OpenCodeError) as err:
        await run_opencode(
            base_args(model="", agent="a", standalone=False),
            prompt="hi",
            binary=str(fake_opencode),
            cwd=str(tmp_path),
            timeout_s=30,
            provider="p",
            fallback_model="",
            max_cost_usd=0.5,
        )
    assert err.value.subtype == "error_max_budget_usd"
    assert err.value.usage is not None and err.value.usage.reported_cost_usd == pytest.approx(0.9)


@pytest.mark.asyncio
async def test_run_opencode_times_out_and_reports_progress(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "hang")
    with pytest.raises(OpenCodeTimeout) as err:
        await run_opencode(
            base_args(model="", agent="a", standalone=False),
            prompt="hi",
            binary=str(fake_opencode),
            cwd=str(tmp_path),
            timeout_s=2,
            provider="p",
            fallback_model="",
        )
    assert err.value.progress.turns == 1
    assert err.value.progress.spent_usd == pytest.approx(0.0153)


# --- the opencode model backend -------------------------------------------------------------
@pytest.mark.asyncio
async def test_model_runs_a_tool_less_turn_through_the_cli(
    fake_opencode: Path, tmp_path: Path
) -> None:
    model = OpenCodeModel(
        model_name="anthropic/claude-sonnet-4-5", binary=str(fake_opencode), max_budget_usd=2.0
    )
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
    assert result.session_id == "sess-oc-1"
    assert result.usage.reported_cost_usd == pytest.approx(0.01)
    assert model.estimate_cost_usd(result.usage) == pytest.approx(0.01)

    (call,) = _calls(tmp_path)
    argv = call["argv"]
    assert isinstance(argv, list)
    assert argv[:5] == ["run", "--format", "json", "--auto", "--agent"]
    assert argv[argv.index("--agent") + 1] == "lha-model"
    assert "--standalone" in argv
    assert argv[argv.index("--model") + 1] == "anthropic/claude-sonnet-4-5"
    assert call["project_disabled"] == "true"
    config = call["config"]
    assert isinstance(config, dict)
    agent = config["agents"]["lha-model"]
    assert agent["permissions"] == [{"action": "*", "resource": "*", "effect": "deny"}]
    assert agent["system"] == "SYSTEM PROMPT"
    assert "mcp" not in config  # a plain text turn: no tools
    stdin = str(call["stdin"])
    assert "do the thing" in stdin and "file a" in stdin
    assert '{"tool": "read_file", "arguments": {"path": "a"}}' in stdin  # the prior action


@pytest.mark.asyncio
async def test_model_single_turn_prompt_is_sent_verbatim(
    fake_opencode: Path, tmp_path: Path
) -> None:
    model = OpenCodeModel(binary=str(fake_opencode))
    await model.complete([ModelMessage(role="user", content="plan this")])
    (call,) = _calls(tmp_path)
    assert call["stdin"] == "plan this"
    assert "--model" not in call["argv"]  # the default model is OpenCode's own choice


@pytest.mark.asyncio
async def test_model_reports_a_missing_binary() -> None:
    model = OpenCodeModel(binary="/nonexistent/opencode", max_retries=0)
    with pytest.raises(OpenCodeError, match="LHA_OPENCODE_BIN"):
        await model.complete([ModelMessage(role="user", content="x")])
    health = await model.health_check(timeout_s=1)
    assert not health.ok and "not found" in health.detail


def test_worst_case_is_the_budget_cap_unless_the_model_is_priced() -> None:
    unpriced = OpenCodeModel(model_name="x", max_budget_usd=3.0)
    big = Usage(input_tokens=10_000_000, output_tokens=10_000_000)
    assert unpriced.estimate_cost_usd(big) == 3.0
    priced = OpenCodeModel(model_name="x", max_budget_usd=3.0, price=ModelPrice(1.0, 1.0))
    assert priced.estimate_cost_usd(Usage(input_tokens=1_000_000)) == pytest.approx(1.0)
    assert priced.estimate_cost_usd(big) == 3.0  # never above the cap the engine enforces


@pytest.mark.asyncio
async def test_health_runs_the_cli_version(fake_opencode: Path) -> None:
    model = OpenCodeModel(binary=str(fake_opencode))
    health = await model.health_check(timeout_s=10)
    assert health.ok and "0.0.0-fake" in health.detail


def test_build_provider_and_settings() -> None:
    model = build_provider(Settings(_env_file=None, model_backend="opencode"))  # type: ignore[call-arg]
    assert isinstance(model, OpenCodeModel) and model.name == "opencode:default"
    named = build_provider(
        Settings(_env_file=None, model_backend="opencode", model_name="anthropic/claude-sonnet-4-5")  # type: ignore[call-arg]
    )
    assert named.name == "opencode:anthropic/claude-sonnet-4-5"

    # The engine alone routes the other roles through opencode too; an explicit backend wins.
    alone = Settings(_env_file=None, lead_engine="opencode")  # type: ignore[call-arg]
    assert alone.model_backend == "opencode"
    explicit = Settings(_env_file=None, lead_engine="opencode", model_backend="stub")  # type: ignore[call-arg]
    assert explicit.model_backend == "stub"


def test_lead_engine_from_settings() -> None:
    engine = lead_engine(Settings(_env_file=None, lead_engine="opencode", opencode_model="m"))  # type: ignore[call-arg]
    assert isinstance(engine, OpenCodeEngine) and not engine.native
    assert engine.name == "opencode_engine:m"
    assert engine.session_event == "opencode_session"

    native = {"lead_engine": "opencode", "opencode_tools": "native"}
    with pytest.raises(ValueError, match="no isolation"):
        lead_engine(Settings(_env_file=None, **native))  # type: ignore[call-arg]
    unsafe = Settings(_env_file=None, sandbox="local", allow_unsafe_local=True, **native)  # type: ignore[call-arg]
    engine = lead_engine(unsafe)
    assert engine is not None and engine.native
    with pytest.raises(ValueError, match="ownership guard"):
        lead_engine(unsafe, guarded=True)


def test_lha_mode_denies_every_action_but_lha_tools() -> None:
    engine = OpenCodeEngine()
    assert engine._permissions() == [
        {"action": "*", "resource": "*", "effect": "deny"},
        {"action": "lha_*", "resource": "*", "effect": "allow"},
    ]
    config = engine._config("sys", McpBridge([]))
    server = config["mcp"]["servers"]["lha"]
    assert server["type"] == "remote" and server["codemode"] is False
    assert config["agents"][DEFAULT_AGENT]["system"] == "sys"


def test_native_mode_denies_history_publishing_and_the_web() -> None:
    engine = OpenCodeEngine(tools="native")
    rules = engine._permissions()
    assert {"action": "shell", "resource": "git push *", "effect": "deny"} in rules
    assert {"action": "webfetch", "resource": "*", "effect": "deny"} in rules
    assert rules[: len(NATIVE_DENY)] == list(NATIVE_DENY)


# --- the opencode lead engine, end to end ----------------------------------------------------
def _engine_settings(fake: Path, **overrides: object) -> Settings:
    values: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "lead_engine": "opencode",
        "opencode_bin": str(fake),
        "opencode_max_budget_usd": 1.0,
        "budget_usd_ceiling": 5.0,
        "max_cycles": 3,
    }
    values.update(overrides)
    return Settings(_env_file=None, **values)  # type: ignore[call-arg]


@pytest.mark.asyncio
async def test_a_cycle_is_one_opencode_session_using_lha_tools(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "mcp")
    calls = [["write_file", {"path": "hello.txt", "content": "hi\n"}], ["verify", {}]]
    monkeypatch.setenv("FAKE_OPENCODE_CALLS", json.dumps(calls))
    ws = tmp_path / "ws"
    ws.mkdir()

    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist("cmd:grep -qx hi hello.txt"),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_opencode),
    )

    assert summary.completed, summary.stopped_reason
    assert summary.cycles == 1
    assert summary.total_usd == pytest.approx(0.21 * (len(calls) + 1))  # the cost OpenCode reported
    (session,) = _calls(tmp_path)
    argv = session["argv"]
    assert isinstance(argv, list)
    assert argv[:5] == ["run", "--format", "json", "--auto", "--agent"]
    assert "--standalone" in argv and argv[argv.index("--agent") + 1] == "lha"
    config = session["config"]
    assert config["mcp"]["servers"]["lha"]["codemode"] is False
    perms = config["agents"]["lha"]["permissions"]
    assert perms == [
        {"action": "*", "resource": "*", "effect": "deny"},
        {"action": "lha_*", "resource": "*", "effect": "allow"},
    ]
    assert '{"tool"' not in config["agents"]["lha"]["system"]  # the LHA system prompt is injected
    assert "Create hello.txt" in str(session["stdin"])
    assert "cmd:grep -qx hi hello.txt" in str(session["stdin"])  # the witness is in the task

    assert (ws / "hello.txt").read_text() == "hi\n"  # written through LHA's write_file
    assert "opencode_session" in summary.trace_jsonl
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    (verified,) = [e["data"] for e in events if e["kind"] == "verify"]  # reused at the end
    assert verified["trigger"] == "tool" and verified["verdict"] == "passed"
    progress = [e["data"] for e in events if e["kind"] == "session_progress"]
    assert progress and progress[-1]["turns"] >= 2


@pytest.mark.asyncio
async def test_a_session_killed_at_its_cap_is_still_verified_and_charged(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "budget")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_opencode, opencode_max_budget_usd=0.5),
    )
    assert summary.completed  # the work in the workdir passed, though the session hit its cap
    assert summary.total_usd == pytest.approx(0.9)  # what the session streamed before the kill
    assert "error_max_budget_usd" in summary.trace_jsonl


@pytest.mark.asyncio
async def test_a_session_killed_at_its_timeout_is_charged_what_it_spent(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "hang")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_opencode, opencode_timeout_s=3),
    )
    assert summary.completed  # the work in the workdir is still verified
    assert summary.total_usd == pytest.approx(0.0153)  # its one turn, not the $1 cap
    events = [json.loads(line) for line in summary.trace_jsonl.splitlines()]
    (session,) = [e["data"] for e in events if e["kind"] == "opencode_session"]
    assert "did not finish within 3s" in str(session["stopped"])


@pytest.mark.asyncio
async def test_a_killed_session_on_an_unpriced_model_is_charged_its_cap(
    fake_opencode: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("FAKE_OPENCODE_MODE", "hang-unpriced")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "hello.txt").write_text("already here\n")
    summary = await run_mission_local(
        workdir=str(ws),
        title="Hello",
        description="Write hello.txt",
        checklist=_checklist(),
        checks=checks_from_commands([["test", "-f", "hello.txt"]]),
        settings=_engine_settings(fake_opencode, opencode_timeout_s=3),
    )
    assert summary.total_usd == pytest.approx(1.0)  # the session's cap: its spend had no cost
