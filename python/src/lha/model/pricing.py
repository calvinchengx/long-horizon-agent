"""Per-model token pricing (USD per 1M tokens), used only for the cost ledger / budget governor.

Prices are keyed by the model the provider REPORTS as having served the turn. An unknown model is
an error (``UnknownPriceError``) unless explicit prices were configured — cost must never silently
become ``$0``.

Anthropic prompt caching: cache WRITES are billed at 1.25x the input price (5-minute TTL; 2x for the
1-hour TTL) and cache READS at 0.1x the input price. ``input_tokens`` as reported by the Messages
API excludes both, so the three are summed separately.

VERIFY BEFORE RELYING ON THESE NUMBERS: provider prices change. The Claude table below reflects
Anthropic's published first-party rates as last checked (2026-06); partner platforms (Bedrock,
Vertex) price differently. Anything not listed must be configured explicitly.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from lha.contracts.model import UnknownPriceError, Usage

_MTOK = 1_000_000


@dataclass(frozen=True)
class ModelPrice:
    """USD per 1M tokens, plus prompt-cache multipliers relative to the input price."""

    input_per_mtok: float
    output_per_mtok: float
    cache_write_multiplier: float = 1.25  # 5-minute TTL cache write
    cache_write_1h_multiplier: float = 2.0  # 1-hour TTL cache write
    cache_read_multiplier: float = 0.1

    def cost(self, usage: Usage) -> float:
        """USD for ``usage`` under this price."""
        write_1h = min(usage.cache_creation_1h_input_tokens, usage.cache_creation_input_tokens)
        write_5m = usage.cache_creation_input_tokens - write_1h
        input_equiv = (
            usage.input_tokens
            + write_5m * self.cache_write_multiplier
            + write_1h * self.cache_write_1h_multiplier
            + usage.cache_read_input_tokens * self.cache_read_multiplier
        )
        return (
            input_equiv / _MTOK * self.input_per_mtok
            + usage.output_tokens / _MTOK * self.output_per_mtok
        )


# Anthropic first-party rates (see module docstring: verify before relying on them).
CLAUDE_PRICES: dict[str, ModelPrice] = {
    "claude-opus-4-8": ModelPrice(5.0, 25.0),
    "claude-sonnet-4-6": ModelPrice(3.0, 15.0),
    "claude-haiku-4-5": ModelPrice(1.0, 5.0),
}

_DATE_SUFFIX = re.compile(r"-\d{8}$")


def lookup_claude_price(model: str) -> ModelPrice | None:
    """Price for a Claude model id; tolerates a dated snapshot suffix (``...-20251001``)."""
    if model in CLAUDE_PRICES:
        return CLAUDE_PRICES[model]
    return CLAUDE_PRICES.get(_DATE_SUFFIX.sub("", model))


def require_price(price: ModelPrice | None, model: str, provider: str) -> ModelPrice:
    """Return ``price`` or raise ``UnknownPriceError`` naming the unpriced model."""
    if price is None:
        raise UnknownPriceError(
            f"no price configured for model {model!r} on provider {provider!r}; "
            "configure explicit prices (cost is never assumed to be $0)"
        )
    return price
