"""The egress proxy's request log, as ``sandbox_egress`` events committed with each checkpoint.

The proxy (``egress_proxy.py``, running in its own container) logs one line per request it
decides: ``allow CONNECT pypi.org:443 -> 151.101.0.223:443``, ``deny CONNECT github.com:443: host
not in egress allow-list: github.com`` or ``fail ...: upstream unreachable``. The Docker sandbox
session reads the proxy container's log (``DockerSandboxSession.drain_egress_events``) and the
agent loop commits what it found with the cycle's checkpoint, so every host code in the sandbox
reached — or tried to — is on the mission's record next to the tool calls of that cycle.

Requests are aggregated per (decision, method, host, port) within one drain, with a ``count``:
``go mod download`` opens hundreds of tunnels to the same two hosts. Plain-HTTP targets are full
URLs; only their host and port are kept (a query string can carry a secret). Go mirrors this in
``go/internal/execution/egressproxy/events.go``.
"""

from __future__ import annotations

import re
from urllib.parse import urlsplit

from lha.contracts.state import EventRecord
from lha.obs.redact import redact_text

EVENT_KIND = "sandbox_egress"

_LINE = re.compile(
    r" (?:INFO|WARNING) (?P<decision>allow|deny|fail) (?P<method>[A-Z]+) (?P<target>\S+)"
    r"(?: -> (?P<address>\S+)|: (?P<reason>.*))$"
)


def _host_port(method: str, target: str) -> tuple[str, int]:
    try:
        parts = urlsplit(target if method != "CONNECT" else "//" + target)
        port = parts.port
    except ValueError:
        return "?", 0
    default = 443 if parts.scheme == "https" else 80
    return parts.hostname or "?", port if port is not None else default


def parse_proxy_log(lines: list[str]) -> list[EventRecord]:
    """The ``sandbox_egress`` events for ``lines`` of the proxy log (other lines are ignored)."""
    details: dict[tuple[str, str, str, int], str] = {}
    counts: dict[tuple[str, str, str, int], int] = {}
    for line in lines:
        match = _LINE.search(line.rstrip("\r"))
        if match is None:
            continue
        host, port = _host_port(match["method"], match["target"])
        key = (match["decision"], match["method"], host, port)
        if key not in details:
            details[key] = match["address"] or redact_text(match["reason"] or "")
        counts[key] = counts.get(key, 0) + 1
    return [
        EventRecord(
            kind=EVENT_KIND,
            payload={
                "decision": decision,
                "method": method,
                "host": host,
                "port": port,
                "detail": detail,
                "count": counts[(decision, method, host, port)],
            },
        )
        for (decision, method, host, port), detail in details.items()
    ]


class ProxyLogCursor:
    """Remembers how much of a (growing) proxy log was already turned into events."""

    def __init__(self) -> None:
        self._seen = 0

    def drain(self, log: str) -> list[EventRecord]:
        """Events for the complete lines of ``log`` past the previous drain."""
        lines = log.split("\n")
        complete = lines[:-1]  # the last element is "" or a line still being written
        fresh = complete[self._seen :]
        self._seen = max(self._seen, len(complete))
        return parse_proxy_log(fresh)
