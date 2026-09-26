"""The Python implementation against the language-neutral cases in ``spec/`` (Go runs the same)."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest

from lha.agent.loop import parse_action
from lha.agent.prompt import build_messages, corrective_message, render_memory_block, render_tools
from lha.agents.planner import Planner, assign_ownership, parse_plan
from lha.agents.reflection import reflect_on_failure
from lha.agents.replanner import Replanner
from lha.agents.reviewer import Reviewer, ReviewResult, parse_review
from lha.agents.roles import ROLES, claude_model_for
from lha.agents.subagent import SubAgent
from lha.agents.waves import implementer_objective, new_implementer_run
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.contracts.tools import ToolResult, ToolSpec
from lha.contracts.verify import checks_from_commands, derive_check_name
from lha.coordination.decision_log import _canonical, _chain_hash, parse_chain, verify_chain
from lha.coordination.enforcement import OwnershipGuard
from lha.coordination.leases import decide_lease
from lha.coordination.ownership import FileOwnershipMap, LeaseRequest, is_shared
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus
from lha.execution.dispatcher import _missing_required, validate_arguments
from lha.execution.paths import PathEscapeError, contained_posix, is_protected, normalize_relpath
from lha.model.pricing import lookup_claude_price
from lha.model.stub import StubModel
from lha.obs.redact import is_secret_key, redact_text
from lha.safety.commands import classify_command
from lha.safety.egress import (
    EgressDenied,
    EgressPolicy,
    is_public_address,
    normalize_host,
    parse_url,
)
from lha.verify.harness_integrity import _is_harness_file

SPEC = Path(__file__).resolve().parents[3] / "spec"


def _load(rel: str) -> Any:
    return json.loads((SPEC / rel).read_text(encoding="utf-8"))


def test_classify_command() -> None:
    for case in _load("safety/classify_command.json")["cases"]:
        assert classify_command(case["argv"]) == case["reason"], case["argv"]


def test_egress() -> None:
    spec = _load("safety/egress.json")
    for case in spec["normalize_host"]:
        assert normalize_host(case["host"]) == case["normalized"], case
    for case in spec["parse_url"]:
        if case["ok"]:
            parsed = parse_url(case["url"])
            assert (parsed.scheme, parsed.host, parsed.port) == (
                case["scheme"],
                case["host"],
                case["port"],
            )
        else:
            with pytest.raises(EgressDenied):
                parse_url(case["url"])
    for case in spec["is_public_address"]:
        assert is_public_address(case["address"]) == case["public"], case
    for case in spec["permits"]:
        policy = EgressPolicy(allow_hosts=set(case["allow_hosts"]))
        assert policy.permits(case["url"]) == case["permitted"], case


def test_redact() -> None:
    spec = _load("obs/redact.json")
    for case in spec["redact_text"]:
        assert redact_text(case["text"]) == case["redacted"], case
    for case in spec["is_secret_key"]:
        assert is_secret_key(case["key"]) == case["secret"], case


def test_check_names() -> None:
    spec = _load("contracts/check_names.json")
    for case in spec["derive_check_name"]:
        assert derive_check_name(case["command"]) == case["name"], case
    for case in spec["checks_from_commands"]:
        assert [c.name for c in checks_from_commands(case["commands"])] == case["names"]


def test_checklist() -> None:
    spec = _load("state/checklist.json")
    for case in spec["scenarios"]:
        checklist = Checklist.model_validate(case["checklist"])
        nxt = checklist.next_actionable()
        assert (nxt.id if nxt else None) == case["next_actionable"], case["name"]
        assert checklist.is_complete == case["is_complete"], case["name"]
        assert checklist.is_deadlocked == case["is_deadlocked"], case["name"]
        assert checklist.deadlock_reason() == case["deadlock_reason"], case["name"]
        assert checklist.dependency_errors() == case["dependency_errors"], case["name"]
        assert checklist.items_done == case["items_done"], case["name"]
    checklist = Checklist.model_validate(spec["transitions"]["initial"])
    for step in spec["transitions"]["steps"]:
        args = step["args"]
        if step["op"] == "start":
            checklist.start(args["item_id"])
        elif step["op"] == "record_failure":
            checklist.record_failure(
                args["item_id"], args["reason"], max_consecutive_failures=args["max"]
            )
        elif step["op"] == "unblock":
            checklist.unblock(args["item_id"])
        else:
            checklist.record_success(args["item_id"], args["verified_by"])
        assert json.loads(checklist.model_dump_json()) == step["after"], step["op"]
    split = spec["split"]
    checklist = Checklist.model_validate(split["initial"])
    checklist.split(
        split["item_id"],
        [
            ChecklistItem(id=f"d{n}", description=d["description"], witnesses=d["witnesses"])
            for n, d in enumerate(split["drafts"])
        ],
    )
    assert json.loads(checklist.model_dump_json()) == split["after"]
    assert (checklist.next_actionable() or ChecklistItem(id="", description="")).id == split[
        "next_actionable"
    ]
    assert checklist.items_total == split["items_total"]
    assert checklist.is_complete == split["is_complete"]


def test_decision_chain() -> None:
    spec = _load("coordination/decision_chain.json")
    prev = spec["genesis"]
    for link in spec["chain"]:
        assert _canonical(link["record"]) == link["canonical"]
        assert link["prev"] == prev
        assert _chain_hash(prev, link["record"]) == link["hash"]
        prev = link["hash"]

    # The .lha/decisions.ndjson line format: a legacy prefix folded into the chain, then envelopes.
    log = spec["log"]
    data = "".join(f"{line}\n" for line in log["lines"]).encode()
    assert parse_chain(data).last_hash == log["last_hash"]
    prefix = b""
    for line, link in zip(log["lines"], log["links"], strict=True):
        prefix += f"{line}\n".encode()
        assert parse_chain(prefix).last_hash == link["running_hash"], link
        assert ("prev" in json.loads(line)) == (link["kind"] == "chained")
    for case in log["verify"]:
        check = verify_chain("".join(f"{ln}\n" for ln in case["lines"]).encode())
        assert (check.ok, check.checked, check.legacy, check.problem) == (
            case["ok"],
            case["checked"],
            case["legacy"],
            case["problem"],
        ), case["name"]


def test_shared_paths_and_harness_files() -> None:
    for case in _load("coordination/shared_paths.json")["cases"]:
        assert is_shared(case["path"]) == case["shared"], case
    for case in _load("verify/harness_files.json")["cases"]:
        assert _is_harness_file(case["path"]) == case["harness"], case


def test_pricing() -> None:
    for case in _load("model/pricing.json")["claude"]:
        price = lookup_claude_price(case["model"])
        assert (price is not None) == case["priced"], case["model"]
        for cost in case["costs"]:
            assert price is not None
            assert price.cost(Usage(**cost["usage"])) == pytest.approx(cost["usd"], rel=1e-12)


class _Recording(StubModel):
    """Records the messages of every call (and answers with an empty plan)."""

    def __init__(self) -> None:
        super().__init__()
        self.seen: list[dict[str, str]] = []

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.seen.extend({"role": m.role, "content": m.content} for m in messages)
        return TurnResult(text="[]")


def test_agent_prompts() -> None:
    spec = _load("agent/prompts.json")
    for case in spec["build_messages"]:
        messages = build_messages(
            anchor_text=case["anchor_text"],
            mission_text=case["mission_text"],
            snapshot=SituationSnapshot.model_validate(case["snapshot"]),
            item=ChecklistItem.model_validate(case["item"]),
            specs=[ToolSpec.model_validate(s) for s in case["specs"]],
            memory_text=case["memory_text"],
            engine=case["engine"],
        )
        got = [{"role": m.role, "content": m.content} for m in messages]
        assert got == case["messages"], case["name"]
    lead = spec["lead_tools"]
    assert render_tools([ToolSpec.model_validate(s) for s in lead["specs"]]) == lead["rendered"]
    for case in spec["render_memory_block"]:
        sections = [(s["title"], s["lines"]) for s in case["sections"]]
        weights = tuple(case["weights"]) if case["weights"] is not None else None
        rendered = render_memory_block(sections, budget_chars=case["budget_chars"], weights=weights)
        assert rendered == case["rendered"]
    assert corrective_message("no JSON object found in reply").content == spec["corrective"]


def test_agent_reply_protocol_and_planning() -> None:
    spec = _load("agent/prompts.json")
    for case in spec["parse_action"]:
        action = parse_action(case["text"], [], stop_reason=case["stop_reason"])
        got = [action.done, action.tool, action.arguments, action.summary, action.error]
        assert got == [case[k] for k in ("done", "tool", "arguments", "summary", "error")]
    for case in spec["parse_plan"]:
        items, files = parse_plan(case["text"])
        owners = assign_ownership(items, files).owners
        assert [i.model_dump(mode="json") for i in items] == case["items"], case["text"]
        assert (files, owners) == (case["files"], case["owners"]), case["text"]

    model = _Recording()
    asyncio.run(Planner(model).plan_mission(title="T", description="Build the thing."))
    assert model.seen == spec["planner_messages"]
    model = _Recording()
    asyncio.run(
        Planner(model).plan_mission(
            title="T", description="Build the thing.", acceptance="all tests pass"
        )
    )
    assert model.seen == spec["planner_messages_acceptance"]
    replan = spec["replanner"]
    model = _Recording()
    item = ChecklistItem.model_validate(replan["item"])
    asyncio.run(Replanner(model).split(mission_text=replan["mission_text"], item=item))
    assert model.seen == replan["messages"]


def test_execution_paths() -> None:
    spec = _load("execution/paths.json")
    for case in spec["normalize_relpath"]:
        if case["error"] is None:
            assert str(normalize_relpath(case["path"])) == case["normalized"], case
        else:
            with pytest.raises(PathEscapeError) as info:
                normalize_relpath(case["path"])
            assert str(info.value) == case["error"], case
    for case in spec["is_protected"]:
        if case["error"] is None:
            assert is_protected(case["path"]) == case["protected"], case
        else:
            with pytest.raises(PathEscapeError) as info:
                is_protected(case["path"])
            assert str(info.value) == case["error"], case
    for case in spec["contained_posix"]:
        if case["error"] is None:
            assert contained_posix(case["workdir"], case["path"]) == case["result"], case
        else:
            with pytest.raises(PathEscapeError) as info:
                contained_posix(case["workdir"], case["path"])
            assert str(info.value) == case["error"], case


def test_execution_arguments() -> None:
    spec = _load("execution/arguments.json")
    for case in spec["validate"]:
        got = validate_arguments(case["schema"], case["value"], case["where"])
        assert got == case["error"], case
    for case in spec["missing_required"]:
        missing = _missing_required({"required": case["required"]}, case["arguments"])
        assert sorted(missing) == case["missing"], case


class _SpecsOnly:
    """A dispatcher that only lists specs (no tool is ever called)."""

    def __init__(self, specs: list[ToolSpec]) -> None:
        self._specs = specs

    def specs(self) -> list[ToolSpec]:
        return list(self._specs)

    async def dispatch(self, call: object, ctx: object) -> ToolResult:  # pragma: no cover
        raise AssertionError("no tool call expected")


def test_agent_org() -> None:
    spec = _load("agent/org.json")
    for name, role in spec["roles"].items():
        got = ROLES[name]
        assert got.tier.value == role["tier"] and claude_model_for(got.tier) == role["claude_model"]
        assert got.system_prompt == role["system_prompt"], name
        assert (got.allow_mutating, got.allow_egress, got.max_turns) == (
            role["allow_mutating"],
            role["allow_egress"],
            role["max_turns"],
        )
    assert set(spec["roles"]) == set(ROLES)
    specs = [ToolSpec.model_validate(s) for s in spec["specs"]]
    assert render_tools(specs) == spec["rendered_specs"]
    for case in spec["subagent"]:
        model = _Recording()
        agent = SubAgent(role=ROLES[case["role"]], model=model, dispatcher=_SpecsOnly(specs))
        assert [s.name for s in agent.visible_specs()] == case["visible"]
        asyncio.run(
            agent.run(objective=case["objective"], ctx=None, extra_context=case["extra_context"])  # type: ignore[arg-type]
        )
        assert model.seen == case["messages"], case["role"]
    for case in spec["reviewer_messages"]:
        model = _Recording()
        asyncio.run(
            Reviewer(model, _SpecsOnly(specs)).review(
                diff=case["diff"],
                criteria=case["criteria"],
                ctx=None,  # type: ignore[arg-type]
            )
        )
        assert model.seen == case["messages"]
    for case in spec["parse_review"]:
        verdict, blocking, issues, advisory = parse_review(case["text"])
        assert [verdict, blocking, issues, advisory] == [
            case[k] for k in ("verdict", "blocking", "blocking_issues", "advisory")
        ], case["text"]
        review = ReviewResult(
            brief=case["text"],
            blocking=blocking,
            tool_calls=0,
            verdict=verdict,
            blocking_issues=issues,
            advisory=advisory,
        )
        assert review.notes() == case["notes"]
    reflection = spec["reflection"]
    model = _Recording()
    asyncio.run(
        reflect_on_failure(
            model=model,
            item_description=reflection["item_description"],
            failure_summary=reflection["failure_summary"],
        )
    )
    assert model.seen == reflection["messages"]
    for case in spec["implementer_objective"]:
        owners = FileOwnershipMap(owners=case["owners"])
        run = new_implementer_run(
            ChecklistItem.model_validate(case["item"]),
            case["cycle_id"],
            owners,
            tool_budget=12,
            acceptance=case["acceptance"],
        )
        assert (run.ticket.id, run.ticket.contract.write_set) == (
            case["ticket_id"],
            case["write_set"],
        )
        objective, extra = implementer_objective(run, **case["inputs"])
        assert (objective, extra) == (case["objective"], case["extra"]), case["name"]
    for case in spec["ownership_guard"]:
        guard = OwnershipGuard(
            _SpecsOnly([]),
            FileOwnershipMap(owners=case["owners"]),
            writers=case["writers"],
            lease_tool=case["lease_tool"],
        )
        assert guard.refusal(case["path"]) == case["refusal"], case
    for case in spec["leases"]:
        request = LeaseRequest(writer=case["writer"], path=case["path"], reason=case["reason"])
        decision = decide_lease(
            FileOwnershipMap(owners=case["owners"]), request, finished=case["finished"]
        )
        assert decision.model_dump() == case["decision"], case["path"]
        assert decision.message() == case["message"]
    for case in spec["tickets"]:
        ticket = Ticket(
            id="t", contract=TaskContract(objective="o"), status=TicketStatus(case["from"])
        )
        assert ticket.can_transition(TicketStatus(case["to"])) == case["legal"], case
