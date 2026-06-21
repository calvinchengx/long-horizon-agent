"""Pricing, retry/backoff, failover classification and tool-message mapping of the model layer."""

from __future__ import annotations

import json
from collections.abc import Callable

import httpx
import pytest

from lha.contracts.model import (
    ModelMessage,
    ToolCall,
    TurnResult,
    UnknownPriceError,
    Usage,
)
from lha.model import build_provider, secret_value
from lha.model.claude import ClaudeModel
from lha.model.failover import FailoverModel
from lha.model.openai_compat import OpenAICompatModel
from lha.model.pricing import ModelPrice
from lha.model.retry import backoff_delay, is_retryable, retry_after_seconds
from lha.model.stub import StubModel

_OK_CLAUDE = {
    "model": "claude-sonnet-4-6",
    "content": [{"type": "text", "text": "ok"}],
    "usage": {"input_tokens": 1, "output_tokens": 1},
    "stop_reason": "end_turn",
}


def _client(handler: Callable[[httpx.Request], httpx.Response]) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


class _Sleeps:
    def __init__(self) -> None:
        self.calls: list[float] = []

    async def __call__(self, seconds: float) -> None:
        self.calls.append(seconds)


def _status_error(status: int, headers: dict[str, str] | None = None) -> httpx.HTTPStatusError:
    request = httpx.Request("POST", "http://x/")
    response = httpx.Response(status, request=request, headers=headers)
    return httpx.HTTPStatusError(str(status), request=request, response=response)


# --- pricing (M2) -------------------------------------------------------------------------


def test_claude_cache_tokens_billed_with_anthropic_multipliers() -> None:
    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6")
    usage = Usage(
        input_tokens=1_000_000,
        output_tokens=1_000_000,
        cache_read_input_tokens=1_000_000,
        cache_creation_input_tokens=1_000_000,
        model="claude-sonnet-4-6",
    )
    # input 3 + write 3*1.25 + read 3*0.1 + output 15
    assert model.estimate_cost_usd(usage) == pytest.approx(3 + 3.75 + 0.3 + 15)


def test_claude_one_hour_cache_writes_billed_at_2x() -> None:
    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6")
    usage = Usage(
        cache_creation_input_tokens=1_000_000,
        cache_creation_1h_input_tokens=1_000_000,
        model="claude-sonnet-4-6",
    )
    assert model.estimate_cost_usd(usage) == pytest.approx(3 * 2.0)


def test_claude_prices_by_reported_model_not_configured_model() -> None:
    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6")
    usage = Usage(input_tokens=1_000_000, model="claude-haiku-4-5-20251001")  # dated snapshot
    assert model.estimate_cost_usd(usage) == pytest.approx(1.0)


def test_claude_unknown_model_fails_fast_without_explicit_prices() -> None:
    with pytest.raises(UnknownPriceError):
        ClaudeModel(api_key="k", model_name="claude-imaginary-9")
    model = ClaudeModel(api_key="k", model_name="claude-imaginary-9", price=ModelPrice(2.0, 4.0))
    assert model.estimate_cost_usd(
        Usage(input_tokens=1_000_000, model="claude-imaginary-9")
    ) == pytest.approx(2.0)


def test_claude_unknown_reported_model_raises() -> None:
    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6")
    with pytest.raises(UnknownPriceError):
        model.estimate_cost_usd(Usage(input_tokens=1, model="some-other-model"))


@pytest.mark.asyncio
async def test_claude_usage_records_reported_model_and_cache_breakdown() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        body = dict(_OK_CLAUDE)
        body["model"] = "claude-haiku-4-5-20251001"
        body["usage"] = {
            "input_tokens": 5,
            "output_tokens": 2,
            "cache_read_input_tokens": 7,
            "cache_creation_input_tokens": 11,
            "cache_creation": {"ephemeral_5m_input_tokens": 8, "ephemeral_1h_input_tokens": 3},
        }
        return httpx.Response(200, json=body)

    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6", client=_client(handler))
    result = await model.complete([ModelMessage(role="user", content="hi")])
    assert result.usage.model == "claude-haiku-4-5-20251001"
    assert result.usage.cache_read_input_tokens == 7
    assert result.usage.cache_creation_1h_input_tokens == 3
    assert result.usage.provider == "claude:claude-sonnet-4-6"


