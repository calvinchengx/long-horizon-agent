"""System One decision models: the wire format, the metered client, stall triage, reranking.

The loop tests run through the REAL loop, verifier, sandbox (local) and git anchor with a scripted
lead model and a stub System One model, so they assert what the harness actually does with an
answer: a confident answer may stop work on an item sooner, and nothing else changes.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import httpx
import pytest

from lha.agent.assembly import build_lead_loop
from lha.agent.loop import AgentLoop
from lha.agents.replanner import Replanner
from lha.config import Settings
from lha.contracts.memory import MemoryRecord, RetrievalHit
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.system_one import (
    Choice,
    ChoiceAnswer,
    Noul,
    NoulAnswer,
    Question,
    Score,
    SystemOneError,
)
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import CostMeter
from lha.memory.rerank import SystemOneReranker, apply_relevance, system_one_rerank_request
from lha.model.stub import StubModel
from lha.state.mission_anchor import GitMissionAnchor
from lha.systemone import StallTriage, StubSystemOne, SystemOneClient, build_system_one
from lha.systemone.build import build_stall_triage
from lha.systemone.client import TYPESAFE_ENDPOINT, default_price_in_per_mtok
from lha.systemone.stub import choice_answer
from lha.systemone.triage import QUESTION_ID, triage_action, triage_state
from lha.systemone.wire import (
    choice_confidence,
    parse_response,
    request_body,
    score_confidence,
)
from lha.verify.verifier import DeterministicVerifier

PY = sys.executable
PASS = Check(name="always", command=[PY, "-c", "pass"])
DONE = TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn")
KEY = "ts-secret-key-0123456789"

QUESTIONS: dict[str, Question] = {
    "urgent": Noul(instructions="Is it urgent?"),
    "team": Choice(instructions="Which team?", criteria={"billing": "Payments", "shipping": None}),
    "mood": Score(instructions="How upset?", criteria=["calm", "upset", "angry"]),
}
ANSWERS = {
    "urgent": {"type": "noul", "noul": 0.93},
    "team": {
        "type": "choice",
        "choice": "billing",
        "probabilities": {"billing": 0.88, "shipping": 0.12},
        "confidence": 0.76,
    },
    "mood": {
        "type": "score",
        "score": 1.44,
        "legend": {"0": "calm", "1": "upset", "2": "angry"},
        "probabilities": {"0": 0.0, "1": 0.56, "2": 0.44},
        "confidence": 0.34,
    },
}


def _settings(**overrides: object) -> Settings:
    return Settings(_env_file=None, **overrides)  # type: ignore[call-arg]


def _meter(ceiling: float = 10.0) -> CostMeter:
    return CostMeter(
        ledger=CostLedger(), governor=BudgetGovernor(ceiling_usd=ceiling, max_cycles=100)
    )


# --- wire format ------------------------------------------------------------------------------


def test_request_body_matches_the_api() -> None:
    body = request_body("jev-1.13.0", {"ticket": "charged twice"}, QUESTIONS)
    assert body == {
        "model": "jev-1.13.0",
        "state": {"ticket": "charged twice"},
        "questions": {
            "urgent": {"type": "noul", "instructions": "Is it urgent?"},  # no criteria: omitted
            "team": {
                "type": "choice",
                "instructions": "Which team?",
                "criteria": {"billing": "Payments", "shipping": None},
            },
            "mood": {
                "type": "score",
                "instructions": "How upset?",
                "criteria": ["calm", "upset", "angry"],
            },
        },
    }


@pytest.mark.parametrize(
    "questions",
    [
        {},
        {"q": Choice(instructions="x", criteria={})},
        {"q": Choice(instructions="x", criteria={str(i): None for i in range(256)})},
        {"q": Score(instructions="x", criteria=["only one"])},
        {"q": Score(instructions="x", criteria=[str(i) for i in range(11)])},
    ],
)
def test_requests_the_api_would_refuse_are_refused_first(questions: dict[str, Question]) -> None:
    with pytest.raises(SystemOneError):
        request_body("m", "s", questions)


def test_parse_response_reads_every_answer_type() -> None:
    result = parse_response(
        {"model": "jev-1.13.0", "answers": ANSWERS, "usage": {"input_tokens": 210}}, QUESTIONS
    )
    assert result.model == "jev-1.13.0" and result.input_tokens == 210
    assert result.answers["urgent"] == NoulAnswer(noul=0.93)
    team = result.answers["team"]
    assert isinstance(team, ChoiceAnswer) and team.choice == "billing"
    assert result.answers["mood"].probabilities == {"0": 0.0, "1": 0.56, "2": 0.44}


@pytest.mark.parametrize(
    ("qid", "bad"),
    [
        ("urgent", {"type": "choice", "noul": 0.5}),  # wrong type
        ("urgent", {"type": "noul", "noul": 1.5}),  # outside [0, 1]
        ("urgent", {"type": "noul", "noul": True}),  # a bool is not a number
        ("urgent", {"type": "noul", "noul": float("nan")}),
        ("team", {**ANSWERS["team"], "choice": "returns"}),  # not an option
        ("team", {**ANSWERS["team"], "probabilities": {"billing": 1.0}}),  # missing option
        ("team", {**ANSWERS["team"], "probabilities": {"billing": 0.5, "shipping": 0.2}}),
        ("mood", {**ANSWERS["mood"], "score": 2.5}),  # above the top level
        ("mood", {**ANSWERS["mood"], "confidence": -0.1}),
    ],
)
def test_parse_response_refuses_malformed_answers(qid: str, bad: dict[str, object]) -> None:
    with pytest.raises(SystemOneError):
        parse_response({"answers": {**ANSWERS, qid: bad}}, QUESTIONS)


def test_parse_response_refuses_a_missing_answer() -> None:
    with pytest.raises(SystemOneError, match="missing"):
        parse_response({"answers": {"urgent": ANSWERS["urgent"]}}, QUESTIONS)


def test_confidence_formulas_match_the_published_ones() -> None:
    # Kev's README example response (TypeSafe's reference adapter formulas; Kev computed them
    # from unrounded probabilities, hence the tolerance).
    assert choice_confidence([0.47, 0.28, 0.25]) == pytest.approx(0.21, abs=0.01)
    assert score_confidence([0.0, 0.56, 0.44]) == pytest.approx(0.34, abs=0.01)
    assert choice_confidence([1.0, 0.0]) == 1.0 and choice_confidence([0.5, 0.5]) == 0.0
    assert score_confidence([1 / 3] * 3) == 0.0 and score_confidence([0, 0, 1.0]) == 1.0


# --- client -----------------------------------------------------------------------------------


def _api(seen: list[httpx.Request], status: int = 200, body: object | None = None):  # type: ignore[no-untyped-def]
    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        payload = (
            body
            if body is not None
            else {
                "model": "jev-1.13.0",
                "answers": ANSWERS,
                "usage": {"input_tokens": 1_000_000, "output_tokens": 40},
            }
        )
        return httpx.Response(status, json=payload)

    return httpx.MockTransport(handler)


async def test_client_posts_with_the_key_and_meters_input_tokens() -> None:
    seen: list[httpx.Request] = []
    meter = _meter()
    client = SystemOneClient(
        model="jev-1.13.0",
        api_key=KEY,
        price_in_per_mtok=0.042,
        meter=meter,
        transport=_api(seen),
    )
    result = await client.evaluate({"ticket": "x"}, QUESTIONS)
    assert result.answers["urgent"] == NoulAnswer(noul=0.93)
    (request,) = seen
    assert str(request.url) == TYPESAFE_ENDPOINT
    assert request.headers["authorization"] == f"Bearer {KEY}"
    assert json.loads(request.content)["model"] == "jev-1.13.0"
    (entry,) = meter.ledger.entries
    assert entry.role == "system_one" and entry.cost_known
    assert entry.usd == pytest.approx(0.042)  # a million input tokens; output is free
    assert KEY not in repr(client)
    await client.aclose()


async def test_client_errors_are_system_one_errors_without_the_key() -> None:
    client = SystemOneClient(
        model="m", api_key=KEY, price_in_per_mtok=0.0, transport=_api([], status=401)
    )
    with pytest.raises(SystemOneError) as caught:
        await client.evaluate("s", QUESTIONS)
    assert "401" in str(caught.value) and KEY not in str(caught.value)


async def test_client_retries_an_overloaded_server_once() -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            return httpx.Response(529, json={})
        return httpx.Response(200, json={"answers": ANSWERS})

    async def no_sleep(_s: float) -> None:
        return None

    client = SystemOneClient(
        model="m",
        api_key=KEY,
        price_in_per_mtok=0.0,
        transport=httpx.MockTransport(handler),
        sleep=no_sleep,
    )
    await client.evaluate("s", QUESTIONS)
    assert calls == 2


async def test_client_refused_by_the_budget_is_a_system_one_error() -> None:
    client = SystemOneClient(
        model="m", api_key=KEY, price_in_per_mtok=1e6, meter=_meter(0.001), transport=_api([])
    )
    with pytest.raises(SystemOneError, match="refused"):
        await client.evaluate("a long enough state", QUESTIONS)


async def test_remote_endpoint_must_be_public_https() -> None:
    from lha.safety.egress import EgressDenied

    with pytest.raises(EgressDenied, match="https"):
        SystemOneClient(model="m", endpoint="http://decisions.example.com/v1/systemone")

    async def private(host: str, port: int) -> list[str]:
        return ["10.0.0.7"]

    client = SystemOneClient(
        model="m", endpoint="https://decisions.example.com/v1/systemone", resolver=private
    )
    with pytest.raises(SystemOneError, match="resolves to a non-public address"):
        await client.evaluate("s", QUESTIONS)


async def test_loopback_endpoint_may_use_http_without_a_key() -> None:
    seen: list[httpx.Request] = []
    client = SystemOneClient(
        model="kev-latest", endpoint="http://127.0.0.1:8009/v1/systemone", transport=_api(seen)
    )
    await client.evaluate("s", QUESTIONS)
    assert "authorization" not in seen[0].headers


def test_default_prices() -> None:
    assert default_price_in_per_mtok(TYPESAFE_ENDPOINT) == 0.042
    assert default_price_in_per_mtok("http://localhost:8009/v1/systemone") == 0.0
    assert default_price_in_per_mtok("https://kev.example.com/v1/systemone") is None


# --- build ------------------------------------------------------------------------------------


def test_off_by_default_and_stub_for_tests() -> None:
    assert build_system_one(_settings(), None) is None
    assert isinstance(build_system_one(_settings(system_one_backend="stub"), None), StubSystemOne)


@pytest.mark.parametrize(
    ("overrides", "match"),
    [
        ({}, "API_KEY"),  # the hosted endpoint needs a key
        (
            {"system_one_api_key": KEY, "private_data": True},
            "PRIVATE_DATA",
        ),
        (
            {"system_one_api_key": KEY, "system_one_endpoint": "https://kev.example.com/v1/x"},
            "PRICE",
        ),
        ({"system_one_endpoint": "ftp://x/"}, "not usable"),
        ({"system_one_endpoint": "http://kev.example.com/v1/systemone"}, "not usable"),
    ],
)
def test_unsafe_configurations_are_refused(overrides: dict[str, object], match: str) -> None:
    with pytest.raises(ValueError, match=match):
        build_system_one(_settings(system_one_backend="systemone", **overrides), None)


async def test_usable_configurations_build() -> None:
    local = build_system_one(
        _settings(
            system_one_backend="systemone",
            system_one_endpoint="http://127.0.0.1:8009/v1/systemone",
            private_data=True,  # loopback never leaves the machine
        ),
        None,
    )
    hosted = build_system_one(
        _settings(
            system_one_backend="systemone",
            system_one_api_key=KEY,
            private_data=True,
            system_one_private_data_ok=True,
        ),
        _meter(),
    )
    assert isinstance(local, SystemOneClient) and isinstance(hosted, SystemOneClient)
    await local.aclose()
    await hosted.aclose()


# --- stall triage -----------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("probs", "can_split", "action"),
    [
        ({"defect": 0.02, "scope": 0.96, "environment": 0.02}, True, "split"),
        ({"defect": 0.02, "scope": 0.96, "environment": 0.02}, False, "continue"),
        ({"defect": 0.02, "scope": 0.02, "environment": 0.96}, True, "block"),
        ({"defect": 0.96, "scope": 0.02, "environment": 0.02}, True, "continue"),
        ({"defect": 0.2, "scope": 0.7, "environment": 0.1}, True, "continue"),  # not confident
    ],
)
def test_triage_acts_only_on_a_confident_scope_or_environment(
    probs: dict[str, float], can_split: bool, action: str
) -> None:
    answer = choice_answer(probs)
    assert triage_action(answer, threshold=0.9, can_split=can_split) == action


def test_triage_state_is_redacted_and_bounded() -> None:
    item = ChecklistItem(id="01", description="call the API", witnesses=["go:TestX"])
    latest = "x" * 5000 + " token sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA end"
    state = triage_state(item, latest, "", max_chars=100)
    assert state["witnesses"] == ["go:TestX"]
    assert isinstance(state["latest_failure"], str) and len(state["latest_failure"]) <= 103
    assert "sk-ant-api03" not in state["latest_failure"]


def _items() -> list[ChecklistItem]:
    return [
        ChecklistItem(id="01", description="coarse", witnesses=["cmd:exit 1"]),
        ChecklistItem(id="02", description="after", depends_on=["01"]),
    ]


def _cause(probs: dict[str, float]) -> StubSystemOne:
    return StubSystemOne(lambda state, questions: {QUESTION_ID: choice_answer(probs)})


async def _run(
    tmp_path: Path,
    lead: list[TurnResult],
    triage: StallTriage | None,
    *,
    replanner_script: list[TurnResult] | None = None,
) -> tuple[AgentLoop, GitMissionAnchor, list[object]]:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(title="T", description="D", items=Checklist(items=_items()))
    session = await LocalSandbox().open(workdir=str(tmp_path))
    ctx = ToolContext(mission_id="m", session=session)
    model = StubModel(script=lead)
    loop = AgentLoop(
        model=model,
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        verifier=DeterministicVerifier(default_timeout_s=60),
        anchor=anchor,
        max_turns=2,
        max_consecutive_failures=5,
        replanner=Replanner(StubModel(script=replanner_script or [])),
        max_replans=5,
        triage=triage,
    )
    outcomes = [
        await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id=f"c{n}", checks=[PASS])
        for n in (1, 2)
    ]
    return loop, anchor, outcomes


def _events(tmp_path: Path, kind: str) -> list[dict[str, object]]:
    lines = (tmp_path / ".lha" / "events.ndjson").read_text().splitlines()
    return [e for e in map(json.loads, lines) if e.get("kind") == kind]


async def test_confident_scope_splits_the_item_before_the_failure_limit(tmp_path: Path) -> None:
    stub = _cause({"defect": 0.01, "scope": 0.98, "environment": 0.01})
    plan = TurnResult(text='[{"description": "part A"}, {"description": "part B"}]')
    _loop, anchor, outcomes = await _run(
        tmp_path, [DONE, DONE], StallTriage(stub), replanner_script=[plan]
    )
    first, second = outcomes
    assert not first.item_split and len(stub.calls) == 1  # asked only after 2 failures in a row
    assert second.item_split  # at 2 failures, not 5
    checklist = await anchor.read_checklist()
    assert [(i.id, i.status) for i in checklist.items] == [
        ("01", "split"),
        ("02", "todo"),
        ("01.1", "todo"),
        ("01.2", "todo"),
    ]
    assert checklist.get("01.2").witnesses == ["cmd:exit 1"]  # type: ignore[union-attr]
    (event,) = _events(tmp_path, "system_one")
    payload = event["payload"]
    assert isinstance(payload, dict)
    assert payload["use"] == "stall_triage" and payload["action"] == "split"
    assert payload["answer"] == "scope" and payload["model"] == "stub-1"


async def test_confident_environment_blocks_the_item_without_splitting(tmp_path: Path) -> None:
    stub = _cause({"defect": 0.01, "scope": 0.01, "environment": 0.98})
    _loop, anchor, outcomes = await _run(tmp_path, [DONE, DONE], StallTriage(stub))
    assert outcomes[1].item_blocked and not outcomes[1].item_split
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and item.consecutive_failures == 2
    assert item.last_failure.startswith("Blocked before the failure limit")


@pytest.mark.parametrize(
    "stub",
    [
        _cause({"defect": 0.98, "scope": 0.01, "environment": 0.01}),  # a plain defect
        _cause({"defect": 0.3, "scope": 0.4, "environment": 0.3}),  # not confident
        StubSystemOne(error="system one call failed: timeout"),  # unavailable
    ],
)
async def test_other_answers_and_failures_change_nothing(
    tmp_path: Path, stub: StubSystemOne
) -> None:
    _loop, anchor, outcomes = await _run(tmp_path, [DONE, DONE], StallTriage(stub))
    assert not outcomes[1].item_blocked and not outcomes[1].item_split
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and item.status == "in_progress"
    (event,) = _events(tmp_path, "system_one")
    assert event["payload"]["action"] == "continue"  # type: ignore[index]


async def test_a_split_the_replanner_cannot_make_keeps_the_item_going(tmp_path: Path) -> None:
    stub = _cause({"defect": 0.01, "scope": 0.98, "environment": 0.01})
    _loop, anchor, outcomes = await _run(
        tmp_path, [DONE, DONE], StallTriage(stub), replanner_script=[TurnResult(text="no idea")]
    )
    assert not outcomes[1].item_blocked and not outcomes[1].item_split
    item = (await anchor.read_checklist()).get("01")
    assert item is not None and item.status == "in_progress"


def test_the_lead_loop_gets_triage_only_when_configured(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    stub = StubSystemOne()
    on = build_lead_loop(
        _settings(), model=StubModel(), anchor=anchor, workdir=str(tmp_path), system_one=stub
    )
    off = build_lead_loop(
        _settings(system_one_triage=False),
        model=StubModel(),
        anchor=anchor,
        workdir=str(tmp_path),
        system_one=stub,
    )
    assert isinstance(on._triage, StallTriage) and off._triage is None
    assert build_stall_triage(_settings(), None) is None


# --- reranking --------------------------------------------------------------------------------


def _hits(*texts: str) -> list[RetrievalHit]:
    return [
        RetrievalHit(record=MemoryRecord(id=t, kind="semantic", text=t), score=0.5) for t in texts
    ]


def test_rerank_request_puts_each_passage_in_its_own_question() -> None:
    state, questions = system_one_rerank_request("fix the parser", _hits("a", "b"))
    assert state == {"task": "fix the parser"}
    assert list(questions) == ["p0", "p1"]
    assert questions["p1"].instructions["passage"] == "b"  # type: ignore[index]


def test_apply_relevance_orders_filters_and_cuts() -> None:
    hits = _hits("a", "b", "c", "d")
    ranked = apply_relevance(hits, [0.2, 0.9, 0.9, 0.05], k=2, min_p=0.1)
    assert [(h.record.text, h.score) for h in ranked] == [("b", 0.9), ("c", 0.9)]
    assert [
        h.record.text for h in apply_relevance(hits, [0.2, 0.9, 0.9, 0.05], k=None, min_p=0.1)
    ] == ["b", "c", "a"]


async def test_system_one_reranker_ranks_by_relevance_and_falls_back_on_failure() -> None:
    def respond(state: object, questions: dict[str, Question]) -> dict[str, NoulAnswer]:
        return {
            qid: NoulAnswer(noul=0.9 if "parser" in str(q.instructions) else 0.1)
            for qid, q in questions.items()
        }

    hits = _hits("unrelated note", "the parser fails on tabs")
    ranked = await SystemOneReranker(StubSystemOne(respond)).rerank("fix it", hits, k=1)
    assert [h.record.text for h in ranked] == ["the parser fails on tabs"]
    broken = SystemOneReranker(StubSystemOne(error="down"))
    assert await broken.rerank("fix it", hits, k=1) == hits[:1]
    assert await broken.rerank("fix it", []) == []


# --- wiring -----------------------------------------------------------------------------------


async def test_memory_uses_the_system_one_reranker_only_with_a_model(tmp_path: Path) -> None:
    from lha.memory.service import open_mission_memory
    from lha.persistence.sqlite import SqliteStore

    store = SqliteStore(tmp_path / "s.sqlite3")
    settings = _settings(memory_rerank="system_one", system_one_rerank_min=0.3)
    with_model = await open_mission_memory(
        settings, store=store, workdir=tmp_path, mission_id="m", system_one=StubSystemOne()
    )
    without = await open_mission_memory(settings, store=store, workdir=tmp_path, mission_id="m")
    assert with_model is not None and isinstance(with_model.reranker, SystemOneReranker)
    assert without is not None and type(without.reranker).__name__ == "NoopReranker"
    await store.close()


async def test_run_services_own_and_close_the_system_one_model(tmp_path: Path) -> None:
    from lha.persistence.services import open_run_services

    class _Closable(StubSystemOne):
        closed = False

        async def aclose(self) -> None:
            self.closed = True

    model = _Closable()
    services = await open_run_services(
        _settings(memory_enabled=False, sqlite_path=str(tmp_path / "s.sqlite3")),
        mission_id="m",
        workdir=tmp_path,
        meter=_meter(),
        system_one=model,
    )
    assert services.system_one is model
    await services.close()
    assert model.closed


async def test_stub_answers_neutrally_unless_scripted() -> None:
    from lha.contracts.system_one import ScoreAnswer

    result = await StubSystemOne().evaluate("s", QUESTIONS)
    assert result.answers["urgent"] == NoulAnswer(noul=0.5)
    team = result.answers["team"]
    assert isinstance(team, ChoiceAnswer) and team.confidence == 0.0
    mood = result.answers["mood"]
    assert isinstance(mood, ScoreAnswer) and mood.score == pytest.approx(1.0)
    assert mood.confidence == 0.0
