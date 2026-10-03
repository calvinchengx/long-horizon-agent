"""Structured logging + an in-memory trace recorder.

Every meaningful step emits a ``TraceEvent`` (cycle_started, llm_turn, tool_call, verify,
checkpoint, error, ...). Events go to structlog (human console or JSON) and are collected so a
run can be replayed/inspected — this is what powers the "watch it think, act, and spend" view and
the audit trail. Event data is passed through ``lha.obs.redact`` first, so secret-looking keys and
values never reach logs or the collected trace.

The recorder does NOT export to any backend itself; external tracing (an OTLP collector, or
Langfuse through its OTLP endpoint) is the OpenTelemetry spans of ``lha.obs.otel``.
"""

from __future__ import annotations

import json
import os
from collections.abc import Callable
from pathlib import Path
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


#: Events a ``TraceRecorder`` keeps in memory: a long local mission records events for weeks, and
#: every one is also logged (structlog) as it happens, so only the newest are kept.
MAX_TRACE_EVENTS = 10_000


class TraceRecorder:
    """Collects the newest ``max_events`` ``TraceEvent``s and mirrors every one to structlog."""

    def __init__(self, *, logger_name: str = "lha", max_events: int = MAX_TRACE_EVENTS) -> None:
        self._log = structlog.get_logger(logger_name)
        self._max = max_events
        self.events: list[TraceEvent] = []
        self.dropped = 0  # events no longer in ``events``
        # Called with every recorded event, after redaction (the mission's shared event record,
        # ``lha.persistence.event_log``). A listener that raises is logged, never propagated.
        self.listeners: list[Callable[[TraceEvent], None]] = []
        # LHA_TRACE_AUDIT_DIR: also append every event to a file there (``lha.obs.event_schema``).
        audit_dir = os.environ.get("LHA_TRACE_AUDIT_DIR", "")
        self._audit = Path(audit_dir) / f"python-{os.getpid()}.ndjson" if audit_dir else None

    def record(
        self, kind: str, *, mission_id: str, cycle_id: str = "", **data: object
    ) -> TraceEvent:
        safe = redact_mapping(data)
        event = TraceEvent(kind=kind, mission_id=mission_id, cycle_id=cycle_id, data=safe)
        self.events.append(event)
        if len(self.events) > self._max + self._max // 10:  # trim in batches, not per event
            excess = len(self.events) - self._max
            del self.events[:excess]
            self.dropped += excess
        self._log.info(kind, mission_id=mission_id, cycle_id=cycle_id, **safe)
        if self._audit is not None:
            self._append_audit(self._audit, event)
        for listener in self.listeners:
            try:
                listener(event)
            except Exception as exc:  # observing a run must never fail it
                self._log.warning("trace_listener_failed", error=f"{type(exc).__name__}: {exc}")
        return event

    def _append_audit(self, path: Path, event: TraceEvent) -> None:
        line = json.dumps(event.model_dump(), sort_keys=True, default=str)
        try:
            with path.open("a", encoding="utf-8") as out:
                out.write(line + "\n")
        except OSError as exc:
            self._log.warning("trace_audit_failed", error=f"{type(exc).__name__}: {exc}")

    def to_jsonl(self) -> str:
        """Serialize the collected trace as newline-delimited JSON (for inspection/export)."""
        return "\n".join(event.model_dump_json() for event in self.events)
