"""Regression tests for the agent/model/governor review fixes.

1. Price / unpriced-model settings are real ``Settings`` fields (env vars were silently ignored).
2. An unpriced model never crashes a cycle: spend is recorded as UNKNOWN (``cost_known=False``).
3. The local runner stops on deadlock (blocked item + dependents) with a ``deadlocked:`` reason.
4. Planner spend goes through the shared meter, and providers built by the runner are closed.
5. ``compact_messages(keep_last=0)`` no longer raises ``IndexError``.
6. ``OpenAICompatModel`` always sends ``max_tokens`` (the cap the budget meter reserves for).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import httpx
import pytest

from lha.agent import runner as runner_mod
from lha.agent.compaction import compact_messages
from lha.agent.runner import build_meter, plan_and_run_local, run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, TurnResult, UnknownPriceError, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.governor.metering import MeteredModel
from lha.model import build_provider
from lha.model.openai_compat import OpenAICompatModel
from lha.model.stub import StubModel

_PASS = Check(name="green", command=[sys.executable, "-c", "pass"])
_FAIL = Check(name="red", command=[sys.executable, "-c", "raise SystemExit(1)"])
_DONE = TurnResult(text='{"done": true, "summary": "ok"}')


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 20,
        "max_turns_per_cycle": 3,
        "stall_limit": 50,
    }
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


# --- 1. price settings are declared and reach the providers ---------------------------------


def test_openai_price_env_vars_are_honoured(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LHA_MODEL_BACKEND", "openai_compat")
    monkeypatch.setenv("LHA_MODEL_NAME", "m")
    monkeypatch.setenv("LHA_OPENAI_BASE_URL", "http://example.invalid/v1")
    monkeypatch.setenv("LHA_OPENAI_PRICE_IN_PER_MTOK", "1.0")
    monkeypatch.setenv("LHA_OPENAI_PRICE_OUT_PER_MTOK", "2.0")
    monkeypatch.setenv("LHA_ALLOW_UNPRICED_MODELS", "true")
    settings = Settings(_env_file=None)  # type: ignore[call-arg]
    assert settings.openai_price_in_per_mtok == 1.0
    assert settings.openai_price_out_per_mtok == 2.0
    assert settings.allow_unpriced_models is True

    provider = build_provider(settings)
    cost = provider.estimate_cost_usd(
        Usage(input_tokens=1_000_000, output_tokens=1_000_000, model="m")
    )
    assert cost == pytest.approx(3.0)


def test_claude_explicit_price_settings_apply(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LHA_MODEL_BACKEND", "claude")
    monkeypatch.setenv("LHA_MODEL_NAME", "claude-custom-model")  # not in the built-in table
    monkeypatch.setenv("LHA_ANTHROPIC_API_KEY", "sk-test")
    monkeypatch.setenv("LHA_CLAUDE_PRICE_IN_PER_MTOK", "4.0")
    monkeypatch.setenv("LHA_CLAUDE_PRICE_OUT_PER_MTOK", "8.0")
    provider = build_provider(Settings(_env_file=None))  # type: ignore[call-arg]
    cost = provider.estimate_cost_usd(
        Usage(input_tokens=1_000_000, output_tokens=0, model="claude-custom-model")
    )
    assert cost == pytest.approx(4.0)


def test_allow_unpriced_models_reaches_the_governor() -> None:
    assert build_meter(_settings(allow_unpriced_models=True)).governor._allow_unknown_cost
    assert not build_meter(_settings()).governor._allow_unknown_cost


# --- 2. unpriced model: recorded as unknown, never a crash ----------------------------------


class _Unpriced(StubModel):
    def estimate_cost_usd(self, usage: Usage) -> float:
        raise UnknownPriceError("no price")


@pytest.mark.asyncio
async def test_unpriced_model_is_recorded_unknown_when_allowed(tmp_path: Path) -> None:
    settings = _settings(allow_unpriced_models=True)
    meter = build_meter(settings)
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="x")]),
        checks=[_PASS],
        settings=settings,
        meter=meter,
        model=_Unpriced(script=[_DONE]),
    )
    assert summary.completed
    assert meter.ledger.entries and all(not e.cost_known for e in meter.ledger.entries)


@pytest.mark.asyncio
async def test_unpriced_model_is_refused_not_crashed_by_default(tmp_path: Path) -> None:
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="x")]),
        checks=[_PASS],
        settings=_settings(),
        model=_Unpriced(script=[_DONE]),
    )
    assert summary.stopped_reason.startswith("governor:")
    assert not summary.completed


# --- 3. deadlock stops the runner with a clear reason ---------------------------------------


@pytest.mark.asyncio
async def test_runner_stops_on_dependency_deadlock(tmp_path: Path) -> None:
    checklist = Checklist(
        items=[
            ChecklistItem(id="01", description="a"),
            ChecklistItem(id="02", description="b", depends_on=["01"]),
        ]
    )
    summary = await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=checklist,
        checks=[_FAIL],
        settings=_settings(),
        model=StubModel(script=[_DONE]),
    )
    assert summary.stopped_reason.startswith("deadlocked:")
    assert "01" in summary.stopped_reason
    assert "None" not in summary.stopped_reason
    assert summary.cycles == 3  # 3 failed attempts block item 01; no idle cycles are burned
    assert not summary.completed


# --- 4. planner is metered; runner-built providers are closed -------------------------------


class _Closable(StubModel):
    closed = 0

    async def aclose(self) -> None:
        type(self).closed += 1


@pytest.mark.asyncio
async def test_planner_metered_and_providers_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _Closable.closed = 0
    planner_reply = TurnResult(
        text=json.dumps([{"description": "do it"}]), usage=Usage(input_tokens=5, output_tokens=5)
    )
    built: list[_Closable] = []

    def fake_build(settings: Settings | None = None, **_: object) -> _Closable:
        model = _Closable(script=[planner_reply] if not built else [_DONE])
        built.append(model)
        return model

    monkeypatch.setattr(runner_mod, "build_provider", fake_build)
    settings = _settings()
    meter = build_meter(settings)
    summary = await plan_and_run_local(
        workdir=str(tmp_path),
        title="t",
        task="do it",
        checks=[_PASS],
        settings=settings,
        meter=meter,
    )
    assert summary.completed
    assert {"planner", "lead"} <= {e.role for e in meter.ledger.entries}
    assert len(built) == 2 and _Closable.closed == 2  # planner + lead providers both closed


@pytest.mark.asyncio
async def test_caller_supplied_model_is_not_closed(tmp_path: Path) -> None:
    _Closable.closed = 0
    await run_mission_local(
        workdir=str(tmp_path),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="x")]),
        checks=[_PASS],
        settings=_settings(),
        model=_Closable(script=[_DONE]),
    )
    assert _Closable.closed == 0


# --- 5. compaction with keep_last=0 ---------------------------------------------------------


@pytest.mark.asyncio
async def test_compact_messages_keep_last_zero() -> None:
    messages = [
        ModelMessage(role="system", content="s"),
        ModelMessage(role="user", content="task"),
        ModelMessage(role="assistant", content="a"),
        ModelMessage(role="user", content="OBSERVATION (x): y"),
    ]
    out = await compact_messages(model=StubModel(), messages=messages, keep_last=0)
    assert [m.role for m in out] == ["system", "user", "user"]
    assert out[-1].content.startswith("[compacted summary")


# --- 6. OpenAI-compatible requests carry the cap the meter reserves for ---------------------


def _capture_client(seen: list[dict[str, object]]) -> httpx.AsyncClient:
    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "model": "m",
                "choices": [{"message": {"content": "hi"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1},
            },
        )

    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


@pytest.mark.asyncio
async def test_openai_compat_always_sends_max_tokens() -> None:
    seen: list[dict[str, object]] = []
    async with _capture_client(seen) as client:
        model = OpenAICompatModel(
            base_url="http://x/v1",
            model_name="m",
            price_in_per_mtok=1.0,
            price_out_per_mtok=1.0,
            client=client,
            default_max_tokens=1234,
        )
        await model.complete([ModelMessage(role="user", content="hi")])
        await model.complete([ModelMessage(role="user", content="hi")], max_tokens=50)
    assert seen[0]["max_tokens"] == 1234
    assert seen[1]["max_tokens"] == 50


def test_openai_compat_worst_case_uses_the_sent_cap() -> None:
    model = OpenAICompatModel(
        base_url="http://x/v1", model_name="m", price_in_per_mtok=0.0, price_out_per_mtok=1.0
    )
    assert model.default_max_tokens == 8192
    metered = MeteredModel(model, build_meter(_settings()))
    worst = metered.worst_case_usd([ModelMessage(role="user", content="")])
    assert worst == pytest.approx(8192 / 1_000_000)