def test_openai_compat_without_prices_is_unknown_not_zero() -> None:
    model = OpenAICompatModel(base_url="http://x/v1", model_name="m")
    with pytest.raises(UnknownPriceError):
        model.estimate_cost_usd(Usage(input_tokens=10, output_tokens=10, model="m"))
    free = OpenAICompatModel(
        base_url="http://x/v1", model_name="m", price_in_per_mtok=0.0, price_out_per_mtok=0.0
    )
    assert free.estimate_cost_usd(Usage(input_tokens=10, output_tokens=10)) == 0.0


def test_build_provider_accepts_secretstr_and_plain_keys() -> None:
    from pydantic import SecretStr

    from lha.config import Settings

    assert secret_value(SecretStr("abc")) == "abc"
    assert secret_value("abc") == "abc"
    assert secret_value(None) is None
    settings = Settings(
        model_backend="claude", model_name="claude-sonnet-4-6", anthropic_api_key="sk-x"
    )
    provider = build_provider(settings)
    assert provider.name == "claude:claude-sonnet-4-6"
    ollama = build_provider(Settings(model_backend="ollama", model_name="llama3"))
    assert ollama.estimate_cost_usd(Usage(input_tokens=1000)) == 0.0  # local => genuinely $0


# --- retry / backoff (M4) -----------------------------------------------------------------


def test_retry_classification() -> None:
    assert is_retryable(_status_error(429))
    assert is_retryable(_status_error(503))
    assert is_retryable(_status_error(529))
    assert is_retryable(httpx.ConnectError("down"))
    assert is_retryable(httpx.ReadTimeout("slow"))
    for status in (400, 401, 403, 404):
        assert not is_retryable(_status_error(status))
    assert not is_retryable(RuntimeError("bug"))


def test_retry_after_is_honoured_and_capped() -> None:
    assert retry_after_seconds(_status_error(429, {"retry-after": "7"})) == 7.0
    assert backoff_delay(0, _status_error(429, {"retry-after": "7"})) == 7.0
    assert backoff_delay(0, _status_error(429, {"retry-after": "9999"}), max_s=60) == 60.0
    assert backoff_delay(3, _status_error(503), base_s=1.0) == 8.0


@pytest.mark.asyncio
async def test_claude_retries_429_with_retry_after_then_succeeds() -> None:
    attempts = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        attempts["n"] += 1
        if attempts["n"] < 3:
            return httpx.Response(429, headers={"retry-after": "2"}, json={"error": "rate"})
        return httpx.Response(200, json=_OK_CLAUDE)

    sleeps = _Sleeps()
    model = ClaudeModel(
        api_key="k", model_name="claude-sonnet-4-6", client=_client(handler), sleep=sleeps
    )
    result = await model.complete([ModelMessage(role="user", content="hi")])
    assert result.text == "ok"
    assert attempts["n"] == 3
    assert sleeps.calls == [2.0, 2.0]


@pytest.mark.asyncio
async def test_claude_does_not_retry_client_errors() -> None:
    attempts = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        attempts["n"] += 1
        return httpx.Response(400, json={"error": "bad request"})

    model = ClaudeModel(
        api_key="k", model_name="claude-sonnet-4-6", client=_client(handler), sleep=_Sleeps()
    )
    with pytest.raises(httpx.HTTPStatusError):
        await model.complete([ModelMessage(role="user", content="hi")])
    assert attempts["n"] == 1


@pytest.mark.asyncio
async def test_claude_retries_are_bounded() -> None:
    attempts = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        attempts["n"] += 1
        return httpx.Response(503)

    model = ClaudeModel(
        api_key="k",
        model_name="claude-sonnet-4-6",
        client=_client(handler),
        sleep=_Sleeps(),
        max_retries=2,
    )
    with pytest.raises(httpx.HTTPStatusError):
        await model.complete([ModelMessage(role="user", content="hi")])
    assert attempts["n"] == 3


# --- failover (M4 / M2) -------------------------------------------------------------------


class _Raises:
    def __init__(self, exc: Exception, name: str = "raises") -> None:
        self.name = name
        self._exc = exc
        self.calls = 0

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls += 1
        raise self._exc

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 100.0


