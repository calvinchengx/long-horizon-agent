"""Terminal approver, escalation ladder, webhook, gate activities and the gate CLI (no Temporal)."""

from __future__ import annotations

import io
import json
import sys
from pathlib import Path
from typing import Any

import httpx
import pytest
from pydantic import SecretStr
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.hitl import GateDecision, GateRequest, GateResolution
from lha.contracts.model import ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, EventRecord
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.durable.activities import _declare_impossible, _notify_gate, gate_notice_payload
from lha.durable.signals import GATE_DEADLOCK, GATE_TOOL_CALL, SIGNAL_HUMAN_DECISION, SIGNAL_SNOOZE
from lha.durable.types import FinalizeInput, GateNotice, GateView, PendingApproval
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.hitl.approvals import (
    DeferredApprovalGate,
    TerminalApprover,
    console_gate,
    describe,
    request_argv,
    select_readline,
    settings_notifier,
)
from lha.hitl.escalation import Rung, escalation_schedule, next_rung
from lha.hitl.notify import post_webhook, post_webhook_sync
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

runner = CliRunner()
PUSH = ["git", "push", "origin", "main"]


def _request(argv: list[str] | None = PUSH, **context: str) -> GateRequest:
    ctx = {"tool": "run_command", "reason": "git push (outward-facing)", "fingerprint": "fp1"}
    ctx["arguments"] = repr({"argv": argv}) if argv is not None else "{'url': 'https://x.test'}"
    if argv is not None:
        ctx["argv"] = json.dumps(argv)
    ctx.update(context)
    return GateRequest(gate_id="m:tool:c1", question="Allow?", context=ctx)


# --- escalation ladder ---------------------------------------------------------------------
def test_escalation_schedule_keeps_offsets_inside_the_timeout() -> None:
    assert escalation_schedule(3600, [2700, 900, 900, 0, -5, 3600, 9999]) == [900.0, 2700.0]
    assert escalation_schedule(10, []) == []


def test_next_rung_walks_reminders_then_the_timeout() -> None:
    schedule = [10.0, 30.0]
    assert next_rung(0, 60, schedule, 0) == Rung(wait_seconds=10.0, step=1)
    assert next_rung(12, 60, schedule, 1) == Rung(wait_seconds=18.0, step=2)
    assert next_rung(31, 60, schedule, 2) == Rung(wait_seconds=29.0, step=0)
    assert next_rung(99, 60, schedule, 2) == Rung(wait_seconds=0.0, step=0)


# --- terminal approver ---------------------------------------------------------------------
def _approver(
    answers: list[str | None],
    *,
    tty: bool = True,
    timeout: float = 60,
    steps: tuple[int, ...] = (),
    notify: Any = None,
) -> tuple[TerminalApprover, io.StringIO, list[float]]:
    out = io.StringIO()
    now = [0.0]
    waits: list[float] = []

    def read_line(timeout_s: float) -> str | None:
        waits.append(timeout_s)
        answer = answers.pop(0) if answers else None
        if answer is None:
            now[0] += timeout_s  # nobody typed anything for the whole window
        return answer

    approver = TerminalApprover(
        timeout_seconds=timeout,
        escalation_seconds=steps,
        out=out,
        is_tty=lambda: tty,
        read_line=read_line,
        notify=notify,
        clock=lambda: now[0],
    )
    return approver, out, waits


@pytest.mark.asyncio
async def test_terminal_approver_shows_argv_and_reason_and_accepts_yes() -> None:
    approver, out, _ = _approver(["y\n"])
    result = await approver.request(_request())
    assert result.decision is GateDecision.APPROVE and not result.defaulted
    shown = out.getvalue()
    assert "git push origin main" in shown and "outward-facing" in shown
    assert "[y/N]" in shown and "default: reject" in shown


@pytest.mark.asyncio
@pytest.mark.parametrize("answer", ["\n", "n\n", "sure\n", "YES please\n"])
async def test_terminal_approver_default_is_reject(answer: str) -> None:
    approver, _, _ = _approver([answer])
    result = await approver.request(_request())
    assert result.decision is GateDecision.REJECT and not result.defaulted


