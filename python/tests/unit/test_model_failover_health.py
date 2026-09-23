"""Fallback chains from settings (``LHA_FALLBACK_MODELS``) and real model health probes.

No real network: providers share an ``httpx.AsyncClient`` over a ``MockTransport``.
"""

from __future__ import annotations

import functools
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest

from lha.config import Settings
from lha.contracts.model import ModelMessage, Usage
from lha.durable import activities as acts
from lha.durable.types import HealthInput
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor
from lha.governor.metering import CostMeter
from lha.model import (
    CHAIN_MEMBER_RETRIES,
    ClaudeModel,
    FailoverModel,
    OpenAICompatModel,
    StubModel,
    build_provider,
    parse_fallback_entry,
)
from lha.model.health import ModelHealth, probe_model, probe_provider
from lha.model.pricing import ModelPrice
from lha.state import git_ops

_MSG = [ModelMessage(role="user", content="hi")]


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {"_env_file": None, "model_backend": "stub"}
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


def _client(handler: Callable[[httpx.Request], httpx.Response]) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


# --- parsing ----------------------------------------------------------------------------------


def test_parse_fallback_entries() -> None:
    assert parse_fallback_entry("ollama:qwen3:8b").model == "qwen3:8b"
    priced = parse_fallback_entry(" OpenAI_Compat:llama-3.3-70b@0.59/0.79 ")
    assert priced.backend == "openai_compat" and priced.model == "llama-3.3-70b"
    assert priced.price == ModelPrice(0.59, 0.79)
    assert parse_fallback_entry("claude:claude-haiku-4-5").price is None


@pytest.mark.parametrize(
    ("entry", "error"),
    [
        ("claude", "expected 'backend:model"),
        ("gpt:4o", "expected 'backend:model"),
        ("claude:", "empty model name"),
        ("openai_compat:m@abc", "invalid price"),
        ("openai_compat:m@1", "invalid price"),
        ("openai_compat:m@-1/2", "invalid price"),
        ("openai_compat:@1/2", "empty model name"),
    ],
)
def test_parse_fallback_entry_errors(entry: str, error: str) -> None:
    with pytest.raises(ValueError, match=error):
        parse_fallback_entry(entry)


# --- build_provider ---------------------------------------------------------------------------


def test_no_fallbacks_returns_the_single_backend() -> None:
    assert isinstance(build_provider(_settings()), StubModel)


def test_fallbacks_build_a_failover_chain_in_order() -> None:
    settings = _settings(
        model_backend="claude",
        model_name="claude-sonnet-4-6",
        anthropic_api_key="sk-ant-test",
        openai_base_url="https://api.groq.test/openai/v1",
        fallback_models=(
            " openai_compat:llama-3.3-70b@0.59/0.79, ollama:qwen3:8b,,claude:claude-haiku-4-5"
        ),
        fallback_max_rounds=3,
    )
    provider = build_provider(settings)
    assert isinstance(provider, FailoverModel)
    assert provider.name == (
        "failover:claude:claude-sonnet-4-6,openai_compat:llama-3.3-70b,"
        "ollama:qwen3:8b,claude:claude-haiku-4-5"
    )
    members = provider._providers
    assert isinstance(members[0], ClaudeModel) and isinstance(members[1], OpenAICompatModel)
    assert all(getattr(m, "_max_retries", CHAIN_MEMBER_RETRIES) == 1 for m in members)
    assert provider._max_rounds == 3
    # Per-role routing overrides the primary model only.
    routed = build_provider(settings, model_name="claude-opus-4-8")
    assert routed.name.startswith("failover:claude:claude-opus-4-8,openai_compat:")


def test_fallback_config_errors_surface_at_build() -> None:
    with pytest.raises(ValueError, match="LHA_OPENAI_BASE_URL"):
        build_provider(_settings(fallback_models="openai_compat:m"))
    with pytest.raises(ValueError, match="LHA_ANTHROPIC_API_KEY"):
        build_provider(_settings(fallback_models="claude:claude-haiku-4-5"))
    with pytest.raises(ValueError, match="or neither"):
        build_provider(
            _settings(
                model_backend="openai_compat",
                openai_base_url="https://x.test/v1",
                openai_price_in_per_mtok=1.0,
            )
        )


