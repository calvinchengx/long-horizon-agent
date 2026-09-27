"""The Python implementation against the language-neutral cases in ``spec/`` (Go runs the same)."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import httpx
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
from lha.contracts.memory import MemoryRecord
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
from lha.memory.embeddings import (
    VOYAGE_BATCH_CHARS,
    VOYAGE_BATCH_TEXTS,
    VOYAGE_DEFAULT_MODEL,
    HashEmbedder,
    VoyageEmbedder,
    parse_voyage_response,
    voyage_batches,
    voyage_status_message,
)
from lha.memory.hybrid import BM25Index, reciprocal_rank_fusion
from lha.memory.semantic_memory import cosine
from lha.memory.service import _render_episode, _terms
from lha.model import parse_fallback_entry
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
from lha.state.vendor import _target_path
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


def test_flaky_retry() -> None:
    from tests.unit.test_flaky_retry_verifier import SPEC_REVISION, run_flaky_scenario

    spec = _load("verify/flaky_retry.json")
    assert spec["revision"] == SPEC_REVISION and spec["cases"]
    for case in spec["cases"]:
        assert run_flaky_scenario(case) == case["expected"], case["name"]


def test_pricing() -> None:
    for case in _load("model/pricing.json")["claude"]:
        price = lookup_claude_price(case["model"])
        assert (price is not None) == case["priced"], case["model"]
        for cost in case["costs"]:
            assert price is not None
            assert price.cost(Usage(**cost["usage"])) == pytest.approx(cost["usd"], rel=1e-12)


def test_fallback_models() -> None:
    for case in _load("model/fallback_models.json")["cases"]:
        if "error" in case:
            with pytest.raises(ValueError) as exc:
                parse_fallback_entry(case["entry"])
            assert str(exc.value) == case["error"], case
            continue
        spec = parse_fallback_entry(case["entry"])
        assert (spec.backend, spec.model) == (case["backend"], case["model"]), case
        price = case["price"]
        assert (spec.price is None) == (price is None), case
        if spec.price is not None:
            assert spec.price.input_per_mtok == price["input_per_mtok"], case
            assert spec.price.output_per_mtok == price["output_per_mtok"], case


def test_vendor_paths() -> None:
    for case in _load("state/vendor_paths.json")["cases"]:
        assert str(_target_path(case["url"], case["content_type"])) == case["path"], case


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
            code_map_text=case["code_map_text"],
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
    from lha.agent.code_map import code_map_argv
    from lha.agent.prompt import CODE_MAP_HARD_CAP, CODE_MAP_HEADER, render_code_map

    code_map = spec["code_map"]
    assert (code_map["header"], code_map["hard_cap"]) == (CODE_MAP_HEADER, CODE_MAP_HARD_CAP)
    for case in code_map["argv"]:
        item = ChecklistItem.model_validate(case["item"])
        assert code_map_argv(item, case["budget"]) == case["argv"]
    for case in code_map["render"]:
        assert render_code_map(case["output"]) == case["rendered"]
    from lha.agent.code_map import (
        TRACE_CHARS,
        TRACE_SCRIPT,
        code_map_query,
        found_code,
        trace_argv,
        trace_env,
    )

    assert (code_map["trace_chars"], code_map["trace_script"]) == (TRACE_CHARS, TRACE_SCRIPT)
    assert code_map["trace_argv"] == trace_argv()
    for case in code_map["query"]:
        assert code_map_query(ChecklistItem.model_validate(case["item"])) == case["query"]
    for case in code_map["trace_env"]:
        item = ChecklistItem.model_validate(case["item"])
        assert trace_env(item, case["budget"]) == case["env"]
    for case in code_map["found_code"]:
        assert found_code(case["output"]) == case["found"]


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


def test_execution_sandbox_egress() -> None:
    from lha.config import Settings
    from lha.execution.egress_events import parse_proxy_log
    from lha.execution.egress_hosts import (
        PACKAGE_FETCH_HOSTS,
        WRITE_HOSTS,
        SandboxEgressError,
        sandbox_allow_list,
    )
    from lha.execution.tools.toolset import check_run_rule_of_two
    from lha.safety.rule_of_two import RuleOfTwoViolation

    spec = _load("execution/sandbox_egress.json")
    assert spec["package_fetch_hosts"] == list(PACKAGE_FETCH_HOSTS)
    assert spec["write_hosts"] == list(WRITE_HOSTS)
    for case in spec["allow_list"]:
        lists = (case["egress"], case["extra"], case["write"])
        if case["error"] is None:
            assert sandbox_allow_list(*lists) == case["hosts"], case
        else:
            with pytest.raises(SandboxEgressError) as info:
                sandbox_allow_list(*lists)
            assert str(info.value) == case["error"], case
    for case in spec["rule_of_two"]:
        settings = Settings(_env_file=None, **case["env"])  # type: ignore[call-arg]
        assert settings.sandbox_egress_enabled() == case["egress_enabled"], case
        if case["error"] is None:
            check_run_rule_of_two(settings)
        else:
            with pytest.raises(RuleOfTwoViolation) as refused:
                check_run_rule_of_two(settings)
            assert str(refused.value) == case["error"], case
    log = spec["proxy_log"]
    assert [e.payload for e in parse_proxy_log(log["lines"])] == log["events"]


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


def test_memory_embedder_and_cosine() -> None:
    spec = _load("memory/hash_embedder.json")
    for case in spec["hash"]:
        (vector,) = asyncio.run(HashEmbedder(dim=case["dim"]).embed([case["text"]]))
        assert vector == case["vector"], case["text"]  # byte-identical, not approximately equal
    for case in spec["cosine"]:
        assert cosine(case["a"], case["b"]) == case["cosine"], case


def test_memory_voyage() -> None:
    spec = _load("memory/voyage.json")
    constants = (spec["default_model"], spec["batch_texts"], spec["batch_chars"])
    assert constants == (VOYAGE_DEFAULT_MODEL, VOYAGE_BATCH_TEXTS, VOYAGE_BATCH_CHARS)
    for case in spec["batches"]:
        got = voyage_batches(
            case["texts"], max_texts=case["max_texts"], max_chars=case["max_chars"]
        )
        assert got == case["batches"], case
    for case in spec["requests"]:
        # What actually goes on the wire (a mocked API: no network).
        sent: list[Any] = []

        def api(request: httpx.Request, sent: list[Any] = sent) -> httpx.Response:
            body = json.loads(request.content)
            sent.append(body)
            data = [{"index": i, "embedding": [1.0, 0.0]} for i in range(len(body["input"]))]
            return httpx.Response(200, json={"data": data})

        async def run(case: dict[str, Any] = case) -> None:
            embedder = VoyageEmbedder(
                api_key="pa-test",
                model=case["model"],
                dim=2,
                resolver=_public_resolver,
                transport=httpx.MockTransport(api),
            )
            if case["input_type"] == "query":
                await embedder.embed_query(case["texts"])
            else:
                await embedder.embed(case["texts"])
            await embedder.aclose()

        asyncio.run(run())
        assert sent == case["bodies"], case["model"]
    for case in spec["responses"]:
        if "error" in case:
            with pytest.raises(ValueError) as caught:
                parse_voyage_response(case["response"], case["count"])
            assert str(caught.value) == case["error"], case
        else:
            assert parse_voyage_response(case["response"], case["count"]) == case["vectors"]
    for case in spec["statuses"]:
        assert voyage_status_message(case["model"], case["status"]) == case["message"]


async def _public_resolver(host: str, port: int) -> list[str]:
    return ["93.184.216.34"]


def test_memory_retrieval() -> None:
    spec = _load("memory/retrieval.json")
    bm25 = BM25Index()
    for doc_id, text in spec["bm25_docs"]:
        bm25.add([MemoryRecord(id=doc_id, kind="semantic", text=text)])
    for case in spec["bm25"]:
        got = bm25.query(case["query"], k=case["k"])
        assert [d for d, _ in got] == [d for d, _ in case["hits"]], case["query"]
        assert [s for _, s in got] == pytest.approx([s for _, s in case["hits"]], rel=1e-12)
    for case in spec["fusion"]:
        got = reciprocal_rank_fusion(case["rankings"])
        assert [[d, s] for d, s in got] == case["fused"], case["rankings"]


def test_memory_recall() -> None:
    from tests.unit.memory_spec_fixture import run_case

    spec = _load("memory/recall.json")
    for case in spec["episodes"]:
        assert _render_episode(case["payload"], case["cycle_id"]) == case["rendered"], case
    for case in spec["terms"]:
        assert _terms(case["text"]) == case["terms"], case
    for case in spec["cases"]:
        assert asyncio.run(run_case(case)) == case["blocks"], case["name"]


def test_wire_bytes(tmp_path: Path) -> None:
    """Raw bytes (never parsed JSON) of events, events.ndjson, ownership.json and gate bodies."""
    import httpx._content

    from lha.contracts.state import Checkpoint, EventRecord
    from lha.coordination.leases import LeaseDecision, lease_event
    from lha.durable.activities import gate_notice_payload
    from lha.durable.types import GateNotice, PendingApproval
    from lha.execution.egress_events import parse_proxy_log
    from lha.state.mission_anchor import GitMissionAnchor

    spec = _load("state/wire_bytes.json")
    events = [EventRecord.model_validate_json(line) for line in spec["events"]]
    assert [e.model_dump_json() for e in events] == spec["events"]

    async def write_anchor() -> str:
        anchor = GitMissionAnchor(str(tmp_path))
        items = Checklist(items=[ChecklistItem(id="01", description="one")])
        await anchor.initialize(title="Wire", description="bytes", items=items)
        await anchor.commit_checkpoint(
            Checkpoint(cycle_id="c1", progress_summary="- c1", checklist=items, events=events)
        )
        return (tmp_path / ".lha" / "events.ndjson").read_text(encoding="utf-8")

    assert asyncio.run(write_anchor()) == spec["anchor_events_file"]
    for case in spec["lease_events"]:
        line = lease_event(LeaseDecision(**case["decision"]), "c9").model_dump_json()
        assert line == case["line"]
    lines = _load("execution/sandbox_egress.json")["proxy_log"]["lines"]
    assert [e.model_dump_json() for e in parse_proxy_log(lines)] == spec["egress_events"]
    for case in spec["ownership"]:
        items = [ChecklistItem(id=i, description=f"item {i}") for i in case["items"]]
        ownership = assign_ownership(items, case["files"])
        for writer in case["release"]:
            ownership.release(writer)
        for path, writer in case["reassign"]:
            ownership.reassign(path, writer)
        assert ownership.model_dump_json(indent=2) == case["json"]
        back = FileOwnershipMap.model_validate_json(case["json"])
        assert back.model_dump_json(indent=2) == case["json"]
    for case in spec["gates"]:
        fields = dict(case["notice"])
        if fields.get("request") is not None:
            fields["request"] = PendingApproval(**fields["request"])
        notice = GateNotice(**fields)
        payload = gate_notice_payload(notice)
        _, stream = httpx._content.encode_json(payload)
        assert b"".join(stream).decode("utf-8") == case["body"]
        event = EventRecord(
            kind=f"gate_{notice.event}", cycle_id=f"gate:{notice.gate_id}", payload=payload
        )
        assert event.model_dump_json() == case["event"]


def test_system_one_wire() -> None:
    from lha.contracts.memory import MemoryRecord, RetrievalHit
    from lha.contracts.state import ChecklistItem
    from lha.contracts.system_one import ChoiceAnswer, SystemOneError
    from lha.memory.rerank import apply_relevance, system_one_rerank_request
    from lha.safety.egress import parse_url
    from lha.systemone import triage
    from lha.systemone.client import default_price_in_per_mtok, is_local_endpoint
    from lha.systemone.wire import (
        choice_confidence,
        parse_response,
        question_body,
        question_from_wire,
        request_body,
        score_confidence,
    )

    spec = _load("systemone/wire.json")
    for case in spec["requests"]:
        questions = {qid: question_from_wire(q) for qid, q in case["questions"].items()}
        if case["error"]:
            with pytest.raises(SystemOneError):
                request_body(case["model"], case["state"], questions)
        else:
            assert request_body(case["model"], case["state"], questions) == case["body"]
    asked = {qid: question_from_wire(q) for qid, q in spec["questions"].items()}
    for case in spec["responses"]:
        if case["error"]:
            with pytest.raises(SystemOneError):
                parse_response(case["response"], asked)
        else:
            assert parse_response(case["response"], asked).model_dump() == case["result"]
    for case in spec["choice_confidence"]:
        assert choice_confidence(case["probabilities"]) == case["confidence"]
    for case in spec["score_confidence"]:
        assert score_confidence(case["probabilities"]) == case["confidence"]
    assert spec["triage"]["question"] == question_body(triage.QUESTION)
    for case in spec["triage"]["actions"]:
        probs = {k: (0.9 if k == case["choice"] else 0.05) for k in triage.CAUSES}
        answer = ChoiceAnswer(
            choice=case["choice"], probabilities=probs, confidence=case["confidence"]
        )
        action = triage.triage_action(
            answer, threshold=case["threshold"], can_split=case["can_split"]
        )
        assert action == case["action"]
    for case in spec["triage"]["states"]:
        state = triage.triage_state(
            ChecklistItem(**case["item"]),
            case["latest"],
            case["previous"],
            max_chars=case["max_chars"],
        )
        assert state == case["state"]

    def hits(texts: list[str]) -> list[RetrievalHit]:
        return [
            RetrievalHit(record=MemoryRecord(id=f"r{i}", kind="semantic", text=t), score=0.1)
            for i, t in enumerate(texts)
        ]

    for case in spec["rerank"]["requests"]:
        state, questions = system_one_rerank_request(case["query"], hits(case["texts"]))
        assert state == case["state"]
        assert {qid: question_body(q) for qid, q in questions.items()} == case["questions"]
    for case in spec["rerank"]["apply"]:
        texts = [f"t{i}" for i in range(len(case["relevance"]))]
        ranked = apply_relevance(hits(texts), case["relevance"], k=case["k"], min_p=case["min_p"])
        assert [int(h.record.id[1:]) for h in ranked] == case["order"]
        assert [h.score for h in ranked] == case["scores"]
    for case in spec["endpoints"]:
        assert is_local_endpoint(parse_url(case["endpoint"])) == case["local"]
        assert default_price_in_per_mtok(case["endpoint"]) == case["default_price_in_per_mtok"]


def test_execution_code_query() -> None:
    from lha.execution.tools.code_query import (
        KINDS,
        MAX_ANSWER_CHARS,
        MAX_FIND_CHARS,
        MAX_SYMBOL_CHARS,
        CodeQueryTool,
        clip_answer,
        code_query_argv,
    )

    spec = _load("execution/code_query.json")
    assert spec["kinds"] == KINDS
    assert (spec["max_symbol_chars"], spec["max_find_chars"], spec["max_answer_chars"]) == (
        MAX_SYMBOL_CHARS,
        MAX_FIND_CHARS,
        MAX_ANSWER_CHARS,
    )
    assert spec["spec"] == CodeQueryTool.spec.model_dump(mode="json")
    for case in spec["questions"]:
        if case["error"] is None:
            assert code_query_argv(case["kind"], case["target"], case["budget"]) == case["argv"]
        else:
            with pytest.raises(ValueError) as caught:
                code_query_argv(case["kind"], case["target"], case["budget"])
            assert str(caught.value) == case["error"]
    for case in spec["clip"]:
        assert clip_answer(case["text"]) == case["clipped"]
