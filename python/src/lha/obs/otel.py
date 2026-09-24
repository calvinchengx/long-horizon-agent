"""OpenTelemetry tracing: exporter setup at process start + spans on every run path.

``configure_tracing`` is called once at process start by the CLI (every command, including
``lha worker``) and by ``python -m lha.durable.worker``. It installs a ``TracerProvider`` with an
OTLP/HTTP exporter for each configured backend:

* an OTLP collector at ``LHA_OTEL_EXPORTER_OTLP_ENDPOINT`` (or the standard
  ``OTEL_EXPORTER_OTLP_ENDPOINT``); ``OTEL_EXPORTER_OTLP_HEADERS`` is honoured by the exporter;
* Langfuse, when ``LHA_LANGFUSE_HOST`` + ``LHA_LANGFUSE_PUBLIC_KEY`` + ``LHA_LANGFUSE_SECRET_KEY``
  are all set, through Langfuse's OTLP endpoint (``<host>/api/public/otel``, Basic auth).

Nothing is configured when neither is set, when ``OTEL_SDK_DISABLED=true``, or when the
``observability`` extra (``opentelemetry-sdk`` + ``opentelemetry-exporter-otlp-proto-http``) is
not installed: then every span below is a no-op.

Spans (``span``) are opened around a local or orchestrated mission (``lha.mission``), every agent
cycle (``lha.cycle``), every durable cycle activity (``lha.activity.run_agent_cycle``), every
metered model call (``chat <model>``) and every dispatched tool call (``execute_tool <name>``).
They carry metadata only (ids, model, token counts, verdicts, tool names, ok/error), never
prompts, tool arguments or outputs, and every attribute goes through ``lha.obs.redact`` first.

Tracing never blocks or fails the agent: spans are exported by a background
``BatchSpanProcessor`` (a bounded queue that drops spans when the backend is down), each export
gives up after ``LHA_OTEL_EXPORT_TIMEOUT_S``, and any error raised by the tracing machinery itself
is swallowed. Errors raised by the traced work are recorded on the span and re-raised unchanged.
"""

from __future__ import annotations

import base64
from collections.abc import Iterator, Mapping
from contextlib import AbstractContextManager, contextmanager, suppress
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

import structlog

from lha.config import Settings, get_settings
from lha.obs.redact import redact_mapping, redact_text

if TYPE_CHECKING:
    from lha.contracts.model import ToolCall
    from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult

_log = structlog.get_logger("lha.obs")

# The provider this module installed (``None`` until ``configure_tracing`` exports somewhere).
_provider: Any | None = None


@dataclass(frozen=True)
class ExportTarget:
    """One OTLP/HTTP trace destination."""

    name: str  # "otlp" | "langfuse"
    endpoint: str  # full traces URL (".../v1/traces")
    headers: dict[str, str] | None = None  # None: the exporter reads OTEL_EXPORTER_OTLP_HEADERS


def _traces_url(base: str) -> str:
    base = base.rstrip("/")
    return base if base.endswith("/v1/traces") else f"{base}/v1/traces"


def export_targets(settings: Settings) -> list[ExportTarget]:
    """The trace destinations ``settings`` configures (empty: tracing stays off)."""
    targets: list[ExportTarget] = []
    if settings.otel_exporter_otlp_endpoint:
        targets.append(ExportTarget("otlp", _traces_url(settings.otel_exporter_otlp_endpoint)))
    if settings.langfuse_host and settings.langfuse_public_key and settings.langfuse_secret_key:
        token = base64.b64encode(
            f"{settings.langfuse_public_key}:"
            f"{settings.langfuse_secret_key.get_secret_value()}".encode()
        ).decode()
        targets.append(
            ExportTarget(
                "langfuse",
                _traces_url(f"{settings.langfuse_host.rstrip('/')}/api/public/otel"),
                {"Authorization": f"Basic {token}"},
            )
        )
    return targets


