"""Tests for the real HTTP model backends, using httpx MockTransport.

These exercise the genuine request-building and response-parsing of the adapters against a mocked
transport — no network, no API key. This tests the HTTP *contract* (honest), not a faked model.
"""

from __future__ import annotations

import json
from collections.abc import Callable

import httpx
import pytest

from lha.contracts.model import ModelMessage
from lha.model.claude import ClaudeModel
from lha.model.openai_compat import OpenAICompatModel


def _client(handler: Callable[[httpx.Request], httpx.Response]) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


@pytest.mark.asyncio
async def test_openai_compat_maps_request_and_response() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={
                "choices": [{"message": {"content": "hello world"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 3},
            },
        )

    model = OpenAICompatModel(
        base_url="http://local/v1",
        model_name="m1",
        client=_client(handler),
        price_in_per_mtok=1.0,
        price_out_per_mtok=2.0,
    )
    result = await model.complete([ModelMessage(role="user", content="hi")])

    assert str(captured["url"]).endswith("/chat/completions")
    assert isinstance(captured["body"], dict) and captured["body"]["model"] == "m1"
    assert result.text == "hello world"
    assert result.usage.input_tokens == 12
    assert result.usage.output_tokens == 3
    assert model.estimate_cost_usd(result.usage) == pytest.approx(12 / 1e6 * 1.0 + 3 / 1e6 * 2.0)


@pytest.mark.asyncio
async def test_openai_compat_parses_tool_calls() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "choices": [
                    {
                        "message": {
                            "content": None,
                            "tool_calls": [
                                {
                                    "id": "t1",
                                    "function": {"name": "search", "arguments": '{"q": "x"}'},
                                }
                            ],
                        },
                        "finish_reason": "tool_calls",
                    }
                ],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1},
            },
        )

    model = OpenAICompatModel(base_url="http://local/v1", model_name="m1", client=_client(handler))
    result = await model.complete([ModelMessage(role="user", content="hi")])

    assert len(result.tool_calls) == 1
    assert result.tool_calls[0].name == "search"
    assert result.tool_calls[0].arguments == {"q": "x"}


@pytest.mark.asyncio
async def test_claude_maps_system_content_blocks_and_cost() -> None:
    captured: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        captured["api_key"] = request.headers.get("x-api-key")
        return httpx.Response(
            200,
            json={
                "content": [
                    {"type": "text", "text": "hi there"},
                    {"type": "tool_use", "id": "u1", "name": "edit", "input": {"path": "a.py"}},
                ],
                "usage": {"input_tokens": 10, "output_tokens": 4},
                "stop_reason": "tool_use",
            },
        )

    model = ClaudeModel(api_key="sk-test", model_name="claude-sonnet-4-6", client=_client(handler))
    result = await model.complete(
        [
            ModelMessage(role="system", content="be terse"),
            ModelMessage(role="user", content="hi"),
        ]
    )

    body = captured["body"]
    assert isinstance(body, dict)
    assert body["system"] == "be terse"
    assert body["messages"] == [{"role": "user", "content": "hi"}]
    assert captured["api_key"] == "sk-test"
    assert result.text == "hi there"
    assert result.tool_calls[0].name == "edit"
    assert result.tool_calls[0].arguments == {"path": "a.py"}
    assert result.usage.input_tokens == 10
    assert model.estimate_cost_usd(result.usage) == pytest.approx(10 / 1e6 * 3.0 + 4 / 1e6 * 15.0)