@pytest.mark.asyncio
async def test_terminal_approver_rejects_without_a_tty_and_on_eof() -> None:
    approver, out, waits = _approver(["y\n"], tty=False)
    result = await approver.request(_request())
    assert result.decision is GateDecision.REJECT and result.defaulted
    assert "not a TTY" in result.resolved_by and waits == [] and out.getvalue() == ""
    eof, _, _ = _approver([""])
    closed = await eof.request(_request())
    assert closed.decision is GateDecision.REJECT and closed.defaulted


@pytest.mark.asyncio
async def test_terminal_approver_escalates_then_rejects_on_timeout() -> None:
    sent: list[dict[str, Any]] = []

    def notify(payload: dict[str, Any]) -> str:
        sent.append(payload)
        return "sent"

    approver, out, waits = _approver([None, None, None], steps=(10, 30, 90), notify=notify)
    result = await approver.request(_request())
    assert result.decision is GateDecision.REJECT and result.defaulted
    assert waits == [10.0, 20.0, 30.0]  # reminder 1, reminder 2, then the timeout
    assert out.getvalue().count("reminder") == 2 and "rejected" in out.getvalue()
    assert [p["event"] for p in sent] == ["opened", "reminder", "reminder", "defaulted"]
    assert sent[0]["argv"] == PUSH and sent[-1]["decision"] == "reject"
    events = approver.drain_events()
    assert [e.payload["step"] for e in events] == [1, 2] and approver.drain_events() == []


@pytest.mark.asyncio
async def test_terminal_approver_answer_after_a_reminder_and_non_command_calls() -> None:
    approver, _, waits = _approver([None, "yes\n"], steps=(5,))
    assert (await approver.request(_request())).decision is GateDecision.APPROVE
    assert waits == [5.0, 55.0]
    other, out, _ = _approver(["n\n"])
    req = _request(None)
    assert request_argv(req) == [] and describe(req) == "{'url': 'https://x.test'}"
    await other.request(req)
    assert "https://x.test" in out.getvalue()


def test_request_argv_tolerates_bad_context(tmp_path: Path) -> None:
    for raw in ("not json", '{"a": 1}'):
        req = GateRequest(gate_id="g", question="?", context={"argv": raw})
        assert request_argv(req) == []
    path = tmp_path / "in.txt"
    path.write_text("y\n", encoding="utf-8")
    with path.open(encoding="utf-8") as stream:
        assert select_readline(stream)(1.0) == "y\n"


def test_console_gate_and_notifier_follow_settings() -> None:
    settings = Settings(_env_file=None, approval_timeout_seconds=30)  # type: ignore[call-arg]
    assert isinstance(console_gate(settings), TerminalApprover)
    assert settings_notifier(settings) is None
    hooked = Settings(_env_file=None, gate_webhook_url="https://hooks.test/T0KEN")  # type: ignore[call-arg]
    assert settings_notifier(hooked) is not None
    assert hooked.redacted()["gate_webhook_url"] == "***" and "T0KEN" not in repr(hooked)


def test_gate_settings_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LHA_GATE_ESCALATION_SECONDS", "[60, 120]")
    monkeypatch.setenv("LHA_DEADLOCK_GATE_DEFAULT", "impossible")
    settings = Settings(_env_file=None)  # type: ignore[call-arg]
    assert settings.gate_escalation_seconds == [60, 120]
    assert settings.deadlock_gate_default == "impossible"
    monkeypatch.setenv("LHA_DEADLOCK_GATE_DEFAULT", "retry")  # never an unattended default
    with pytest.raises(ValueError):
        Settings(_env_file=None)  # type: ignore[call-arg]


# --- dispatcher records every answer ---------------------------------------------------------
async def _ctx(tmp_path: Path) -> ToolContext:
    return ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))


def _push() -> ToolCall:
    return ToolCall(id="p1", name="run_command", arguments={"argv": PUSH})


@pytest.mark.asyncio
async def test_dispatcher_records_no_gate_errors_and_pending(tmp_path: Path) -> None:
    ctx = await _ctx(tmp_path)
    bare = AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True)
    assert not (await bare.dispatch(_push(), ctx)).ok
    (event,) = bare.drain_events()
    assert event.kind == "tool_approval" and event.payload["decision"] == "reject"
    assert event.payload["resolved_by"] == "no human gate configured" and bare.drain_events() == []

    class _Exploding:
        async def request(self, req: GateRequest) -> GateResolution:
            raise RuntimeError("gate down")

    broken = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=_Exploding()
    )
    await broken.dispatch(_push(), ctx)
    assert broken.drain_events()[0].payload["resolved_by"] == "gate error: RuntimeError"

    deferred = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=DeferredApprovalGate()
    )
    result = await deferred.dispatch(_push(), ctx)
    assert "queued for human approval" in (result.error or "")
    (queued,) = deferred.drain_events()
    assert queued.payload["decision"] == "pending" and not queued.payload["approved"]


