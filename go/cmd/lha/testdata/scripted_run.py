"""Python side of the Go CLI's scripted end-to-end parity test (go/cmd/lha/e2e_test.go).

Reads a JSON spec from argv[1] ({"workdir", "title", "items", "checks", "script"}), runs
``run_mission_local`` with a scripted ``StubModel`` on the real local sandbox and real tools
(settings from the environment: LHA_SANDBOX=local, LHA_ALLOW_UNSAFE_LOCAL=true), and prints the
CLI's three-line report. Exit code 0 when complete, else 1 — like ``lha run-local``.
"""

import asyncio
import json
import sys

from lha.agent.runner import run_mission_local
from lha.config import get_settings
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import checks_from_commands
from lha.model.stub import StubModel


def main() -> int:
    with open(sys.argv[1], encoding="utf-8") as fh:
        spec = json.load(fh)
    script = [TurnResult.model_validate(turn) for turn in spec["script"]]
    checklist = Checklist(
        items=[
            ChecklistItem(id=f"{i + 1:02d}", description=d) for i, d in enumerate(spec["items"])
        ]
    )
    summary = asyncio.run(
        run_mission_local(
            workdir=spec["workdir"],
            title=spec["title"],
            description="",
            checklist=checklist,
            checks=checks_from_commands(spec["checks"]),
            settings=get_settings(),
            model=StubModel(script=script),
        )
    )
    print(f"mission {summary.mission_id}: {summary.stopped_reason}")
    print(
        f"items {summary.items_done}/{summary.items_total}  cycles {summary.cycles}  "
        f"cost ${summary.total_usd:.4f}"
    )
    print(f"head {summary.head_sha or '(none)'}")
    return 0 if summary.completed else 1


if __name__ == "__main__":
    sys.exit(main())
