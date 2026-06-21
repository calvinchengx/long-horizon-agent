"""The model layer contract.

Every LLM backend (stub / Ollama / OpenAI-compatible / Claude) implements ``ModelProvider``.
Calls return a ``TurnResult`` carrying the REAL output (text, reasoning, tool calls) plus the
REAL token ``Usage`` the provider reported, so cost is computed from actual numbers — never
faked. For local/free backends the computed cost is genuinely ``$0`` while tokens/latency are
still real and displayed.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from pydantic import BaseModel, Field


class Usage(BaseModel):
    """Real token accounting as reported by the provider."""

    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_input_tokens: int = 0
    cache_creation_input_tokens: int = 0
    # Subset of ``cache_creation_input_tokens`` written with the 1-hour TTL (billed higher than
    # the default 5-minute TTL). Zero when the provider does not report the breakdown.
    cache_creation_1h_input_tokens: int = 0
    # The model that ACTUALLY served the turn (as reported by the provider) — cost is keyed on it.
    model: str = ""
    # The provider (``ModelProvider.name``) that served the turn; set by failover so cost is
    # computed with the responding provider's prices, not the first provider's.
    provider: str = ""


class UnknownPriceError(ValueError):
    """Raised by ``estimate_cost_usd`` when the provider has no price for the responding model.

    Cost must never silently become ``$0``: callers (the metering wrapper) record the tokens and
    mark the cost as UNKNOWN, and the governor treats unknown spend conservatively.
    """


class ToolCall(BaseModel):
    """A tool invocation the model requested (executed by the dispatcher, not the model)."""

    id: str
    name: str
    arguments: dict[str, object] = Field(default_factory=dict)


class ModelMessage(BaseModel):
    """One message in a model conversation.

    Native tool-use round trips: an ``assistant`` message carries the ``tool_calls`` it made, and
    each result goes back as a ``tool`` message whose ``tool_call_id`` names the call it answers.
    """

    role: str  # "system" | "user" | "assistant" | "tool"
    content: str
    tool_calls: list[ToolCall] = Field(default_factory=list)
    tool_call_id: str | None = None


class TurnResult(BaseModel):
    """The real result of one model turn.

    ``thinking`` holds the reasoning/CoT when the backend exposes it (captured for the
    observability view); ``tool_calls`` are requests for the dispatcher to execute.
    """

    text: str = ""
    thinking: str | None = None
    tool_calls: list[ToolCall] = Field(default_factory=list)
    usage: Usage = Field(default_factory=Usage)
    stop_reason: str | None = None
    session_id: str | None = None


@runtime_checkable
class ModelProvider(Protocol):
    """A pluggable LLM backend. Implementations live in ``src/lha/model/``."""

    name: str

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        """Run one real model turn and return its real output + usage."""
        ...

    def estimate_cost_usd(self, usage: Usage) -> float:
        """Compute real USD cost from real token counts (``0.0`` for local/free backends).

        Raises ``UnknownPriceError`` when the responding model has no configured price.
        """
        ...
