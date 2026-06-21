"""OpenAI-compatible model backend.

One adapter covers a large slice of the ecosystem because they all speak the OpenAI
`/chat/completions` shape: **Ollama** (local, `$0`, via its `/v1` endpoint), and free-tier/paid
clouds (Groq, Google Gemini's OpenAI-compat endpoint, OpenRouter, Together, ...). Real HTTP, real
usage, real cost from configured prices. Prices default to ``None`` = UNKNOWN: tokens are still
recorded but ``estimate_cost_usd`` raises ``UnknownPriceError`` rather than silently reporting
``$0``. Pass ``0.0`` explicitly for genuinely free/local endpoints.

Transient failures (429 / 5xx / timeouts / connection errors) are retried with bounded exponential
backoff honouring ``Retry-After``; 4xx client errors raise immediately.
"""

from __future__ import annotations

import asyncio
import json
from types import TracebackType

import httpx

from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, TurnResult, Usage
from lha.model.pricing import ModelPrice, require_price
from lha.model.retry import Sleep, with_retries


def _to_openai_messages(messages: list[ModelMessage]) -> list[dict[str, object]]:
    """Map neutral messages to chat-completions messages (tool calls / results preserved)."""
    out: list[dict[str, object]] = []
    for m in messages:
        if m.role == "assistant" and m.tool_calls:
            out.append(
                {
                    "role": "assistant",
                    "content": m.content or None,
                    "tool_calls": [
                        {
                            "id": c.id,
                            "type": "function",
                            "function": {"name": c.name, "arguments": json.dumps(c.arguments)},
                        }
                        for c in m.tool_calls
                    ],
                }
            )
        elif m.role == "tool":
            if m.tool_call_id:
                out.append({"role": "tool", "tool_call_id": m.tool_call_id, "content": m.content})
            else:
                # OpenAI rejects role:"tool" without tool_call_id; degrade to a plain user turn.
                out.append({"role": "user", "content": f"TOOL RESULT:\n{m.content}"})
        else:
            out.append({"role": m.role, "content": m.content})
    return out


class OpenAICompatModel(ModelProvider):
    """A ``ModelProvider`` backed by any OpenAI-compatible chat-completions endpoint."""

    def __init__(
        self,
        *,
        base_url: str,
        model_name: str,
        api_key: str | None = None,
        price_in_per_mtok: float | None = None,
        price_out_per_mtok: float | None = None,
        timeout_s: float = 120.0,
        client: httpx.AsyncClient | None = None,
        label: str = "openai_compat",
        max_retries: int = 3,
        retry_base_delay_s: float = 1.0,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        if (price_in_per_mtok is None) != (price_out_per_mtok is None):
            raise ValueError("configure both price_in_per_mtok and price_out_per_mtok, or neither")
        self.name = f"{label}:{model_name}"
        self._base_url = base_url.rstrip("/")
        self._model = model_name
        self._api_key = api_key
        self._price: ModelPrice | None = (
            ModelPrice(price_in_per_mtok, price_out_per_mtok)
            if price_in_per_mtok is not None and price_out_per_mtok is not None
            else None
        )
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=timeout_s)
        self._max_retries = max_retries
        self._retry_base_delay_s = retry_base_delay_s
        self._sleep = sleep

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        payload: dict[str, object] = {
            "model": self._model,
            "messages": _to_openai_messages(messages),
        }
        if max_tokens is not None:
            payload["max_tokens"] = max_tokens
        if tools:
            payload["tools"] = tools
        headers = {"Authorization": f"Bearer {self._api_key}"} if self._api_key else {}

        async def _post() -> httpx.Response:
            resp = await self._client.post(
                f"{self._base_url}/chat/completions", json=payload, headers=headers
            )
            resp.raise_for_status()
            return resp

        resp = await with_retries(
            _post,
            max_retries=self._max_retries,
            base_delay_s=self._retry_base_delay_s,
            sleep=self._sleep,
        )
        data = resp.json()

        choice = data["choices"][0]
        message = choice.get("message", {})
        text = message.get("content") or ""
        tool_calls = _parse_tool_calls(message.get("tool_calls"))
        usage = data.get("usage") or {}
        reported_model = data.get("model")

        return TurnResult(
            text=text,
            tool_calls=tool_calls,
            usage=Usage(
                input_tokens=int(usage.get("prompt_tokens") or 0),
                output_tokens=int(usage.get("completion_tokens") or 0),
                model=reported_model
                if isinstance(reported_model, str) and reported_model
                else self._model,
                provider=self.name,
            ),
            stop_reason=choice.get("finish_reason"),
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        """Cost at the configured endpoint prices; raises ``UnknownPriceError`` if unpriced."""
        return require_price(self._price, usage.model or self._model, self.name).cost(usage)

    async def aclose(self) -> None:
        """Close the HTTP client if this instance created it."""
        if self._owns_client:
            await self._client.aclose()

    async def __aenter__(self) -> OpenAICompatModel:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        await self.aclose()


def _parse_tool_calls(raw: object) -> list[ToolCall]:
    """Map OpenAI ``tool_calls`` blocks to our ``ToolCall`` (arguments arrive as a JSON string)."""
    if not isinstance(raw, list):
        return []
    calls: list[ToolCall] = []
    for entry in raw:
        if not isinstance(entry, dict):
            continue
        fn = entry.get("function", {})
        args_raw = fn.get("arguments", "{}")
        try:
            arguments = json.loads(args_raw) if isinstance(args_raw, str) else dict(args_raw)
        except (ValueError, TypeError):
            arguments = {"_raw": args_raw}
        if not isinstance(arguments, dict):
            arguments = {"_raw": args_raw}
        calls.append(
            ToolCall(id=str(entry.get("id", "")), name=str(fn.get("name", "")), arguments=arguments)
        )
    return calls
