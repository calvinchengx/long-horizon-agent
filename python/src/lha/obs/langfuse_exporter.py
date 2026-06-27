"""Optional Langfuse client wiring.

Returns a configured Langfuse client when host + keys are set and the ``observability`` extra is
installed, else ``None`` (so observability is never a hard dependency). This only constructs the
client: nothing in the codebase mirrors ``TraceRecorder`` events to it automatically. Route traces
to Langfuse via its OTLP endpoint (``lha.obs.otel`` spans) or by calling the client directly.
"""

from __future__ import annotations

from typing import Any

from lha.config import Settings, get_settings


def build_langfuse(settings: Settings | None = None) -> Any | None:
    """Construct a Langfuse client, or ``None`` if not configured / not installed."""
    settings = settings or get_settings()
    if not (
        settings.langfuse_host and settings.langfuse_public_key and settings.langfuse_secret_key
    ):
        return None
    try:
        from langfuse import Langfuse
    except ImportError:
        return None
    return Langfuse(
        host=settings.langfuse_host,
        public_key=settings.langfuse_public_key,
        secret_key=settings.langfuse_secret_key.get_secret_value(),
    )
