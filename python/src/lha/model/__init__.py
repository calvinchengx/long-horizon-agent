"""The pluggable model layer.

One ``ModelProvider`` interface, many backends. ``build_provider`` selects the concrete
backend from ``Settings.model_backend`` so the rest of the system never imports a specific LLM.

- ``stub``         -> StubModel (deterministic; tests/CI only, never shown as a real run)
- ``ollama``       -> local models via Ollama's OpenAI-compatible API ($0/offline)
- ``openai_compat``-> any OpenAI-compatible endpoint (Groq/Gemini/OpenRouter, ...)
- ``claude``       -> the Anthropic Messages API

``LHA_FALLBACK_MODELS`` turns the result into a ``FailoverModel`` (primary, then each fallback).
``lha.model.health.probe_model`` contacts the configured provider(s) cheaply (health probe)."""

from __future__ import annotations

from dataclasses import dataclass

import httpx

from lha.config import Settings, get_settings
from lha.contracts.model import ModelProvider
from lha.model.claude import ClaudeModel
from lha.model.failover import FailoverModel
from lha.model.openai_compat import OpenAICompatModel
from lha.model.pricing import ModelPrice
from lha.model.stub import StubModel
from lha.model.tool_schemas import to_claude_tools, to_openai_tools


def secret_value(value: object) -> str | None:
    """Unwrap a settings secret that may be a plain ``str`` or a pydantic ``SecretStr``."""
    if value is None:
        return None
    getter = getattr(value, "get_secret_value", None)
    raw = getter() if callable(getter) else value
    return str(raw) if raw else None


_BACKENDS = ("stub", "ollama", "openai_compat", "claude")
# Inside a failover chain each member retries a transient error once before the chain moves on,
# so an outage fails over in seconds instead of after the full per-provider backoff.
CHAIN_MEMBER_RETRIES = 1


@dataclass(frozen=True)
class FallbackSpec:
    """One ``LHA_FALLBACK_MODELS`` entry: ``backend:model[@in/out]`` (USD per 1M tokens)."""

    backend: str
    model: str
    price: ModelPrice | None = None


def parse_fallback_entry(entry: str) -> FallbackSpec:
    """Parse ``backend:model[@in/out]``; the model part may itself contain ``:`` (``qwen3:8b``)."""
    backend, sep, rest = entry.strip().partition(":")
    backend = backend.strip().lower()
    if not sep or backend not in _BACKENDS:
        raise ValueError(
            f"invalid LHA_FALLBACK_MODELS entry {entry!r}: expected 'backend:model[@in/out]' "
            f"with backend one of {', '.join(_BACKENDS)}"
        )
    model, at, price_text = rest.rpartition("@") if "@" in rest else (rest, "", "")
    price: ModelPrice | None = None
    if at:
        price_in, slash, price_out = price_text.partition("/")
        try:
            price = ModelPrice(float(price_in), float(price_out)) if slash else None
        except ValueError:
            price = None
        if price is None or price.input_per_mtok < 0 or price.output_per_mtok < 0:
            raise ValueError(
                f"invalid price in LHA_FALLBACK_MODELS entry {entry!r}: expected '@<in>/<out>' "
                "USD per 1M tokens"
            )
    model = model.strip()
    if not model:
        raise ValueError(f"invalid LHA_FALLBACK_MODELS entry {entry!r}: empty model name")
    return FallbackSpec(backend=backend, model=model, price=price)