@pytest.mark.asyncio
async def test_failover_serves_from_fallback_and_is_priced_by_the_serving_model() -> None:
    calls: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.host)
        if request.url.host == "api.anthropic.com":
            return httpx.Response(529, headers={"retry-after": "0"}, json={"error": "overloaded"})
        return httpx.Response(
            200,
            json={
                "model": "llama-3.3-70b-versatile",
                "choices": [{"message": {"content": "hi"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 1_000_000, "completion_tokens": 1_000_000},
            },
        )

    settings = _settings(
        model_backend="claude",
        model_name="claude-sonnet-4-6",
        anthropic_api_key="sk-ant-test",
        openai_base_url="https://api.groq.test/openai/v1",
        fallback_models="openai_compat:llama-3.3-70b@0.59/0.79",
    )
    async with _client(handler) as client:
        provider = build_provider(settings, client=client)
        meter = CostMeter(
            ledger=CostLedger(), governor=BudgetGovernor(ceiling_usd=100.0, max_cycles=10)
        )
        result = await meter.wrap(provider, role="lead").complete(_MSG)
    # Claude: 1 attempt + CHAIN_MEMBER_RETRIES retry, then the fallback serves the turn.
    assert calls == ["api.anthropic.com"] * (1 + CHAIN_MEMBER_RETRIES) + ["api.groq.test"]
    assert result.usage.provider == "openai_compat:llama-3.3-70b"
    assert result.usage.model == "llama-3.3-70b-versatile"
    entry = meter.ledger.entries[-1]
    assert entry.cost_known and entry.usd == pytest.approx(0.59 + 0.79)
    assert provider.estimate_cost_usd(
        Usage(input_tokens=1_000_000, provider="claude:claude-sonnet-4-6")
    ) == pytest.approx(3.0)


# --- health probes ----------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_stub_probe_is_healthy() -> None:
    health = await probe_model(_settings())
    assert health.ok and "stub" in health.detail


@pytest.mark.asyncio
async def test_openai_compat_probe_lists_models() -> None:
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"data": []})

    settings = _settings(
        model_backend="openai_compat",
        openai_base_url="https://api.groq.test/openai/v1",
        openai_api_key="gsk-key",
        model_name="m",
    )
    async with _client(handler) as client:
        health = await probe_model(settings, client=client)
    assert health.ok
    assert str(seen[0].url) == "https://api.groq.test/openai/v1/models"
    assert seen[0].method == "GET" and seen[0].headers["authorization"] == "Bearer gsk-key"


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("handler", "detail"),
    [
        (lambda r: httpx.Response(401), "HTTP 401"),
        (lambda r: httpx.Response(503), "HTTP 503"),
    ],
)
async def test_openai_compat_probe_reports_down(
    handler: Callable[[httpx.Request], httpx.Response], detail: str
) -> None:
    settings = _settings(model_backend="openai_compat", openai_base_url="https://x.test/v1")
    async with _client(handler) as client:
        health = await probe_model(settings, client=client)
    assert not health.ok and detail in health.detail


