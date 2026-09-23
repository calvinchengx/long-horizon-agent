"""Human approval for irreversible tool calls (``git push``, releases, uploads).

The dispatcher sends every command the classifier flags to a ``HITLGate``. Where the human
answers depends on how the mission runs:

* **Durable missions** use ``DeferredApprovalGate``. An activity must not block for hours on a
  person, so the gate records the request and DENIES it for now; the cycle reports the pending
  request, the ``MissionWorkflow`` opens a durable human gate (status ``WAITING_ON_HUMAN``), and an
  approval (``lha mission-approve <id> --decision approve``) is passed to the next cycle, where the
  SAME action — identified by its fingerprint — is allowed exactly once.
* **Local runs** can use ``console_gate()`` (a ``TerminalApprover``) to ask on the terminal: it
  shows the exact argv and the classifier's reason, asks y/N (default reject), rejects without
  asking when stdin is not a TTY, and rejects on timeout after reminders on the escalation
  ladder. With no gate, flagged commands are denied.

A fingerprint covers the tool and its exact arguments, so approving ``git push origin main`` does
not approve ``git push --force`` or a push to another remote.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import select
import shlex
import sys
import time
from collections.abc import Callable, Iterable
from typing import Any, TextIO

from lha.config import Settings, get_settings
from lha.contracts.hitl import GateDecision, GateRequest, GateResolution
from lha.contracts.state import EventRecord
from lha.hitl.escalation import escalation_schedule, next_rung
from lha.hitl.notify import post_webhook_sync
from lha.obs.redact import redact_text

#: ``GateResolution.resolved_by`` for a request that was queued for a later human decision.
PENDING = "pending-approval"


def action_fingerprint(tool: str, arguments: dict[str, object]) -> str:
    """A stable id for one exact tool call (tool name + canonical JSON arguments)."""
    canonical = json.dumps(
        {"tool": tool, "arguments": arguments}, sort_keys=True, separators=(",", ":"), default=str
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:32]


class DeferredApprovalGate:
    """Approves only previously-approved fingerprints (once each); queues everything else."""

    def __init__(self, approved: Iterable[str] = ()) -> None:
        self._approved = set(approved)
        self.pending: list[dict[str, str]] = []
        self.used: list[str] = []

    async def request(self, req: GateRequest) -> GateResolution:
        fingerprint = req.context.get("fingerprint", "")
        if fingerprint and fingerprint in self._approved and fingerprint not in self.used:
            self.used.append(fingerprint)
            return GateResolution(
                gate_id=req.gate_id, decision=GateDecision.APPROVE, resolved_by="operator"
            )
        if fingerprint and all(p["fingerprint"] != fingerprint for p in self.pending):
            self.pending.append(
                {
                    "fingerprint": fingerprint,
                    "tool": req.context.get("tool", ""),
                    "reason": req.context.get("reason", ""),
                    "arguments": req.context.get("arguments", ""),
                }
            )
        return GateResolution(
            gate_id=req.gate_id, decision=GateDecision.REJECT, resolved_by=PENDING
        )


#: ``read_line(timeout_s)`` -> a line, ``None`` on timeout, ``""`` on end of input.
ReadLine = Callable[[float], str | None]
#: ``notify(payload)`` -> webhook outcome; called from a worker thread, never raises.
Notify = Callable[[dict[str, Any]], str]


def request_argv(req: GateRequest) -> list[str]:
    """The exact argv of a gated command call (``[]`` for non-command tools)."""
    raw = req.context.get("argv", "")
    if not raw:
        return []
    try:
        value = json.loads(raw)
    except ValueError:
        return []
    return [str(token) for token in value] if isinstance(value, list) else []


def describe(req: GateRequest) -> str:
    """One-line human description of the call: the shell-quoted argv, else the arguments."""
    argv = request_argv(req)
    return shlex.join(argv) if argv else req.context.get("arguments", "")


def select_readline(stream: TextIO) -> ReadLine:
    """A ``ReadLine`` over a real terminal: ``select`` with a timeout, then ``readline``."""

    def read(timeout_s: float) -> str | None:
        ready, _, _ = select.select([stream], [], [], max(0.0, timeout_s))
        if not ready:
            return None
        return stream.readline()

    return read


def _is_tty(stream: TextIO) -> Callable[[], bool]:
    def check() -> bool:
        try:
            return stream.isatty()
        except (AttributeError, ValueError):
            return False

    return check


def _denied(req: GateRequest, by: str, *, defaulted: bool) -> GateResolution:
    return GateResolution(
        gate_id=req.gate_id, decision=GateDecision.REJECT, resolved_by=by, defaulted=defaulted
    )


class TerminalApprover:
    """Asks on the terminal: exact argv + reason, ``y/N`` (default deny), timeout → deny.

    Stdin that is not a TTY is never read: the call is denied at once. While waiting, the approver
    walks the escalation ladder: at each offset in ``escalation_seconds`` inside
    ``timeout_seconds`` it prints a reminder, keeps a ``gate_reminder`` event (the dispatcher
    commits it with the checkpoint) and notifies the webhook if one is configured; at the timeout
    the call is denied. Prompts are serialized (one question on the terminal at a time).
    """

    def __init__(
        self,
        *,
        timeout_seconds: float = 3600,
        escalation_seconds: Iterable[int | float] = (),
        stdin: TextIO | None = None,
        out: TextIO | None = None,
        is_tty: Callable[[], bool] | None = None,
        read_line: ReadLine | None = None,
        notify: Notify | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        stdin = stdin or sys.stdin
        self._timeout = float(timeout_seconds)
        self._schedule = escalation_schedule(self._timeout, escalation_seconds)
        self._out = out or sys.stderr
        self._is_tty = is_tty or _is_tty(stdin)
        self._read_line = read_line or select_readline(stdin)
        self._notify = notify
        self._clock = clock
        self._lock = asyncio.Lock()
        self._events: list[EventRecord] = []

    def drain_events(self) -> list[EventRecord]:
        events, self._events = self._events, []
        return events

    async def request(self, req: GateRequest) -> GateResolution:
        if not self._is_tty():
            return _denied(req, "non-interactive (stdin is not a TTY)", defaulted=True)
        async with self._lock:
            return await asyncio.to_thread(self._prompt, req)

    # --- internals (run in a worker thread) --------------------------------------------
    def _say(self, text: str) -> None:
        self._out.write(text)
        self._out.flush()

    def _send(self, req: GateRequest, event: str, **extra: Any) -> None:
        if self._notify is None:
            return
        self._notify(
            {
                "source": "lha",
                "kind": "tool_call",
                "event": event,
                "gate_id": req.gate_id,
                "question": redact_text(req.question),
                "tool": req.context.get("tool", ""),
                "argv": [redact_text(t) for t in request_argv(req)],
                "reason": req.context.get("reason", ""),
                "default_action": "reject",
                **extra,
            }
        )

    def _prompt(self, req: GateRequest) -> GateResolution:
        label = "argv" if request_argv(req) else "args"
        self._say(
            "\n[lha] APPROVAL NEEDED - an irreversible command was flagged by the classifier\n"
            f"  tool:    {req.context.get('tool', '')}\n"
            f"  {label}:    {describe(req)}\n"
            f"  reason:  {req.context.get('reason', '')}\n"
            f"  timeout: {int(self._timeout)}s (default: reject)\n"
            "Allow this exact call? [y/N]: "
        )
        self._send(req, "opened")
        start = self._clock()
        sent = 0
        while True:
            rung = next_rung(self._clock() - start, self._timeout, self._schedule, sent)
            line = self._read_line(rung.wait_seconds)
            if line is None:
                if rung.step == 0:
                    self._say("\n[lha] no answer before the timeout: rejected.\n")
                    self._send(req, "defaulted", decision="reject")
                    return _denied(req, "timeout", defaulted=True)
                sent = rung.step
                remaining = max(0, int(self._timeout - (self._clock() - start)))
                self._say(
                    f"\n[lha] reminder {sent}: still waiting for your answer "
                    f"(rejected in {remaining}s). Allow this exact call? [y/N]: "
                )
                self._events.append(
                    EventRecord(
                        kind="gate_reminder",
                        payload={
                            "gate_id": req.gate_id,
                            "gate": "tool_call",
                            "step": sent,
                            "tool": req.context.get("tool", ""),
                            "fingerprint": req.context.get("fingerprint", ""),
                        },
                    )
                )
                self._send(req, "reminder", step=sent)
                continue
            if line == "":
                self._say("\n[lha] end of input: rejected.\n")
                self._send(req, "defaulted", decision="reject")
                return _denied(req, "end of input", defaulted=True)
            approved = line.strip().lower() in ("y", "yes")
            self._send(req, "resolved", decision="approve" if approved else "reject")
            if approved:
                return GateResolution(
                    gate_id=req.gate_id, decision=GateDecision.APPROVE, resolved_by="human"
                )
            return _denied(req, "human", defaulted=False)


def settings_notifier(settings: Settings) -> Notify | None:
    """A webhook notifier from settings (``None`` when ``gate_webhook_url`` is unset)."""
    if settings.gate_webhook_url is None:
        return None
    url = settings.gate_webhook_url.get_secret_value()
    timeout = settings.gate_webhook_timeout_seconds

    def notify(payload: dict[str, Any]) -> str:
        return post_webhook_sync(url, payload, timeout=timeout)

    return notify


def console_gate(settings: Settings | None = None) -> TerminalApprover:
    """The terminal approver for attended local runs (``--approve-interactive``).

    Anything but ``y``/``yes`` rejects; no TTY rejects without asking; no answer within
    ``LHA_APPROVAL_TIMEOUT_SECONDS`` rejects, after reminders at ``LHA_GATE_ESCALATION_SECONDS``.
    """
    settings = settings or get_settings()
    return TerminalApprover(
        timeout_seconds=settings.console_approval_timeout_s,
        escalation_seconds=settings.gate_escalation_seconds,
        notify=settings_notifier(settings),
    )
