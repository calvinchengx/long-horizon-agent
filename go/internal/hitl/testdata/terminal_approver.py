"""Python side of the Go console gate's parity test (go/internal/hitl/approvals_test.go).

Reads scenarios (JSON, argv[1]); drives lha.hitl.approvals.TerminalApprover with a scripted
read_line (null = timeout, advancing a fake clock by the wait), and prints, per scenario, the
terminal output, the resolution, the drained events and every webhook body exactly as httpx
would encode it.
"""

import asyncio
import json
import sys

from lha.contracts.hitl import GateRequest
from lha.hitl.approvals import TerminalApprover


def run(scenario: dict) -> dict:
    answers = list(scenario["answers"] or [])
    now = [0.0]
    out: list[str] = []
    bodies: list[str] = []

    class Out:
        def write(self, text: str) -> None:
            out.append(text)

        def flush(self) -> None:
            pass

    def read_line(timeout: float) -> str | None:
        answer = answers.pop(0)
        if answer is None:
            now[0] += timeout
        return answer

    def notify(payload: dict) -> str:
        bodies.append(
            json.dumps(payload, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
        )
        return "sent"

    approver = TerminalApprover(
        timeout_seconds=scenario["timeout"],
        escalation_seconds=scenario["escalation"] or [],
        out=Out(),
        is_tty=lambda: scenario["tty"],
        read_line=read_line,
        notify=notify,
        clock=lambda: now[0],
    )
    req = GateRequest(
        gate_id=scenario["gate_id"], question=scenario["question"], context=scenario["context"]
    )
    res = asyncio.run(approver.request(req))
    return {
        "out": "".join(out),
        "resolution": {
            "gate_id": res.gate_id,
            "decision": res.decision.value,
            "resolved_by": res.resolved_by,
            "defaulted": res.defaulted,
        },
        "events": [{"kind": e.kind, "payload": e.payload} for e in approver.drain_events()],
        "webhook": bodies,
    }


def main() -> None:
    with open(sys.argv[1], encoding="utf-8") as fh:
        scenarios = json.load(fh)
    print(json.dumps([run(s) for s in scenarios], sort_keys=True))


if __name__ == "__main__":
    main()
