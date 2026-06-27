"""Structured logging + an in-memory trace recorder.

Every meaningful step emits a ``TraceEvent`` (cycle_started, llm_turn, tool_call, verify,
checkpoint, error, ...). Events go to structlog (human console or JSON) and are collected so a
run can be replayed/inspected — this is what powers the "watch it think, act, and spend" view and
the audit trail. Event data is passed through ``lha.obs.redact`` first, so secret-looking keys and
values never reach logs or the collected trace.

The recorder does NOT export to Langfuse (or any other backend) itself;
``lha.obs.langfuse_exporter`` only builds a client. OTel spans (``lha.obs.otel``) are the
supported path to external tracing.
"""

from __future__ import annotations

from typing import Any

import structlog
from pydantic import BaseModel, Field

from lha.obs.redact import redact_mapping


def configure_logging(*, json_logs: bool = False) -> None:
    """Configure process-wide structlog output (console for dev, JSON for prod/ingest)."""
    renderer: Any = (
        structlog.processors.JSONRenderer()
        if json_logs
        else structlog.dev.ConsoleRenderer(colors=False)
    )
    processors: list[Any] = [
        structlog.processors.add_log_level,
        structlog.processors.TimeStamper(fmt="iso"),
        renderer,
    ]
    structlog.configure(processors=processors, cache_logger_on_first_use=True)


def get_logger(name: str = "lha") -> Any:
    """Return a bound structlog logger."""
    return structlog.get_logger(name)


class TraceEvent(BaseModel):
    """One observable step in a mission."""

    kind: str
    mission_id: str
    cycle_id: str = ""
    data: dict[str, object] = Field(default_factory=dict)


class TraceRecorder:
    """Collects ``TraceEvent``s and mirrors them to structlog."""

    def __init__(self, *, logger_name: str = "lha") -> None:
        self._log = structlog.get_logger(logger_name)
        self.events: list[TraceEvent] = []

    def record(
        self, kind: str, *, mission_id: str, cycle_id: str = "", **data: object
    ) -> TraceEvent:
        safe = redact_mapping(data)
        event = TraceEvent(kind=kind, mission_id=mission_id, cycle_id=cycle_id, data=safe)
        self.events.append(event)
        self._log.info(kind, mission_id=mission_id, cycle_id=cycle_id, **safe)
        return event

    def to_jsonl(self) -> str:
        """Serialize the collected trace as newline-delimited JSON (for inspection/export)."""
        return "\n".join(event.model_dump_json() for event in self.events)
