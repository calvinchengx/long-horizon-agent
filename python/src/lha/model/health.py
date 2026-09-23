"""Cheap, real model-provider health probes (used by a parked durable mission).

Building a provider only proves the configuration parses; it says nothing about whether the model
can serve a turn. ``probe_model`` builds the configured provider and CONTACTS it with the cheapest
request each backend offers, under a tight timeout, without spending tokens:

| provider                 | probe                                          |
|--------------------------|------------------------------------------------|
| ``StubModel``            | always healthy (in-process)                    |
| Ollama                   | ``GET <base>/api/tags``; the model must be pulled |
| OpenAI-compatible        | ``GET <base>/models`` (with the bearer key)    |
| ``ClaudeModel``          | ``GET /v1/models/<model>`` (with the API key)  |
| ``FailoverModel``        | healthy if ANY member is healthy               |

Any transport error, timeout or non-2xx answer (including 401/403: a bad key cannot serve turns
either) is reported as DOWN with the reason, so the workflow keeps the mission parked.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import TYPE_CHECKING

import httpx

if TYPE_CHECKING:
    from lha.config import Settings


@dataclass(frozen=True)
class ModelHealth:
    """The outcome of one probe: ``ok`` plus a short human-readable detail."""

    ok: bool
    detail: str = ""


def http_failure(exc: BaseException) -> ModelHealth:
    """Map a probe exception to a DOWN result with a compact reason."""
    if isinstance(exc, httpx.HTTPStatusError):
        return ModelHealth(False, f"HTTP {exc.response.status_code} from {exc.request.url}")
    if isinstance(exc, httpx.TimeoutException | TimeoutError):
        return ModelHealth(False, f"timed out ({type(exc).__name__})")
    return ModelHealth(False, f"{type(exc).__name__}: {exc}")


async def probe_provider(provider: object, *, timeout_s: float) -> ModelHealth:
    """Run ``provider.health_check`` (bounded by ``timeout_s``); DOWN on any error.

    A provider without ``health_check`` cannot be contacted cheaply, so it is reported healthy
    only as far as it could be built (the detail says so).
    """
    check = getattr(provider, "health_check", None)
    if check is None:
        name = getattr(provider, "name", type(provider).__name__)
        return ModelHealth(True, f"{name}: no probe available (built only)")
    try:
        return await asyncio.wait_for(check(timeout_s=timeout_s), timeout=timeout_s + 1.0)
    except Exception as exc:  # the probe must never raise into the health activity
        return http_failure(exc)


async def probe_model(
    settings: Settings,
    *,
    timeout_s: float | None = None,
    client: httpx.AsyncClient | None = None,
) -> ModelHealth:
    """Build the configured provider (primary + fallbacks) and contact it cheaply."""
    from lha.model import build_provider

    timeout = timeout_s if timeout_s is not None else settings.model_probe_timeout_s
    try:
        provider = build_provider(settings, client=client)
    except Exception as exc:  # configuration problems surface here
        return ModelHealth(False, f"{type(exc).__name__}: {exc}")
    try:
        return await probe_provider(provider, timeout_s=timeout)
    finally:
        close = getattr(provider, "aclose", None)
        if close is not None:
            await close()
