"""Human approval for irreversible tool calls (``git push``, releases, uploads).

The dispatcher sends every command the classifier flags to a ``HITLGate``. Where the human
answers depends on how the mission runs:

* **Durable missions** use ``DeferredApprovalGate``. An activity must not block for hours on a
  person, so the gate records the request and DENIES it for now; the cycle reports the pending
  request, the ``MissionWorkflow`` opens a durable human gate (status ``WAITING_ON_HUMAN``), and an
  approval (``lha mission-approve <id> --decision approve``) is passed to the next cycle, where the
  SAME action — identified by its fingerprint — is allowed exactly once.
* **Local runs** can use ``console_gate()`` to ask on the terminal, or no gate (denied).

A fingerprint covers the tool and its exact arguments, so approving ``git push origin main`` does
not approve ``git push --force`` or a push to another remote.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import sys
from collections.abc import Iterable

from lha.contracts.hitl import GateDecision, GateRequest, GateResolution
from lha.hitl.gate import CallbackGate

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


def console_gate() -> CallbackGate:
    """A gate that asks on the terminal (for attended local runs); anything but "y" rejects."""

    async def ask(req: GateRequest) -> GateDecision | None:
        prompt = (
            f"\n[lha] APPROVAL NEEDED: {req.question}\n"
            f"      arguments: {req.context.get('arguments', '')}\n"
            "      allow? [y/N] "
        )

        def _read() -> str:
            sys.stderr.write(prompt)
            sys.stderr.flush()
            return sys.stdin.readline()

        answer = (await asyncio.to_thread(_read)).strip().lower()
        return GateDecision.APPROVE if answer in ("y", "yes") else GateDecision.REJECT

    return CallbackGate(ask)