def _build_backend(
    settings: Settings,
    backend: str,
    name: str,
    *,
    client: httpx.AsyncClient | None,
    price: ModelPrice | None,
    max_retries: int,
) -> ModelProvider:
    """One concrete backend. ``price`` (explicit) wins over table / settings prices."""
    if backend == "stub":
        return StubModel(model_name=name)

    if backend == "ollama":
        # Ollama exposes an OpenAI-compatible API at <base>/v1; local, so genuinely $0.
        return OpenAICompatModel(
            base_url=f"{settings.ollama_base_url.rstrip('/')}/v1",
            model_name=name,
            api_key="ollama",  # Ollama ignores it but the OpenAI shape expects a bearer.
            label="ollama",
            price_in_per_mtok=0.0,
            price_out_per_mtok=0.0,
            client=client,
            max_retries=max_retries,
        )

    if backend == "openai_compat":
        if not settings.openai_base_url:
            raise ValueError("LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend.")
        # Prices are optional; unset => cost is UNKNOWN (never silently $0).
        return OpenAICompatModel(
            base_url=settings.openai_base_url,
            model_name=name,
            api_key=secret_value(settings.openai_api_key),
            price_in_per_mtok=price.input_per_mtok if price else None,
            price_out_per_mtok=price.output_per_mtok if price else None,
            client=client,
            max_retries=max_retries,
        )

    if backend == "claude":
        api_key = secret_value(settings.anthropic_api_key)
        if not api_key:
            raise ValueError("LHA_ANTHROPIC_API_KEY is required for the 'claude' backend.")
        return ClaudeModel(
            api_key=api_key, model_name=name, client=client, price=price, max_retries=max_retries
        )

    raise ValueError(f"Unknown model backend: {backend!r}")


def _primary_price(settings: Settings, backend: str, name: str) -> ModelPrice | None:
    """The explicit price settings give the PRIMARY backend (``LHA_OPENAI_/CLAUDE_PRICE_*``)."""
    if backend == "openai_compat":
        price_in, price_out = settings.openai_price_in_per_mtok, settings.openai_price_out_per_mtok
        if (price_in is None) != (price_out is None):
            raise ValueError("configure both price_in_per_mtok and price_out_per_mtok, or neither")
        if price_in is not None and price_out is not None:
            return ModelPrice(price_in, price_out)
    if backend == "claude":
        price_in, price_out = settings.claude_price_in_per_mtok, settings.claude_price_out_per_mtok
        # Explicit prices describe the configured model only, not per-role overrides.
        if price_in is not None and price_out is not None and name == settings.model_name:
            return ModelPrice(price_in, price_out)
    return None


def build_provider(
    settings: Settings | None = None,
    *,
    model_name: str | None = None,
    client: httpx.AsyncClient | None = None,
) -> ModelProvider:
    """Construct the configured model backend (the only place a concrete LLM is chosen).

    ``model_name`` overrides ``settings.model_name`` (per-role routing) for the PRIMARY model;
    ``client`` lets several providers share one HTTP connection pool (the caller then owns
    closing it). With ``LHA_FALLBACK_MODELS`` set, returns a ``FailoverModel`` over the primary
    followed by each fallback in order; every turn is priced by the provider that served it.
    """
    settings = settings or get_settings()
    backend = settings.model_backend
    name = model_name or settings.model_name
    fallbacks = [parse_fallback_entry(e) for e in settings.fallback_model_entries()]
    retries = CHAIN_MEMBER_RETRIES if fallbacks else 3
    primary = _build_backend(
        settings,
        backend,
        name,
        client=client,
        price=_primary_price(settings, backend, name),
        max_retries=retries,
    )
    if not fallbacks:
        return primary
    chain = [primary]
    for spec in fallbacks:
        chain.append(
            _build_backend(
                settings,
                spec.backend,
                spec.model,
                client=client,
                price=spec.price,
                max_retries=retries,
            )
        )
    return FailoverModel(chain, max_rounds=settings.fallback_max_rounds)


__all__ = [
    "CHAIN_MEMBER_RETRIES",
    "ClaudeModel",
    "FailoverModel",
    "FallbackSpec",
    "ModelPrice",
    "OpenAICompatModel",
    "StubModel",
    "build_provider",
    "parse_fallback_entry",
    "secret_value",
    "to_claude_tools",
    "to_openai_tools",
]
