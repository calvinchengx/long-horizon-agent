"""OpenTelemetry GenAI spans (optional).

A thin helper that emits spans following the GenAI semantic conventions when OpenTelemetry is
installed (the ``observability`` extra), and is a no-op otherwise — so instrumentation never
becomes a hard dependency. Wire an OTLP exporter (→ Langfuse/Tempo/Datadog) at the process edge.
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any

from lha.obs.redact import redact_mapping


def _tracer() -> Any | None:
    try:
        from opentelemetry import trace
    except ImportError:
        return None
    return trace.get_tracer("lha")


@contextmanager
def agent_span(name: str, **attributes: object) -> Iterator[None]:
    """Open a GenAI span named ``name`` with ``gen_ai.*`` attributes (no-op if OTel is absent).

    Attributes are redacted (``lha.obs.redact``) before they are attached to the span.
    """
    tracer = _tracer()
    if tracer is None:
        yield
        return
    with tracer.start_as_current_span(name) as span:
        for key, value in redact_mapping(attributes).items():
            safe = value if isinstance(value, str | bool | int | float) else str(value)
            span.set_attribute(f"gen_ai.{key}", safe)
        yield
