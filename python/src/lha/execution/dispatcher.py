"""``AllowListDispatcher`` — the safety gate every tool call passes through.

Enforces (in code, below the model — never via a system prompt that can be prompt-injected):
- an explicit allow-list of tool names (registered AND named in ``allow``; empty => nothing);
- mutating-tool and egress (network) policy, both default-deny (opt in per dispatcher);
- argument validation against each tool's JSON Schema (required keys AND types);
- workspace path containment for ``path_args``, and no mutation of the harness-owned ``.lha/`` and
  ``.git/`` directories;
- human gates: commands classified irreversible/outward-facing (``lha.safety.commands``) are sent
  to the configured ``HITLGate``; with no gate they are DENIED (fail closed). Every answer is kept
  as a ``tool_approval`` event (``drain_events``) that the agent loop commits with the checkpoint;
- Meta's Rule of Two: the dispatcher derives its capability set from its config and refuses to be
  built holding the full trifecta unless a gate is present (then every egress call is gated);
and guarantees the agent loop never crashes on a tool error (failures return as ``ToolResult``).
"""

from __future__ import annotations

import json
from collections.abc import Iterable
from pathlib import Path

from lha.contracts.hitl import GateDecision, GateRequest, GateResolution, HITLGate, RiskTier
from lha.contracts.model import ToolCall
from lha.contracts.sandbox import host_root
from lha.contracts.state import EventRecord
from lha.contracts.tools import Tool, ToolContext, ToolResult, ToolSpec
from lha.execution.paths import PathEscapeError, is_protected, is_protected_resolved
from lha.hitl.approvals import PENDING, action_fingerprint
from lha.obs.redact import redact_text
from lha.safety.commands import classify_command
from lha.safety.rule_of_two import Capability, check_rule_of_two, permits