@pytest.mark.asyncio
async def test_failover_raises_immediately_on_auth_error() -> None:
    backup = StubModel(script=[TurnResult(text="backup")])
    primary = _Raises(_status_error(401))
    fm = FailoverModel([primary, backup], sleep=_Sleeps())
    with pytest.raises(httpx.HTTPStatusError):
        await fm.complete([ModelMessage(role="user", content="hi")])
    assert primary.calls == 1


@pytest.mark.asyncio
async def test_failover_backs_off_between_rounds() -> None:
    primary = _Raises(_status_error(429, {"retry-after": "3"}), name="p1")
    sleeps = _Sleeps()
    fm = FailoverModel([primary], max_rounds=3, sleep=sleeps)
    with pytest.raises(httpx.HTTPStatusError):
        await fm.complete([ModelMessage(role="user", content="hi")])
    assert primary.calls == 3
    assert sleeps.calls == [3.0, 3.0]


@pytest.mark.asyncio
async def test_failover_prices_by_serving_provider() -> None:
    primary = _Raises(_status_error(503), name="expensive")
    backup = StubModel(script=[TurnResult(text="backup", usage=Usage(input_tokens=5))])
    fm = FailoverModel([primary, backup], sleep=_Sleeps())
    result = await fm.complete([ModelMessage(role="user", content="hi")])
    assert result.usage.provider == backup.name
    assert fm.estimate_cost_usd(result.usage) == 0.0  # the stub's price, not the primary's


# --- tool messages (L4: openai tool_call_id) ----------------------------------------------


_TOOL_CONVERSATION = [
    ModelMessage(role="user", content="read it"),
    ModelMessage(
        role="assistant",
        content="",
        tool_calls=[
            ToolCall(id="call_1", name="read_file", arguments={"path": "a"}),
            ToolCall(id="call_2", name="read_file", arguments={"path": "b"}),
        ],
    ),
    ModelMessage(role="tool", content="A", tool_call_id="call_1"),
    ModelMessage(role="tool", content="B", tool_call_id="call_2"),
]


@pytest.mark.asyncio
async def test_openai_compat_sends_tool_call_ids() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(
            200, json={"choices": [{"message": {"content": "done"}, "finish_reason": "stop"}]}
        )

    model = OpenAICompatModel(base_url="http://x/v1", model_name="m", client=_client(handler))
    await model.complete(_TOOL_CONVERSATION)
    body = captured["body"]
    assert isinstance(body, dict)
    msgs = body["messages"]
    assert msgs[1]["tool_calls"][0]["id"] == "call_1"
    assert json.loads(msgs[1]["tool_calls"][1]["function"]["arguments"]) == {"path": "b"}
    assert msgs[2] == {"role": "tool", "tool_call_id": "call_1", "content": "A"}
    assert msgs[3] == {"role": "tool", "tool_call_id": "call_2", "content": "B"}


@pytest.mark.asyncio
async def test_openai_compat_never_sends_tool_role_without_id() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json={"choices": [{"message": {"content": "x"}}]})

    model = OpenAICompatModel(base_url="http://x/v1", model_name="m", client=_client(handler))
    await model.complete([ModelMessage(role="tool", content="orphan result")])
    body = captured["body"]
    assert isinstance(body, dict)
    assert body["messages"][0]["role"] == "user"


@pytest.mark.asyncio
async def test_claude_maps_tool_use_and_merges_tool_results() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json=_OK_CLAUDE)

    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6", client=_client(handler))
    await model.complete(_TOOL_CONVERSATION)
    body = captured["body"]
    assert isinstance(body, dict)
    msgs = body["messages"]
    assert [b["type"] for b in msgs[1]["content"]] == ["tool_use", "tool_use"]
    assert len(msgs) == 3  # both tool results merged into one user turn
    assert [b["tool_use_id"] for b in msgs[2]["content"]] == ["call_1", "call_2"]


@pytest.mark.asyncio
async def test_owned_http_client_is_closed() -> None:
    model = ClaudeModel(api_key="k", model_name="claude-sonnet-4-6")
    async with model:
        pass
    assert model._client.is_closed
    shared = httpx.AsyncClient()
    borrowed = OpenAICompatModel(base_url="http://x/v1", model_name="m", client=shared)
    await borrowed.aclose()
    assert not shared.is_closed  # a borrowed client belongs to the caller
    await shared.aclose()