@pytest.mark.asyncio
async def test_dispatcher_merges_the_approvers_reminders(tmp_path: Path) -> None:
    approver, _, _ = _approver([None, "n\n"], steps=(1,))
    dispatcher = AllowListDispatcher.for_tools(
        default_local_tools(), allow_mutating=True, gate=approver
    )
    await dispatcher.dispatch(_push(), await _ctx(tmp_path))
    assert [e.kind for e in dispatcher.drain_events()] == ["tool_approval", "gate_reminder"]


def _events(workdir: Path) -> list[EventRecord]:
    raw = git_ops.run_git(workdir, "show", "HEAD:.lha/events.ndjson")
    return [EventRecord.model_validate_json(line) for line in raw.splitlines() if line.strip()]


@pytest.mark.asyncio
@pytest.mark.parametrize("answer", ["y\n", "n\n"])
async def test_local_run_asks_and_commits_the_decision(tmp_path: Path, answer: str) -> None:
    marker = tmp_path / "ran.log"
    argv = ["sh", "-c", f"echo ran >> {marker}; git push lha-no-such-remote HEAD"]
    model = StubModel(
        script=[
            TurnResult(
                tool_calls=[ToolCall(id="c", name="run_command", arguments={"argv": argv})],
                stop_reason="tool_use",
            ),
            TurnResult(text='{"done": true, "summary": "ok"}'),
        ]
    )
    approver, out, _ = _approver([answer])
    work = tmp_path / "ws"
    summary = await run_mission_local(
        workdir=str(work),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="push")]),
        checks=[Check(name="green", command=[sys.executable, "-c", "pass"])],
        settings=Settings(
            _env_file=None,  # type: ignore[call-arg]
            sandbox="local",
            allow_unsafe_local=True,
            max_cycles=3,
            max_turns_per_cycle=3,
            max_replans=0,
        ),
        model=model,
        gate=approver,
    )
    assert summary.completed
    assert "git push lha-no-such-remote HEAD" in out.getvalue()
    approved = answer == "y\n"
    assert marker.exists() is approved
    (event,) = [e for e in _events(work) if e.kind == "tool_approval"]
    assert event.payload["approved"] is approved and event.cycle_id == "c1"