class AllowListDispatcher:
    """A ``ToolDispatcher`` with an allow-list, mutating/egress policy and human gates.

    Args:
        tools: the tools this dispatcher may run (registration is itself an allow-list).
        allow: the tool names that may be called. FAIL-CLOSED: the default (empty) allows
            nothing; pass the names explicitly, e.g. ``allow={t.spec.name for t in tools}``.
        allow_mutating: permit tools flagged ``mutating`` (default ``False``: read-only).
        allow_egress: permit tools flagged ``egress`` (default-deny).
        gate: where irreversible commands (and, under the trifecta, egress calls) are sent for a
            decision. ``None`` means such calls are denied.
        capabilities: Rule-of-Two capabilities the caller knows this session holds beyond what
            the tools imply — typically ``Capability.PRIVATE_DATA`` when the workspace or the
            sandbox exposes secrets/customer data.

    Raises:
        RuleOfTwoViolation: if the derived capability set is the full trifecta and no gate is set.
    """

    def __init__(
        self,
        tools: Iterable[Tool],
        *,
        allow: Iterable[str] = (),
        allow_mutating: bool = False,
        allow_egress: bool = False,
        gate: HITLGate | None = None,
        capabilities: Iterable[Capability] = (),
    ) -> None:
        self._tools: dict[str, Tool] = {t.spec.name: t for t in tools}
        if isinstance(allow, str):
            raise TypeError("allow must be a collection of tool names, not a str")
        self._allow: frozenset[str] = frozenset(allow)  # empty => nothing is allowed
        self._allow_mutating = allow_mutating
        self._allow_egress = allow_egress
        self._gate = gate
        self._events: list[EventRecord] = []
        self.capabilities: frozenset[Capability] = frozenset(
            {*capabilities, *self._derived_capabilities()}
        )
        if gate is None:
            check_rule_of_two(set(self.capabilities))
        self._gate_all_egress = not permits(set(self.capabilities))

    @classmethod
    def for_tools(
        cls,
        tools: Iterable[Tool],
        *,
        allow_mutating: bool,
        allow_egress: bool = False,
        gate: HITLGate | None = None,
        capabilities: Iterable[Capability] = (),
    ) -> AllowListDispatcher:
        """Allow every tool in ``tools`` by name; the mutating policy must still be stated."""
        registered = list(tools)
        return cls(
            registered,
            allow={t.spec.name for t in registered},
            allow_mutating=allow_mutating,
            allow_egress=allow_egress,
            gate=gate,
            capabilities=capabilities,
        )

    def _derived_capabilities(self) -> set[Capability]:
        caps: set[Capability] = set()
        for name, tool in self._tools.items():
            spec = tool.spec
            if not self._usable(name, spec):
                continue
            if spec.egress:
                caps.add(Capability.EXTERNAL_COMMS)
            if spec.untrusted_input:
                caps.add(Capability.UNTRUSTED_CONTENT)
        return caps

    def _permitted(self, name: str) -> bool:
        return name in self._allow

    def _usable(self, name: str, spec: ToolSpec) -> bool:
        return (
            self._permitted(name)
            and (self._allow_mutating or not spec.mutating)
            and (self._allow_egress or not spec.egress)
        )

    def drain_events(self) -> list[EventRecord]:
        """Gate events since the last drain (decisions + the gate's own reminders), oldest first.

        The agent loop commits them with the cycle's checkpoint (``.lha/events.ndjson``).
        """
        events, self._events = self._events, []
        drain = getattr(self._gate, "drain_events", None)
        if callable(drain):
            events.extend(drain())
        return events

    def specs(self) -> list[ToolSpec]:
        return [tool.spec for name, tool in self._tools.items() if self._permitted(name)]

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        tool = self._tools.get(call.name)
        if tool is None:
            return ToolResult.failure(f"unknown tool: {call.name!r}")
        if not self._permitted(call.name):
            return ToolResult.failure(f"tool not allowed: {call.name!r}")
        spec = tool.spec
        if spec.mutating and not self._allow_mutating:
            return ToolResult.failure(f"mutating tools are disabled: {call.name!r}")
        if spec.egress and not self._allow_egress:
            return ToolResult.failure(f"egress is disabled (default-deny): {call.name!r}")

        missing = _missing_required(spec.parameters, call.arguments)
        if missing:
            return ToolResult.failure(f"missing required args for {call.name!r}: {sorted(missing)}")
        type_error = validate_arguments(spec.parameters, call.arguments)
        if type_error:
            return ToolResult.failure(f"invalid args for {call.name!r}: {type_error}")

        path_error = _check_paths(spec, call.arguments, ctx)
        if path_error:
            return ToolResult.failure(path_error)

        gate_reason = self._gate_reason(spec, call.arguments, ctx)
        if gate_reason is not None:
            denied = await self._ask_gate(call, ctx, gate_reason)
            if denied:
                return ToolResult.failure(denied)

        try:
            return await tool.run(call.arguments, ctx)
        except Exception as exc:
            return ToolResult.failure(f"{type(exc).__name__}: {exc}")

    def _gate_reason(
        self, spec: ToolSpec, arguments: dict[str, object], ctx: ToolContext
    ) -> str | None:
        if spec.command_arg is not None:
            argv = arguments.get(spec.command_arg)
            if isinstance(argv, list):
                # A sandbox whose workdir is not the host checkout (Docker, E2B) has its own /tmp.
                workdir = ctx.session.workdir
                reason = classify_command(
                    [str(token) for token in argv],
                    workspace=workdir,
                    private_tmp=host_root(ctx.session) != workdir,
                )
                if reason:
                    return reason
        if spec.egress and self._gate_all_egress:
            return "egress while holding untrusted input + private data + external comms"
        return None

    async def _ask_gate(self, call: ToolCall, ctx: ToolContext, reason: str) -> str | None:
        """Return ``None`` if a human approved, else the denial message (fail closed)."""
        context = {
            "tool": call.name,
            "arguments": repr(call.arguments)[:2000],
            "reason": reason,
            "fingerprint": action_fingerprint(call.name, call.arguments),
            "mission_id": ctx.mission_id,
        }
        spec = self._tools[call.name].spec
        argv = call.arguments.get(spec.command_arg) if spec.command_arg else None
        if isinstance(argv, list):  # the exact command, for a human to read
            context["argv"] = json.dumps([str(token) for token in argv])
        request = GateRequest(
            gate_id=f"{ctx.mission_id}:tool:{call.id or call.name}",
            question=f"Allow {call.name!r}? {reason}",
            risk=RiskTier.IRREVERSIBLE,
            default_action=GateDecision.REJECT,
            context=context,
        )
        if self._gate is None:
            self._record(request, None, "no human gate configured")
            return f"irreversible action denied (no human gate configured): {reason}"
        try:
            resolution = await self._gate.request(request)
        except Exception as exc:
            self._record(request, None, f"gate error: {type(exc).__name__}")
            return f"irreversible action denied (gate error: {type(exc).__name__}): {reason}"
        self._record(request, resolution, "")
        if resolution.resolved_by == PENDING:
            return (
                f"queued for human approval: {reason}. It is NOT done. An operator will be asked; "
                "if approved, this exact call is allowed in a later cycle. Continue with other "
                "work meanwhile and do not try to work around the gate."
            )
        if resolution.decision is not GateDecision.APPROVE:
            how = "by default" if resolution.defaulted else f"by {resolution.resolved_by or 'gate'}"
            return f"irreversible action denied ({resolution.decision.value} {how}): {reason}"
        return None

    def _record(
        self, request: GateRequest, resolution: GateResolution | None, failure: str
    ) -> None:
        """Keep the gate's answer as a ``tool_approval`` event (secrets redacted)."""
        if resolution is not None and resolution.resolved_by == PENDING:
            decision = "pending"
        elif resolution is not None:
            decision = resolution.decision.value
        else:
            decision = GateDecision.REJECT.value
        self._events.append(
            EventRecord(
                kind="tool_approval",
                payload={
                    "tool": request.context["tool"],
                    "arguments": redact_text(request.context["arguments"]),
                    "reason": request.context["reason"],
                    "fingerprint": request.context["fingerprint"],
                    "decision": decision,
                    "approved": decision == GateDecision.APPROVE.value,
                    "resolved_by": resolution.resolved_by if resolution else failure,
                    "defaulted": resolution.defaulted if resolution else True,
                },
            )
        )


