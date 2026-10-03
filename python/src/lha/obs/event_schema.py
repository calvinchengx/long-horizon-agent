"""The contract for trace events: every kind a run records and the fields its payload carries.

Each kind's payload is a JSON Schema (a small subset: ``type``, ``enum``, ``properties``,
``required``, ``additionalProperties``, ``items``). The schemas are exported to
``spec/obs/mission_events.json``, where every implementation checks the events it records against
them and the UI API (``spec/serve/``) takes its event types from them (docs/27-mission-ui.md).

A payload is checked as it is stored: after redaction and a JSON round trip, so a JSON
``integer`` is a number written without a fraction or exponent.

``LHA_TRACE_AUDIT_DIR`` makes every ``TraceRecorder`` also append each event it records to
``<dir>/python-<pid>.ndjson``; ``audit`` then checks a directory of such files, from any
implementation. The test suites use it to prove that every event they record matches its schema
and that every kind is recorded at least once. Kinds starting with ``test_`` are test fixtures and
are not checked.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

#: The ``schema_version`` of a ``mission_events`` row written with these schemas.
SCHEMA_VERSION = 1
#: The environment variable naming a directory to append every recorded event to.
AUDIT_DIR_ENV = "LHA_TRACE_AUDIT_DIR"
#: Kinds with this prefix are test fixtures: ``audit`` skips them.
TEST_KIND_PREFIX = "test_"

JSONSchema = dict[str, Any]


def _payload(
    required: dict[str, JSONSchema], optional: dict[str, JSONSchema] | None = None
) -> JSONSchema:
    """An object schema with exactly these fields (``optional`` ones may be absent)."""
    return {
        "type": "object",
        "properties": {**required, **(optional or {})},
        "required": sorted(required),
        "additionalProperties": False,
    }


STRING: JSONSchema = {"type": "string"}
INTEGER: JSONSchema = {"type": "integer"}
NUMBER: JSONSchema = {"type": "number"}
BOOLEAN: JSONSchema = {"type": "boolean"}


def _enum(*values: str) -> JSONSchema:
    return {"type": "string", "enum": list(values)}


def _array(items: JSONSchema) -> JSONSchema:
    return {"type": "array", "items": items}


VERDICT = _enum("passed", "failed", "unverified")

REASON = _payload({"reason": STRING})
ITEM = _payload({"item": STRING})
_CHECK = _payload(
    {
        "name": STRING,
        "passed": BOOLEAN,
        "exit_code": INTEGER,
        "gating": BOOLEAN,
        "timed_out": BOOLEAN,
        "duration_s": NUMBER,
    }
)
_QUARANTINE = _payload({"check": STRING, "revision": STRING, "passes": INTEGER, "fails": INTEGER})

#: Every event kind and its payload schema (docs/16-observability.md describes each field).
EVENT_KINDS: dict[str, JSONSchema] = {
    # --- a cycle (AgentLoop) ---------------------------------------------------------------------
    "cycle_started": _payload({"item_id": STRING}),
    "code_map": _payload(
        {"item_id": STRING, "ok": BOOLEAN, "mode": _enum("trace", "task"), "duration_s": NUMBER},
        # exit_code, timed_out and bytes only when the command ran; error only when not ok;
        # fell_back only in task mode.
        {
            "exit_code": INTEGER,
            "timed_out": BOOLEAN,
            "bytes": INTEGER,
            "error": STRING,
            "fell_back": BOOLEAN,
        },
    ),
    "llm_turn": _payload({"model": STRING, "output_tokens": INTEGER, "stop_reason": STRING}),
    # role only from a sub-agent; error only when the call failed.
    "tool_call": _payload({"tool": STRING, "ok": BOOLEAN}, {"role": STRING, "error": STRING}),
    "invalid_reply": REASON,
    "turns_exhausted": _payload({"max_turns": INTEGER, "tool_calls": INTEGER}),
    # spent_usd is null while a model the session used has no price.
    "session_progress": _payload(
        {
            "turns": INTEGER,
            "tool_calls": INTEGER,
            "tool": STRING,
            "spent_usd": {"type": ["number", "null"]},
        }
    ),
    "claude_code_session": _payload(
        {"turns": INTEGER, "tool_calls": INTEGER, "session_id": STRING, "stopped": STRING}
    ),
    "verify": _payload(
        {"trigger": _enum("done", "tool", "cycle"), "verdict": VERDICT, "checks": _array(_CHECK)}
    ),
    "checkpoint": _payload(
        {"head_sha": STRING, "verified": BOOLEAN, "verdict": VERDICT, "peak_rss_mb": NUMBER}
    ),
    "system_one": _payload(
        {
            "use": STRING,
            "item_id": STRING,
            "model": STRING,
            "answer": STRING,
            "confidence": NUMBER,
            "probabilities": {"type": "object", "additionalProperties": NUMBER},
            "threshold": NUMBER,
            "action": _enum("continue", "split", "block"),
            "error": STRING,
        }
    ),
    "check_quarantined": _QUARANTINE,
    "quarantined_check_failed": _QUARANTINE,
    "sandbox_egress": _payload(
        {
            "decision": _enum("allow", "deny", "fail"),
            "method": STRING,
            "host": STRING,
            "port": INTEGER,
            "detail": STRING,
            "count": INTEGER,
        }
    ),
    # --- a run (runner, orchestrator) --------------------------------------------------------------
    "governor_block": REASON,
    "decision_chain_invalid": REASON,
    "model_unavailable": REASON,
    "deadlocked": REASON,
    "loop_detected": _payload({"item_id": STRING}),
    # --- an organization (orchestrator) ---------------------------------------------------------
    "resumed": _payload({"run": INTEGER, "cycle_offset": INTEGER, "board": INTEGER}),
    "ownership_released": _payload({"paths": _array(STRING)}),
    "ownership_violation": _payload({"writer": STRING, "paths": _array(STRING)}),
    "research": _payload({"item": STRING, "n": INTEGER, "failed": INTEGER}),
    "research_failed": _payload({"item": STRING, "error": STRING}),
    "reflection": ITEM,
    "review_screen": _payload({"item": STRING, "findings": INTEGER, "forced": BOOLEAN}),
    "review": _payload(
        {"item": STRING, "blocking": BOOLEAN, "verdict": _enum("approve", "block", "unparsed")}
    ),
    "review_reopened": ITEM,
    "parallel_wave": _payload({"items": _array(STRING), "base": STRING}),
    "lease": _payload({"writer": STRING, "path": STRING, "granted": BOOLEAN, "why": STRING}),
    "integration": _payload(
        {"item": STRING, "merged": BOOLEAN, "branch": STRING, "reason": STRING}
    ),
    # --- memory -----------------------------------------------------------------------------------
    "memory_error": _payload({"where": STRING, "error": STRING}),
    "memory_degraded": REASON,
    "skill_stored": _payload({"skill_id": STRING}),
    "memory_reembedded": _payload(
        {"count": INTEGER, "embedding_model": STRING, "embedding_version": STRING}
    ),
    "memory_consolidated": _payload(
        {"episodes": INTEGER, "facts": INTEGER, "mode": _enum("model", "extractive")}
    ),
}


# --- checking -----------------------------------------------------------------------------------
def _type_ok(kind: str, value: object) -> bool:
    if kind == "null":
        return value is None
    if kind == "boolean":
        return isinstance(value, bool)
    if kind == "string":
        return isinstance(value, str)
    if kind == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if kind == "number":
        return (
            isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)
        )
    if kind == "array":
        return isinstance(value, list)
    if kind == "object":
        return isinstance(value, dict)
    raise ValueError(f"unsupported schema type {kind!r}")


def schema_errors(schema: JSONSchema, value: object, path: str = "$") -> list[str]:
    """Where ``value`` (decoded JSON) breaks ``schema``; empty when it conforms."""
    kinds = schema.get("type")
    if kinds is not None:
        allowed = kinds if isinstance(kinds, list) else [kinds]
        if not any(_type_ok(k, value) for k in allowed):
            return [f"{path}: expected {' or '.join(allowed)}, got {_json_type(value)}"]
    if "enum" in schema and value not in schema["enum"]:
        return [f"{path}: {value!r} is not one of {schema['enum']}"]
    errors: list[str] = []
    if isinstance(value, dict):
        props: dict[str, JSONSchema] = schema.get("properties", {})
        for name in schema.get("required", []):
            if name not in value:
                errors.append(f"{path}: missing {name!r}")
        extra = schema.get("additionalProperties", True)
        for name, item in value.items():
            if name in props:
                errors.extend(schema_errors(props[name], item, f"{path}.{name}"))
            elif extra is False:
                errors.append(f"{path}: unexpected {name!r}")
            elif isinstance(extra, dict):
                errors.extend(schema_errors(extra, item, f"{path}.{name}"))
    if isinstance(value, list) and "items" in schema:
        for i, item in enumerate(value):
            errors.extend(schema_errors(schema["items"], item, f"{path}[{i}]"))
    return errors


def _json_type(value: object) -> str:
    for kind in ("null", "boolean", "integer", "number", "string", "array", "object"):
        if _type_ok(kind, value):
            return kind
    return type(value).__name__


def event_errors(kind: str, data: object) -> list[str]:
    """Where a recorded event's payload breaks its kind's schema (an unknown kind is one error)."""
    schema = EVENT_KINDS.get(kind)
    if schema is None:
        return [f"unknown event kind {kind!r}"]
    return schema_errors(schema, data)


# --- the audit ----------------------------------------------------------------------------------
@dataclass
class AuditReport:
    """What ``audit`` found: events checked, kinds seen, and each violation (deduplicated)."""

    events: int = 0
    seen: set[str] = field(default_factory=set)
    violations: dict[str, int] = field(default_factory=dict)

    @property
    def unseen(self) -> list[str]:
        """Kinds in the contract that no audited event had."""
        return sorted(set(EVENT_KINDS) - self.seen)


def audit(directory: str | Path) -> AuditReport:
    """Check every event in ``directory``'s ``*.ndjson`` files against ``EVENT_KINDS``."""
    report = AuditReport()
    for path in sorted(Path(directory).glob("*.ndjson")):
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            event = json.loads(line)
            kind = str(event.get("kind", ""))
            if kind.startswith(TEST_KIND_PREFIX):
                continue
            report.events += 1
            report.seen.add(kind)
            for error in event_errors(kind, event.get("data", {})):
                key = f"{kind}: {error}"
                report.violations[key] = report.violations.get(key, 0) + 1
    return report