# --- webhook -------------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_post_webhook_outcomes() -> None:
    seen: list[dict[str, Any]] = []

    def ok(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(200)

    def down(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused")

    url = "https://h.test"
    assert await post_webhook(None, {"a": 1}, timeout=1) == "off"
    assert await post_webhook(url, {"a": 1}, timeout=1, transport=httpx.MockTransport(ok)) == "sent"
    assert seen == [{"a": 1}]
    bad = httpx.MockTransport(lambda _r: httpx.Response(500))
    assert await post_webhook(url, {}, timeout=1, transport=bad) == "failed: HTTP 500"
    failed = await post_webhook(url, {}, timeout=1, transport=httpx.MockTransport(down))
    assert failed == "failed: ConnectError"
    assert post_webhook_sync("", {}, timeout=1) == "off"
    assert post_webhook_sync(url, {}, timeout=1, transport=httpx.MockTransport(ok)) == "sent"
    assert post_webhook_sync(url, {}, timeout=1, transport=httpx.MockTransport(down)).startswith(
        "failed"
    )


# --- gate activities -----------------------------------------------------------------------
PENDING = PendingApproval(
    fingerprint="fp-push", tool="run_command", reason="git push", arguments="['git', 'push']"
)


async def _mission(tmp_path: Path) -> Path:
    items = [ChecklistItem(id="01", description="x", status="blocked")]
    await GitMissionAnchor(tmp_path).initialize(
        title="t", description="d", items=Checklist(items=items)
    )
    return tmp_path


def test_gate_notice_payload_redacts_secrets() -> None:
    request = PendingApproval(
        fingerprint="f",
        tool="run_command",
        reason="curl upload",
        arguments="['curl', '-H', 'Authorization: Bearer sk-ant-abcdefghijklmnopqrstu']",
    )
    payload = gate_notice_payload(
        GateNotice(
            mission_id="m",
            workdir="/w",
            gate_id="g",
            kind=GATE_TOOL_CALL,
            event="reminder",
            step=2,
            decision="reject",
            request=request,
        )
    )
    assert payload["step"] == 2 and payload["decision"] == "reject"
    assert "sk-ant" not in json.dumps(payload)


@pytest.mark.asyncio
async def test_notify_gate_records_in_the_anchor_and_posts(tmp_path: Path) -> None:
    workdir = await _mission(tmp_path)
    (workdir / "partial.txt").write_text("uncommitted", encoding="utf-8")
    posts: list[dict[str, Any]] = []

    def hook(request: httpx.Request) -> httpx.Response:
        posts.append(json.loads(request.content))
        return httpx.Response(200)

    settings = Settings(_env_file=None, gate_webhook_url=SecretStr("https://h.test/x"))  # type: ignore[call-arg]
    notice = GateNotice(
        mission_id="m",
        workdir=str(workdir),
        gate_id="approval-fp",
        kind=GATE_TOOL_CALL,
        event="opened",
        options=["approve", "reject"],
        default_action="reject",
        request=PENDING,
    )
    result = await _notify_gate(notice, settings=settings, transport=httpx.MockTransport(hook))
    assert result.recorded and result.webhook == "sent"
    assert posts[0]["request"]["fingerprint"] == "fp-push"
    (event,) = [e for e in _events(workdir) if e.kind == "gate_opened"]
    assert event.payload["gate_id"] == "approval-fp"
    assert "partial.txt" not in git_ops.run_git(workdir, "ls-files")
    missing = GateNotice(
        mission_id="m", workdir=str(tmp_path / "nope"), gate_id="g", kind="x", event="opened"
    )
    off = await _notify_gate(missing, settings=Settings(_env_file=None))  # type: ignore[call-arg]
    assert not off.recorded and off.webhook == "off"


@pytest.mark.asyncio
async def test_declare_impossible_is_a_final_idempotent_checkpoint(tmp_path: Path) -> None:
    workdir = await _mission(tmp_path)
    inp = FinalizeInput(mission_id="m", workdir=str(workdir), cycle_id="impossible-4", reason="r")
    first = await _declare_impossible(inp)
    again = await _declare_impossible(inp)
    assert git_ops.head_sha(workdir) == first.head_sha == again.head_sha
    final = [e for e in _events(workdir) if e.kind == "mission_impossible"]
    assert len(final) == 1 and final[0].payload["blocked"] == ["01"]


# --- CLI -----------------------------------------------------------------------------------
def _tool_gate() -> GateView:
    return GateView(
        gate_id="approval-fp-push",
        kind=GATE_TOOL_CALL,
        question="Mission m wants to run an irreversible action. Approve or reject?",
        options=["approve", "reject"],
        default_action="reject",
        opened_at="t0",
        deadline="t1",
        escalations_sent=1,
        next_escalation_at="t2",
        request=PENDING,
    )


def _deadlock_gate() -> GateView:
    return GateView(
        gate_id="deadlock-4",
        kind=GATE_DEADLOCK,
        question="Mission m is deadlocked.",
        options=["retry", "abort", "impossible"],
        default_action="abort",
        recommended="impossible",
    )


def test_check_decision_and_format_gate() -> None:
    assert cli.check_decision(_tool_gate(), " Approve ") == "approve"
    with pytest.raises(ValueError, match="approve, reject"):
        cli.check_decision(_tool_gate(), "retry")
    with pytest.raises(ValueError, match="retry, abort, impossible"):
        cli.check_decision(_deadlock_gate(), "approve")
    assert cli.check_decision(None, "impossible") == "impossible"  # held for the next gate
    assert cli.format_gate(None) == []
    tool = "\n".join(cli.format_gate(_tool_gate()))
    assert "fp-push" in tool and "default on timeout: reject" in tool and "next reminder" in tool
    assert "recommended: impossible" in "\n".join(cli.format_gate(_deadlock_gate()))


class _Handle:
    def __init__(self, gate: GateView | None, *, old_worker: bool = False) -> None:
        self.gate = gate
        self.old_worker = old_worker
        self.signals: list[tuple[str, Any]] = []

    async def query(self, name: str, **_kw: Any) -> Any:
        from temporalio.client import WorkflowQueryFailedError

        if self.old_worker and name in ("gate_v1", "gate_log_v1", "resume_at"):
            raise WorkflowQueryFailedError("unknown query")
        return {
            "status_v1": "WAITING_ON_HUMAN" if self.gate else "SLEEPING",
            "cycles_done": 3,
            "resume_at": 0.0 if self.gate else 1_900_000_000.0,
            "gate_v1": self.gate,
            "gate_log_v1": ["t tool_call gate approval-fp-push opened (default reject)"],
            "open_question": "legacy question [approve / reject]",
        }[name]

    async def signal(self, name: str, payload: Any) -> None:
        self.signals.append((name, payload))


def _fake_client(monkeypatch: pytest.MonkeyPatch, handle: _Handle) -> None:
    class _Client:
        def get_workflow_handle(self, workflow_id: str) -> _Handle:
            assert workflow_id == "mission:m1"
            return handle

    async def connect_client(_settings: Any = None) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)


def test_mission_approve_validates_against_the_open_gate(monkeypatch: pytest.MonkeyPatch) -> None:
    handle = _Handle(_deadlock_gate())
    _fake_client(monkeypatch, handle)
    refused = runner.invoke(cli.app, ["mission-approve", "m1", "--decision", "approve"])
    assert refused.exit_code == 2 and "retry, abort, impossible" in refused.output
    assert handle.signals == []
    ok = runner.invoke(cli.app, ["mission-approve", "m1", "--decision", "impossible"])
    assert ok.exit_code == 0 and handle.signals == [(SIGNAL_HUMAN_DECISION, "impossible")]
    unknown = runner.invoke(cli.app, ["mission-approve", "m1", "--decision", "maybe"])
    assert unknown.exit_code == 2


def test_mission_status_shows_gate_sleep_and_log(monkeypatch: pytest.MonkeyPatch) -> None:
    _fake_client(monkeypatch, _Handle(_tool_gate()))
    out = runner.invoke(cli.app, ["mission-status", "m1"]).output
    assert "status=WAITING_ON_HUMAN cycles=3" in out and "fp-push" in out
    assert "recent gate events" in out
    _fake_client(monkeypatch, _Handle(None))
    assert "sleeping until 2030" in runner.invoke(cli.app, ["mission-status", "m1"]).output
    _fake_client(monkeypatch, _Handle(None, old_worker=True))
    old = runner.invoke(cli.app, ["mission-status", "m1"])
    assert old.exit_code == 0 and "waiting on: legacy question" in old.output


def test_mission_snooze(monkeypatch: pytest.MonkeyPatch) -> None:
    handle = _Handle(None)
    _fake_client(monkeypatch, handle)
    assert runner.invoke(cli.app, ["mission-snooze", "m1", "--seconds", "600"]).exit_code == 0
    assert "woken" in runner.invoke(cli.app, ["mission-snooze", "m1", "--seconds", "0"]).output
    assert handle.signals == [(SIGNAL_SNOOZE, 600), (SIGNAL_SNOOZE, 0)]


def _use_settings(monkeypatch: pytest.MonkeyPatch, settings: Settings) -> None:
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)


def test_mission_start_wires_the_ladder_and_sleeping(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _use_settings(monkeypatch, Settings(_env_file=None, model_backend="stub"))  # type: ignore[call-arg]
    started: list[Any] = []

    class _Client:
        async def start_workflow(self, _run: Any, inp: Any, **_kw: Any) -> None:
            started.append(inp)

    async def connect_client(_settings: Settings) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)
    result = runner.invoke(
        cli.app,
        [
            *["mission-start", "--task", "t", "--workdir", str(tmp_path)],
            *["--deadlock-default", "impossible", "--cycle-pause-seconds", "30"],
            *["--start-in-seconds", "120"],
        ],
    )
    assert result.exit_code == 0, result.output
    inp = started[0]
    assert inp.deadlock_gate_default == "impossible" and inp.cycle_pause_seconds == 30
    assert inp.gate_escalation_seconds == [900, 2700, 14_400, 43_200] and inp.resume_at > 0
    bad = runner.invoke(cli.app, ["mission-start", "--task", "t", "--deadlock-default", "retry"])
    assert bad.exit_code == 2 and "abort, impossible" in bad.output
