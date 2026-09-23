"""Run tool assembly: the web tools, their egress policy and the run-level Rule of Two.

``lha.agent.assembly`` (the lead, in every run path), the orchestrator's read-only roles and the
durable sub-agent activity all build their dispatcher with ``build_run_dispatcher``, so the web
tools and the Rule of Two are wired identically everywhere:

- **Web tools are opt-in.** ``fetch_url`` is registered only when ``LHA_WEB_ALLOW_HOSTS`` (plus
  any per-run ``--allow-host``) is non-empty; ``web_search`` additionally needs
  ``LHA_WEB_SEARCH_PROVIDER`` and ``LHA_WEB_SEARCH_API_KEY``. Empty allow-list => no web tools.
- **Egress policy.** ``fetch_url`` gets an ``EgressPolicy`` over exactly the allow-list (IDNA
  normalized; default-deny), the public-address resolver check, per-hop redirect re-checks, the
  configured size/timeout limits and a ``CredentialBroker`` built from ``LHA_WEB_CREDENTIALS``
  whose secrets are bound to allow-listed hosts only.
- **Rule of Two (fail closed).** Web tools give the run ``UNTRUSTED_CONTENT`` and
  ``EXTERNAL_COMMS``. The run holds ``PRIVATE_DATA`` when ``LHA_PRIVATE_DATA=true`` or when the
  sandbox is ``local`` (the agent's shell then runs on the host, with the host's files,
  credentials and network: unrestricted state-changing tools). All three together raise
  ``RuleOfTwoViolation`` before anything runs — even if a human gate is configured.
"""

from __future__ import annotations

import json
from collections.abc import Iterable
from dataclasses import dataclass

import httpx

from lha.config import Settings
from lha.contracts.hitl import HITLGate
from lha.contracts.tools import Tool
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.tools import default_local_tools
from lha.execution.tools.web import FetchUrlTool, WebSearchTool
from lha.safety.egress import (
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    Resolver,
    normalize_host,
    system_resolver,
)
from lha.safety.rule_of_two import Capability, RuleOfTwoViolation, check_rule_of_two


class WebConfigError(ValueError):
    """The web/egress settings are inconsistent (e.g. a credential bound to a non-allowed host)."""


@dataclass
class WebIO:
    """Network seams for the web tools (tests inject a fake resolver + ``MockTransport``)."""

    resolver: Resolver = system_resolver
    transport: httpx.AsyncBaseTransport | None = None


#: The seams every run path uses; tests replace this (never real network in tests).
DEFAULT_WEB_IO = WebIO()


def egress_hosts(settings: Settings) -> set[str]:
    """The normalized egress allow-list (invalid hosts are dropped)."""
    return {h for h in (normalize_host(raw) for raw in settings.web_hosts()) if h}


def web_enabled(settings: Settings) -> bool:
    """Web tools are registered iff the egress allow-list is non-empty."""
    return bool(egress_hosts(settings))


def with_allow_hosts(settings: Settings, hosts: Iterable[str]) -> Settings:
    """``settings`` with ``hosts`` added to the egress allow-list (per-run ``--allow-host``)."""
    extra = [h.strip() for h in hosts if h.strip()]
    if not extra:
        return settings
    merged = list(dict.fromkeys([*settings.web_hosts(), *extra]))
    return settings.model_copy(update={"web_allow_hosts": ",".join(merged)})


def run_capabilities(settings: Settings, *, web: bool | None = None) -> set[Capability]:
    """The Rule-of-Two capability set of a run under ``settings``."""
    web = web_enabled(settings) if web is None else web
    caps: set[Capability] = set()
    if web:
        caps |= {Capability.UNTRUSTED_CONTENT, Capability.EXTERNAL_COMMS}
    if settings.private_data or settings.sandbox == "local":
        caps.add(Capability.PRIVATE_DATA)
    return caps


def check_run_rule_of_two(settings: Settings, *, web: bool | None = None) -> None:
    """Fail closed if the run would hold untrusted content + private data + external comms."""
    caps = run_capabilities(settings, web=web)
    try:
        check_rule_of_two(caps)
    except RuleOfTwoViolation as exc:
        why = (
            "LHA_SANDBOX=local runs the agent's shell on this host (host files, credentials "
            "and network)"
            if settings.sandbox == "local"
            else "LHA_PRIVATE_DATA=true declares the workspace holds secrets or customer data"
        )
        raise RuleOfTwoViolation(
            f"refusing to start: web tools are enabled (egress allow-list: "
            f"{', '.join(sorted(egress_hosts(settings)))}), which brings untrusted content and "
            f"external comms, and {why}. {exc}. Use a docker/e2b sandbox without private data, "
            f"or clear the allow-list (LHA_WEB_ALLOW_HOSTS / --allow-host)."
        ) from exc