def configure_tracing(
    settings: Settings | None = None, *, component: str = "cli", exporter_factory: Any = None
) -> list[str]:
    """Install a tracer provider exporting to every configured target; return their names.

    Idempotent (a second call keeps the first provider). Returns ``[]`` and changes nothing when
    no target is configured, ``OTEL_SDK_DISABLED`` is true, or the ``observability`` extra is
    missing. ``component`` is recorded as the resource attribute ``lha.component``
    (``cli`` / ``worker``). ``exporter_factory(target, timeout_s)`` replaces
    the OTLP/HTTP exporter (tests).
    """
    global _provider
    settings = settings or get_settings()
    targets = export_targets(settings)
    if not targets or settings.otel_sdk_disabled:
        return []
    if _provider is not None:
        return [t.name for t in targets]
    try:
        from opentelemetry import trace
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
    except ImportError:
        _log.warning(
            "tracing_not_configured",
            reason="the 'observability' extra is not installed (lha[observability])",
            targets=[t.name for t in targets],
        )
        return []
    factory = exporter_factory or _otlp_exporter
    try:
        provider = TracerProvider(
            resource=Resource.create(
                {"service.name": settings.otel_service_name, "lha.component": component}
            )
        )
        for target in targets:
            exporter = factory(target, settings.otel_export_timeout_s)
            provider.add_span_processor(
                BatchSpanProcessor(
                    exporter, export_timeout_millis=settings.otel_export_timeout_s * 1000
                )
            )
        trace.set_tracer_provider(provider)
    except Exception as exc:  # a broken exporter setup must not stop the agent
        _log.warning("tracing_not_configured", reason=f"{type(exc).__name__}: {exc}")
        return []
    _provider = provider
    names = [t.name for t in targets]
    _log.info("tracing_configured", targets=names, component=component)
    return names


def _otlp_exporter(target: ExportTarget, timeout_s: float) -> Any:
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

    return OTLPSpanExporter(endpoint=target.endpoint, headers=target.headers, timeout=timeout_s)


def shutdown_tracing() -> None:
    """Flush and stop the provider ``configure_tracing`` installed (no-op otherwise)."""
    global _provider
    provider, _provider = _provider, None
    if provider is None:
        return
    try:
        provider.shutdown()
    except Exception as exc:
        _log.warning("tracing_shutdown_failed", error=f"{type(exc).__name__}: {exc}")


def _tracer() -> Any | None:
    try:
        from opentelemetry import trace
    except ImportError:
        return None
    return trace.get_tracer("lha")


def _attribute(value: object) -> str | bool | int | float:
    return value if isinstance(value, str | bool | int | float) else str(value)


class SpanHandle:
    """Lets traced code add attributes after the work ran (e.g. token usage, a verdict)."""

    def __init__(self, span: Any | None = None) -> None:
        self._span = span

    def set(self, attributes: Mapping[str, object]) -> None:
        if self._span is None:
            return
        try:
            for key, value in redact_mapping(
                {k: v for k, v in attributes.items() if v is not None}
            ).items():
                self._span.set_attribute(key, _attribute(value))
        except Exception:  # tracing must never fail the traced work
            return

    def error(self, message: str) -> None:
        """Mark the span as failed without an exception (e.g. a tool returned ``ok=False``)."""
        if self._span is None:
            return
        try:
            from opentelemetry.trace import Status, StatusCode

            self._span.set_status(Status(StatusCode.ERROR, redact_text(message)[:200]))
        except Exception:
            return


_NOOP = SpanHandle()


@contextmanager
def span(name: str, attributes: Mapping[str, object] | None = None) -> Iterator[SpanHandle]:
    """Open span ``name`` with redacted ``attributes``; a no-op when OTel is absent.

    Exceptions from the body are recorded on the span and propagate unchanged; failures of the
    tracing machinery itself are swallowed.
    """
    tracer = _tracer()
    manager: AbstractContextManager[Any] | None = None
    handle = _NOOP
    if tracer is not None:
        try:
            manager = tracer.start_as_current_span(name)
            handle = SpanHandle(manager.__enter__())
        except Exception:
            manager, handle = None, _NOOP
    handle.set(attributes or {})
    try:
        yield handle
    except BaseException as exc:
        if manager is not None:
            with suppress(Exception):
                manager.__exit__(type(exc), exc, exc.__traceback__)
        raise
    if manager is not None:
        with suppress(Exception):
            manager.__exit__(None, None, None)


@contextmanager
def agent_span(name: str, **attributes: object) -> Iterator[None]:
    """``span`` with every attribute namespaced ``gen_ai.<key>`` (kept for existing callers)."""
    with span(name, {f"gen_ai.{key}": value for key, value in attributes.items()}):
        yield


async def traced_dispatch(
    dispatcher: ToolDispatcher, call: ToolCall, ctx: ToolContext
) -> ToolResult:
    """``dispatcher.dispatch(call, ctx)`` inside an ``execute_tool <name>`` span.

    Every agent that runs tools (the lead's ``AgentLoop``, sub-agents) dispatches through this,
    so each call, including one the policy refuses, is one span: tool name, call id, mission
    and ok. Arguments and output are not recorded.
    """
    with span(
        f"execute_tool {call.name}",
        {
            "gen_ai.operation.name": "execute_tool",
            "gen_ai.tool.name": call.name,
            "gen_ai.tool.call.id": call.id,
            "lha.mission_id": ctx.mission_id,
        },
    ) as traced:
        result = await dispatcher.dispatch(call, ctx)
        traced.set({"lha.tool.ok": result.ok})
        if not result.ok:
            traced.error(result.error or "tool failed")
        return result
