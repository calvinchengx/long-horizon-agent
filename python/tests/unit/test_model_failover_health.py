"""Fallback chains from settings (``LHA_FALLBACK_MODELS``).

No real network: providers share an ``httpx.AsyncClient`` over a ``MockTransport``.
"""

from __future__ import annotations

from collections.abc import Callable

import httpx
import pytest

from lha.config import Settings
from lha.contracts.model import ModelMessage, Usage
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
from lha.model.pricing import ModelPrice

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