def credential_broker(
    settings: Settings, hosts: set[str]
) -> tuple[CredentialBroker, dict[str, list[str]]]:
    """Build the broker from ``LHA_WEB_CREDENTIALS``; each binding must be allow-listed.

    Returns the broker and ``{placeholder: [hosts]}`` (safe to show the model: no secrets).
    """
    broker = CredentialBroker()
    shown: dict[str, list[str]] = {}
    if settings.web_credentials is None:
        return broker, shown
    raw = settings.web_credentials.get_secret_value()
    try:
        spec = json.loads(raw) if raw.strip() else {}
    except json.JSONDecodeError as exc:
        # ``from None`` + only the message: the secret-bearing document never lands in a trace.
        raise WebConfigError(f"LHA_WEB_CREDENTIALS is not valid JSON: {exc.msg}") from None
    if not isinstance(spec, dict):
        raise WebConfigError("LHA_WEB_CREDENTIALS must be a JSON object")
    for placeholder, entry in spec.items():
        if not isinstance(entry, dict):
            raise WebConfigError(f"credential {placeholder!r} must be an object")
        value, bound = entry.get("value"), entry.get("hosts")
        if not isinstance(value, str) or not value:
            raise WebConfigError(f"credential {placeholder!r} needs a non-empty 'value'")
        if not isinstance(bound, list) or not bound or not all(isinstance(h, str) for h in bound):
            raise WebConfigError(f"credential {placeholder!r} needs a non-empty 'hosts' list")
        normalized = {normalize_host(h) for h in bound}
        outside = sorted(h or "<invalid>" for h in normalized if h not in hosts)
        if outside:
            raise WebConfigError(
                f"credential {placeholder!r} is bound to hosts outside the egress allow-list: "
                f"{outside}"
            )
        broker.register(placeholder, value, hosts=normalized)
        shown[placeholder] = sorted(normalized)
    return broker, shown


def web_tools(settings: Settings, *, io: WebIO | None = None) -> list[Tool]:
    """The web tools for ``settings`` (empty when the allow-list is empty)."""
    hosts = egress_hosts(settings)
    if not hosts:
        return []
    io = io or DEFAULT_WEB_IO
    broker, placeholders = credential_broker(settings, hosts)
    tools: list[Tool] = [
        FetchUrlTool(
            egress_policy=EgressPolicy(allow_hosts=hosts, allow_ports=set(settings.web_ports())),
            broker=broker,
            resolver=io.resolver,
            transport=io.transport,
            timeout_s=settings.web_timeout_s,
            max_response_bytes=settings.web_max_response_bytes,
            placeholders=placeholders,
        )
    ]
    key = settings.web_search_api_key.get_secret_value() if settings.web_search_api_key else ""
    if settings.web_search_provider and key:
        try:
            search = WebSearchTool(
                api_key=key,
                provider=settings.web_search_provider,
                endpoint=settings.web_search_endpoint,
                resolver=io.resolver,
                transport=io.transport,
                timeout_s=settings.web_timeout_s,
                max_response_bytes=settings.web_max_response_bytes,
            )
        except EgressDenied as exc:
            raise WebConfigError(f"invalid LHA_WEB_SEARCH_ENDPOINT: {exc}") from exc
        tools.append(search)
    return tools


def preflight_run_tools(settings: Settings) -> None:
    """Validate a run's tool configuration up front (before any planning spend).

    Raises ``RuleOfTwoViolation`` (lethal trifecta) or ``WebConfigError`` (bad web settings).
    """
    check_run_rule_of_two(settings)
    web_tools(settings)


def run_tools(settings: Settings, *, web: bool = True, io: WebIO | None = None) -> list[Tool]:
    """The default local tools, plus the web tools when ``web`` and the allow-list is set."""
    return [*default_local_tools(), *(web_tools(settings, io=io) if web else [])]


def build_run_dispatcher(
    settings: Settings,
    *,
    allow_mutating: bool,
    allow_egress: bool | None = None,
    gate: HITLGate | None = None,
    io: WebIO | None = None,
) -> AllowListDispatcher:
    """The dispatcher for one agent of a run: local tools (+ web tools when enabled).

    ``allow_egress``: ``None`` (default) => web tools iff the allow-list is non-empty; ``False``
    forces them off for this dispatcher (e.g. a role without egress). The run-level Rule of Two
    is checked with the RUN's capabilities (a run's agents share briefs, so untrusted content
    read by one reaches the others) and raises ``RuleOfTwoViolation``.
    """
    check_run_rule_of_two(settings)
    use_web = web_enabled(settings) and allow_egress is not False
    tools = run_tools(settings, web=use_web, io=io)
    declared = {Capability.PRIVATE_DATA} & run_capabilities(settings)
    return AllowListDispatcher.for_tools(
        tools,
        allow_mutating=allow_mutating,
        allow_egress=use_web,
        gate=gate,
        capabilities=declared,
    )
