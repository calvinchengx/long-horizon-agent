"""Python side of the Go CLI's scripted `lha orchestrate` parity test (go/cmd/lha/orchestrate_test.go).

Reads a JSON spec from argv[1] ({"workdir", "title", "task", "checks", "plan"}) and does what
`lha orchestrate --task` does — the Planner (scripted with ``plan``) then the full organization —
with offline fakes for every role: implementers that record a decision and write their write-set,
a Lead / Researchers that say done, and a Reviewer that approves. Settings come from the
environment (LHA_SANDBOX=local, LHA_ALLOW_UNSAFE_LOCAL=true). Prints the CLI's three-line report;
exit code 0 when complete, else 1 — like the CLI.
"""

import asyncio
import json
import re
import sys

from lha.agent.runner import build_meter
from lha.agents.orchestrator import Orchestrator
from lha.agents.planner import Planner
from lha.config import get_settings
from lha.contracts.model import ModelMessage, TurnResult
from lha.contracts.verify import checks_from_commands
from lha.model.stub import StubModel

_DONE = TurnResult(text='{"done": true, "summary": "done"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
_WRITE_SET = re.compile(r"Your write-set \(the ONLY files you may create or modify\): (.*)")
_ITEM = re.compile(r"Checklist item \[([\w.]+)\]")


class _Implementers(StubModel):
    """Records a decision, writes every file of its write-set, then says done."""

    def __init__(self) -> None:
        super().__init__(model_name="implementers")

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        objective = messages[1].content
        item = _ITEM.search(objective)
        write_set = _WRITE_SET.search(objective)
        assert item is not None and write_set is not None
        actions: list[dict[str, object]] = [
            {
                "tool": "record_decision",
                "arguments": {"decision": f"item {item.group(1)} layout", "rationale": "r"},
            },
            *(
                {"tool": "write_file", "arguments": {"path": p.strip(), "content": "x\n"}}
                for p in write_set.group(1).split(",")
                if p.strip()
            ),
        ]
        turn = sum(1 for m in messages if m.role == "assistant")
        if turn < len(actions):
            return TurnResult(text=json.dumps(actions[turn]))
        return _DONE


async def _mission(spec: dict[str, object]) -> object:
    settings = get_settings()
    meter = build_meter(settings)
    planner = Planner(meter.wrap(StubModel(script=[TurnResult(text=str(spec["plan"]))]), role="planner"))
    plan = await planner.plan_mission(title=str(spec["title"]), description=str(spec["task"]))
    models = {
        "implementer": _Implementers(),
        "lead": StubModel(script=[_DONE]),
        "researcher": StubModel(script=[_DONE]),
        "reviewer": StubModel(script=[_APPROVE]),
    }
    return await Orchestrator(settings, meter=meter, models=models).run_mission(  # type: ignore[arg-type]
        workdir=str(spec["workdir"]),
        title=str(spec["title"]),
        description=str(spec["task"]),
        checklist=plan.checklist,
        checks=checks_from_commands(spec["checks"]),  # type: ignore[arg-type]
        ownership=plan.ownership,
    )


def main() -> int:
    with open(sys.argv[1], encoding="utf-8") as fh:
        spec = json.load(fh)
    summary = asyncio.run(_mission(spec))
    print(f"mission {summary.mission_id}: {summary.stopped_reason}")  # type: ignore[attr-defined]
    print(
        f"items {summary.items_done}/{summary.items_total}  "  # type: ignore[attr-defined]
        f"cycles {summary.cycles}  cost ${summary.total_usd:.4f}"  # type: ignore[attr-defined]
    )
    print(f"head {summary.head_sha or '(none)'}")  # type: ignore[attr-defined]
    return 0 if summary.completed else 1  # type: ignore[attr-defined]


if __name__ == "__main__":
    sys.exit(main())