def _check_paths(spec: ToolSpec, arguments: dict[str, object], ctx: ToolContext) -> str | None:
    """Containment for every path arg; mutating tools may not touch ``.lha/`` or ``.git/``."""
    local_root = Path(host_root(ctx.session))
    host_local = local_root.is_dir()
    for name in spec.path_args:
        value = arguments.get(name)
        if not isinstance(value, str) or not value:
            continue
        try:
            protected = (
                is_protected_resolved(local_root, value) if host_local else is_protected(value)
            )
        except PathEscapeError as exc:
            return f"path not allowed for {spec.name!r}: {exc}"
        if protected and spec.mutating:
            return f"{spec.name!r} may not modify harness-owned path {value!r} (.lha/ or .git/)"
    return None


def _missing_required(parameters: dict[str, object], arguments: dict[str, object]) -> set[str]:
    """Return required parameter names (per JSON Schema) absent from ``arguments``."""
    required = parameters.get("required", [])
    if not isinstance(required, list):
        return set()
    return {str(name) for name in required if name not in arguments}


_JSON_TYPES: dict[str, tuple[type, ...]] = {
    "string": (str,),
    "integer": (int,),
    "number": (int, float),
    "boolean": (bool,),
    "array": (list,),
    "object": (dict,),
    "null": (type(None),),
}


def validate_arguments(schema: dict[str, object], value: object, where: str = "") -> str | None:
    """Validate ``value`` against the JSON-Schema subset tools use; return the first error.

    Supports ``type`` (``bool`` is never an integer/number), ``properties``, ``required``,
    ``additionalProperties`` (bool or schema), ``items``, ``enum``, ``minimum``/``maximum``.
    """
    label = where or "arguments"
    expected = schema.get("type")
    if isinstance(expected, str) and expected in _JSON_TYPES:
        ok = isinstance(value, _JSON_TYPES[expected])
        if expected in ("integer", "number") and isinstance(value, bool):
            ok = False
        if not ok:
            return f"{label} must be of type {expected}, got {type(value).__name__}"
    enum = schema.get("enum")
    if isinstance(enum, list) and value not in enum:
        return f"{label} must be one of {enum}"
    if isinstance(value, int | float) and not isinstance(value, bool):
        minimum, maximum = schema.get("minimum"), schema.get("maximum")
        if isinstance(minimum, int | float) and value < minimum:
            return f"{label} must be >= {minimum}"
        if isinstance(maximum, int | float) and value > maximum:
            return f"{label} must be <= {maximum}"
    if isinstance(value, dict):
        props = schema.get("properties")
        props = props if isinstance(props, dict) else {}
        required = schema.get("required")
        if where and isinstance(required, list):
            missing = [str(k) for k in required if k not in value]
            if missing:
                return f"{label} is missing {missing}"
        extra_schema = schema.get("additionalProperties", True)
        for key, item in value.items():
            sub = props.get(key)
            child = f"{where}.{key}" if where else str(key)
            if isinstance(sub, dict):
                error = validate_arguments(sub, item, child)
            elif extra_schema is False:
                error = f"unexpected argument {child!r}"
            elif isinstance(extra_schema, dict):
                error = validate_arguments(extra_schema, item, child)
            else:
                error = None
            if error:
                return error
    if isinstance(value, list):
        items = schema.get("items")
        if isinstance(items, dict):
            for index, item in enumerate(value):
                error = validate_arguments(items, item, f"{label}[{index}]")
                if error:
                    return error
    return None