@pytest.mark.asyncio
async def test_probe_reports_transport_errors_and_timeouts() -> None:
    def refuse(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    def slow(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("read timed out", request=request)

    settings = _settings(model_backend="openai_compat", openai_base_url="https://x.test/v1")
    async with _client(refuse) as client:
        refused = await probe_model(settings, client=client)
    assert not refused.ok and "connection refused" in refused.detail
    async with _client(slow) as client:
        timed_out = await probe_model(settings, client=client)
    assert not timed_out.ok and "timed out" in timed_out.detail


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("tags", "model", "ok"),
    [
        ([{"name": "qwen3:8b"}], "qwen3:8b", True),
        ([{"name": "llama3:latest"}], "llama3", True),
        ([{"name": "llama3:latest"}, "junk"], "qwen3:8b", False),
    ],
)
async def test_ollama_probe_checks_the_model_is_pulled(
    tags: list[Any], model: str, ok: bool
) -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        return httpx.Response(200, json={"models": tags})

    settings = _settings(
        model_backend="ollama", model_name=model, ollama_base_url="http://ollama.test:11434/"
    )
    async with _client(handler) as client:
        health = await probe_model(settings, client=client)
    assert health.ok is ok
    assert seen == ["http://ollama.test:11434/api/tags"]
    if not ok:
        assert "not pulled" in health.detail


@pytest.mark.asyncio
async def test_ollama_probe_handles_garbage() -> None:
    settings = _settings(model_backend="ollama", model_name="m")
    async with _client(lambda r: httpx.Response(200, text="not json")) as client:
        health = await probe_model(settings, client=client)
    assert not health.ok


@pytest.mark.asyncio
async def test_claude_probe_gets_the_model() -> None:
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200 if "haiku" in request.url.path else 404)

    base = {"model_backend": "claude", "anthropic_api_key": "sk-ant-test"}
    async with _client(handler) as client:
        ok = await probe_model(_settings(**base, model_name="claude-haiku-4-5"), client=client)
        missing = await probe_model(
            _settings(**base, model_name="claude-sonnet-4-6"), client=client
        )
    assert ok.ok and not missing.ok and "HTTP 404" in missing.detail
    assert str(seen[0].url) == "https://api.anthropic.com/v1/models/claude-haiku-4-5"
    assert seen[0].headers["x-api-key"] == "sk-ant-test"
    assert seen[0].headers["anthropic-version"] == ClaudeModel.API_VERSION


@pytest.mark.asyncio
async def test_failover_probe_is_healthy_if_any_member_is() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(503 if request.url.host == "api.anthropic.com" else 200, json={})

    settings = _settings(
        model_backend="claude",
        model_name="claude-haiku-4-5",
        anthropic_api_key="sk-ant-test",
        openai_base_url="https://x.test/v1",
        fallback_models="openai_compat:m",
    )
    async with _client(handler) as client:
        health = await probe_model(settings, client=client)
    assert (
        health.ok and "HTTP 503" in health.detail and "openai_compat:m: reachable" in health.detail
    )

    async with _client(lambda r: httpx.Response(500)) as client:
        down = await probe_model(settings, client=client)
    assert not down.ok


@pytest.mark.asyncio
async def test_probe_provider_edge_cases() -> None:
    class NoProbe:
        name = "custom:x"

    class Hangs:
        async def health_check(self, *, timeout_s: float) -> ModelHealth:
            raise TimeoutError

    built = await probe_provider(NoProbe(), timeout_s=1.0)
    assert built.ok and "no probe available" in built.detail
    hung = await probe_provider(Hangs(), timeout_s=1.0)
    assert not hung.ok and "timed out" in hung.detail


@pytest.mark.asyncio
async def test_probe_reports_config_errors() -> None:
    health = await probe_model(_settings(model_backend="openai_compat"))
    assert not health.ok and "LHA_OPENAI_BASE_URL" in health.detail


# --- the durable health activity parks on a model outage -------------------------------------


@pytest.mark.asyncio
async def test_durable_health_probe_reports_model_outage(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    git_ops.init_repo(tmp_path)
    (tmp_path / "a.txt").write_text("a", encoding="utf-8")
    git_ops.commit_all(tmp_path, "init")
    settings = _settings(
        model_backend="openai_compat",
        openai_base_url="https://x.test/v1",
        sandbox="local",
        allow_unsafe_local=True,
    )
    async with _client(lambda r: httpx.Response(503)) as client:
        monkeypatch.setattr(acts, "probe_model", functools.partial(probe_model, client=client))
        down = await acts.probe_health(HealthInput("m", str(tmp_path)), settings=settings)
    assert not down.healthy and "model" in down.reason and "HTTP 503" in down.reason

    async with _client(lambda r: httpx.Response(200, json={"data": []})) as client:
        monkeypatch.setattr(acts, "probe_model", functools.partial(probe_model, client=client))
        up = await acts.probe_health(HealthInput("m", str(tmp_path)), settings=settings)
    assert up.healthy
