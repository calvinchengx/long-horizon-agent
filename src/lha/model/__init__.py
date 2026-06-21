"""The pluggable model layer.

One ``ModelProvider`` interface, many backends. ``build_provider`` selects the concrete
backend from ``Settings.model_backend`` so the rest of the system never imports a specific LLM.

- ``stub``         -> StubModel (deterministic; tests/CI only, never shown as a real run)
- ``ollama``       -> local models, $0/offline                       (added in a later phase)
- ``openai_compat``-> free-tier cloud (Groq/Gemini/OpenRouter, ...)  (added in a later phase)
- ``claude``       -> Claude API / Agent SDK                          (added in a later phase)
"""

from __future__ import annotations

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


def build_provider(
    settings: Settings | None = None,
    *,
    model_name: str | None = None,
    client: httpx.AsyncClient | None = None,
) -> ModelProvider:
    """Construct the configured model backend (the only place a concrete LLM is chosen).

    ``model_name`` overrides ``settings.model_name`` (per-role routing); ``client`` lets several
    providers share one HTTP connection pool (the caller then owns closing it).
    """
    settings = settings or get_settings()
    backend = settings.model_backend
    name = model_name or settings.model_name

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
        )

    if backend == "openai_compat":
        if not settings.openai_base_url:
            raise ValueError("LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend.")
        # Prices are optional settings; unset => cost is UNKNOWN (never silently $0).
        return OpenAICompatModel(
            base_url=settings.openai_base_url,
            model_name=name,
            api_key=secret_value(settings.openai_api_key),
            price_in_per_mtok=settings.openai_price_in_per_mtok,
            price_out_per_mtok=settings.openai_price_out_per_mtok,
            client=client,
        )

    if backend == "claude":
        api_key = secret_value(settings.anthropic_api_key)
        if not api_key:
            raise ValueError("LHA_ANTHROPIC_API_KEY is required for the 'claude' backend.")
        price_in = settings.claude_price_in_per_mtok
        price_out = settings.claude_price_out_per_mtok
        # Explicit prices describe the configured model only, not per-role overrides.
        price = (
            ModelPrice(price_in, price_out)
            if price_in is not None and price_out is not None and name == settings.model_name
            else None
        )
        return ClaudeModel(api_key=api_key, model_name=name, client=client, price=price)

    raise ValueError(f"Unknown model backend: {backend!r}")


__all__ = [
    "ClaudeModel",
    "FailoverModel",
    "ModelPrice",
    "OpenAICompatModel",
    "StubModel",
    "build_provider",
    "secret_value",
    "to_claude_tools",
    "to_openai_tools",
]
