"""Claude (Anthropic Messages API) model backend.

A direct, dependency-light adapter over the real Messages API: real generated text + tool-use,
real token usage (including prompt-cache read/write tokens), real cost from a per-model price
table keyed by the model the API reports. This is the single-turn ``ModelProvider``; the full
agentic loop with Claude's built-in tools is layered on top via the Claude Agent SDK.

Transient failures (429 / 5xx / 529 overloaded / timeouts / connection errors) are retried a
bounded number of times with exponential backoff honouring ``Retry-After``; 4xx client errors
(bad request, bad key) raise immediately.
"""

from __future__ import annotations

import asyncio
from types import TracebackType

import httpx

from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, TurnResult, Usage
from lha.model.health import ModelHealth, http_failure
from lha.model.pricing import ModelPrice, lookup_claude_price, require_price
from lha.model.retry import Sleep, with_retries


def _to_claude_messages(messages: list[ModelMessage]) -> list[dict[str, object]]:
    """Map neutral messages to Messages-API turns (tool_use / tool_result blocks preserved).

    Consecutive tool results are merged into ONE user turn, as the API expects all results for an
    assistant turn's tool calls together.
    """
    out: list[dict[str, object]] = []
    for m in messages:
        if m.role == "system":
            continue
        if m.role == "assistant" and m.tool_calls:
            blocks: list[dict[str, object]] = []
            if m.content:
                blocks.append({"type": "text", "text": m.content})
            blocks.extend(
                {"type": "tool_use", "id": c.id, "name": c.name, "input": c.arguments}
                for c in m.tool_calls
            )
            out.append({"role": "assistant", "content": blocks})
        elif m.role == "tool" and m.tool_call_id:
            block: dict[str, object] = {
                "type": "tool_result",
                "tool_use_id": m.tool_call_id,
                "content": m.content,
            }
            prev = out[-1] if out else None
            prev_content = prev.get("content") if prev is not None else None
            if (
                prev is not None
                and prev.get("role") == "user"
                and isinstance(prev_content, list)
                and all(
                    isinstance(b, dict) and b.get("type") == "tool_result" for b in prev_content
                )
            ):
                prev_content.append(block)
            else:
                out.append({"role": "user", "content": [block]})
        elif m.role == "tool":
            # A tool result with no call id cannot be a tool_result block; send it as plain text.
            out.append({"role": "user", "content": f"TOOL RESULT:\n{m.content}"})
        else:
            out.append(
                {"role": "assistant" if m.role == "assistant" else "user", "content": m.content}
            )
    return out


class ClaudeModel(ModelProvider):
    """A ``ModelProvider`` backed by Anthropic's Messages API."""

    ENDPOINT = "https://api.anthropic.com/v1/messages"
    MODELS_ENDPOINT = "https://api.anthropic.com/v1/models"
    API_VERSION = "2023-06-01"

    def __init__(
        self,
        *,
        api_key: str,
        model_name: str,
        default_max_tokens: int = 4096,
        timeout_s: float = 300.0,
        client: httpx.AsyncClient | None = None,
        price: ModelPrice | None = None,
        max_retries: int = 3,
        retry_base_delay_s: float = 1.0,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        # Unknown model => fail fast unless explicit prices are configured (never silently $0).
        self._price = price
        require_price(price or lookup_claude_price(model_name), model_name, "claude")
        self.name = f"claude:{model_name}"
        self._api_key = api_key
        self._model = model_name
        self.default_max_tokens = default_max_tokens
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
        # Anthropic takes the system prompt separately from the conversation turns.
        system = "\n\n".join(m.content for m in messages if m.role == "system")
        payload: dict[str, object] = {
            "model": self._model,
            "max_tokens": max_tokens or self.default_max_tokens,
            "messages": _to_claude_messages(messages),
        }
        if system:
            payload["system"] = system
        if tools:
            payload["tools"] = tools

        async def _post() -> httpx.Response:
            resp = await self._client.post(
                self.ENDPOINT,
                json=payload,
                headers={
                    "x-api-key": self._api_key,
                    "anthropic-version": self.API_VERSION,
                    "content-type": "application/json",
                },
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

        blocks = data.get("content", [])
        text = "".join(b.get("text", "") for b in blocks if b.get("type") == "text")
        tool_calls = [
            ToolCall(
                id=str(b.get("id", "")), name=str(b.get("name", "")), arguments=b.get("input", {})
            )
            for b in blocks
            if b.get("type") == "tool_use"
        ]
        usage = data.get("usage") or {}
        cache_breakdown = usage.get("cache_creation") or {}
        reported_model = data.get("model")

        return TurnResult(
            text=text,
            tool_calls=tool_calls,
            usage=Usage(
                input_tokens=int(usage.get("input_tokens") or 0),
                output_tokens=int(usage.get("output_tokens") or 0),
                cache_read_input_tokens=int(usage.get("cache_read_input_tokens") or 0),
                cache_creation_input_tokens=int(usage.get("cache_creation_input_tokens") or 0),
                cache_creation_1h_input_tokens=int(
                    cache_breakdown.get("ephemeral_1h_input_tokens") or 0
                )
                if isinstance(cache_breakdown, dict)
                else 0,
                model=reported_model
                if isinstance(reported_model, str) and reported_model
                else self._model,
                provider=self.name,
            ),
            stop_reason=data.get("stop_reason"),
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        """Price by the model the API reported (falls back to the configured model if unset)."""
        model = usage.model or self._model
        # Explicit prices win for the configured model; any other reported model uses the table.
        explicit = self._price if model == self._model else None
        price = explicit or lookup_claude_price(model) or self._price
        return require_price(price, model, self.name).cost(usage)

    async def health_check(self, *, timeout_s: float) -> ModelHealth:
        """``GET /v1/models/<model>``: proves the API is up, the key works and the model exists,
        without spending tokens."""
        try:
            resp = await self._client.get(
                f"{self.MODELS_ENDPOINT}/{self._model}",
                headers={"x-api-key": self._api_key, "anthropic-version": self.API_VERSION},
                timeout=timeout_s,
            )
            resp.raise_for_status()
        except httpx.HTTPError as exc:
            return ModelHealth(False, f"{self.name}: {http_failure(exc).detail}")
        return ModelHealth(True, f"{self.name}: reachable")

    async def aclose(self) -> None:
        """Close the HTTP client if this instance created it."""
        if self._owns_client:
            await self._client.aclose()

    async def __aenter__(self) -> ClaudeModel:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        await self.aclose()
