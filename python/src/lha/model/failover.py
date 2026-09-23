"""Provider failover (AI-gateway resilience).

Wraps an ordered list of providers. On a TRANSIENT error from one (429, 5xx, timeout, connection
error) it falls through to the next; after a full round of transient failures it backs off
(exponentially, honouring ``Retry-After``) and tries again, up to ``max_rounds``. Non-transient
errors (400 bad request, 401/403 auth, programming errors) raise immediately: failing over on them
would only mask a misconfiguration.

Cost is computed by the provider that actually served the turn (``Usage.provider``), never by the
first provider's price table.
"""

from __future__ import annotations

import asyncio

from lha.contracts.model import (
    ModelMessage,
    ModelProvider,
    TurnResult,
    UnknownPriceError,
    Usage,
)
from lha.model.health import ModelHealth, probe_provider
from lha.model.retry import Sleep, backoff_delay, is_retryable


class FailoverModel(ModelProvider):
    """Try providers in order on transient errors; raise immediately on non-transient ones."""

    def __init__(
        self,
        providers: list[ModelProvider],
        *,
        max_rounds: int = 2,
        base_delay_s: float = 1.0,
        max_delay_s: float = 60.0,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        if not providers:
            raise ValueError("FailoverModel needs at least one provider")
        if max_rounds < 1:
            raise ValueError("max_rounds must be >= 1")
        self._providers = providers
        self._max_rounds = max_rounds
        self._base_delay_s = base_delay_s
        self._max_delay_s = max_delay_s
        self._sleep = sleep
        self.name = "failover:" + ",".join(p.name for p in providers)
        self.default_max_tokens = max(
            (getattr(p, "default_max_tokens", 0) or 0 for p in providers), default=0
        )

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        last_error: Exception | None = None
        for round_no in range(self._max_rounds):
            if round_no > 0:
                await self._sleep(
                    backoff_delay(
                        round_no - 1, last_error, base_s=self._base_delay_s, max_s=self._max_delay_s
                    )
                )
            for provider in self._providers:
                try:
                    result = await provider.complete(messages, tools=tools, max_tokens=max_tokens)
                except Exception as exc:
                    if not is_retryable(exc):
                        raise
                    last_error = exc
                    continue
                if not result.usage.provider:
                    result.usage.provider = provider.name
                return result
        assert last_error is not None  # every provider was attempted at least once
        raise last_error

    def _provider_for(self, usage: Usage) -> ModelProvider:
        for provider in self._providers:
            if provider.name == usage.provider:
                return provider
        if len(self._providers) == 1:
            return self._providers[0]
        raise UnknownPriceError(
            f"cannot attribute usage (provider={usage.provider!r}, model={usage.model!r}) to any "
            f"provider of {self.name}"
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        """Priced by the provider that served the turn."""
        if not usage.provider:
            # Pre-call worst-case estimate: the most expensive provider bounds it.
            return max(p.estimate_cost_usd(usage) for p in self._providers)
        return self._provider_for(usage).estimate_cost_usd(usage)

    async def health_check(self, *, timeout_s: float) -> ModelHealth:
        """Healthy if ANY member can serve (failover routes around the others)."""
        results = await asyncio.gather(
            *(probe_provider(p, timeout_s=timeout_s) for p in self._providers)
        )
        detail = "; ".join(r.detail for r in results if r.detail)
        return ModelHealth(any(r.ok for r in results), detail)

    async def aclose(self) -> None:
        """Close every wrapped provider that owns resources."""
        for provider in self._providers:
            close = getattr(provider, "aclose", None)
            if close is not None:
                await close()
