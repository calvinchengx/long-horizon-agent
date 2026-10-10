"""``lha watch``: a refreshing terminal view of one mission (Mission UI Phase 4).

Pure render + fetch helpers, split out of the CLI so they are unit-testable. The render is the
same bytes as the Go implementation's (``go/cmd/lha/watch.go``): both read the UI API
(``spec/serve/openapi.json``), print the mission's state and its newest events.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import parse_qs, urlsplit, urlunsplit

import httpx

#: Clear the screen and home the cursor before each refresh.
CLEAR = "\x1b[2J\x1b[H"

#: The default server base when ``--url`` and ``LHA_SERVE_URL`` are unset.
DEFAULT_URL = "http://127.0.0.1:8765"


class WatchError(Exception):
    """The server is unreachable, or the mission is unknown (a clean operator error)."""


def resolve_target(url: str, token: str | None) -> tuple[str, str]:
    """``(base, token)`` from a ``--url`` value and an explicit ``--token``.

    The URL may be the ``lha serve`` start-up form with a ``?token=`` query; an explicit token
    (``--token`` or ``LHA_SERVE_TOKEN``) wins over the query's.
    """
    parts = urlsplit(url)
    query_token = parse_qs(parts.query).get("token", [""])[0]
    base = urlunsplit((parts.scheme, parts.netloc, parts.path.rstrip("/"), "", "")).rstrip("/")
    if not base:
        base = DEFAULT_URL
    return base, (token if token else query_token)


def _summary(kind: str, payload: dict[str, Any]) -> str:
    """The per-kind one-field summary ``lha watch`` shows after an event's kind (or ``""``)."""
    if kind == "tool_call":
        tool = str(payload.get("tool", ""))
        return f"{tool} failed" if payload.get("ok") is False else tool
    if kind in ("cycle_started", "loop_detected"):
        return str(payload.get("item_id", ""))
    if kind in ("verify", "checkpoint", "review"):
        return str(payload.get("verdict", ""))
    if kind == "session_progress":
        return f"turns {payload.get('turns', '')}"
    if kind == "parallel_wave":
        return ",".join(str(item) for item in payload.get("items") or [])
    if kind == "tool_approval" or kind.startswith("gate"):
        return str(payload.get("decision", ""))
    return ""


def watch_render(mission: dict[str, Any], events: list[dict[str, Any]]) -> str:
    """The exact bytes ``lha watch`` prints for one render (matches the Go ``watchRender``).

    ``events`` are oldest first (the API's order); they print newest first.
    """
    header = f"{mission['mission_id']}  {mission['status']}"
    if mission.get("durable"):
        header += "  durable"
    lines = [header, str(mission.get("title", ""))]
    items = mission.get("items")
    if items is None:
        lines.append("items -")
    else:
        lines.append(
            f"items {items['done']}/{items['total']} done, {items['in_progress']} in progress, "
            f"{items['blocked']} blocked, {items['split']} split"
        )
    spend = mission["spend"]
    spend_line = f"spend ${spend['known_usd']:.4f} over {spend['calls']} calls"
    if spend.get("unknown_cost_calls"):
        spend_line += f" ({spend['unknown_cost_calls']} unpriced)"
    lines.append(spend_line)
    live = mission.get("live")
    gate = live.get("gate") if isinstance(live, dict) else None
    if gate:
        lines.append(f"gate {gate['gate_id']}: {gate['question']}")
    out = "\n".join(lines) + "\n\nevents:\n"
    for event in reversed(events):
        fields = [str(event["ts"]), event.get("cycle_id") or "-", str(event["kind"])]
        summary = _summary(str(event["kind"]), event.get("payload") or {})
        if summary:
            fields.append(summary)
        out += "  " + "  ".join(fields) + "\n"
    return out


def watch_fetch(
    client: httpx.Client, base_url: str, token: str, mission_id: str, limit: int
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """GET the mission and its newest ``limit`` events; raise ``WatchError`` on a 404 or a failure.

    The events endpoint pages forward from ``after`` (exclusive), so the newest ``limit`` events
    are those after ``last_event.id - limit``.
    """
    headers = {"X-LHA-Token": token} if token else {}
    try:
        detail = client.get(f"{base_url}/api/v1/missions/{mission_id}", headers=headers)
        if detail.status_code == 404:
            raise WatchError(f"no mission {mission_id!r}")
        if detail.status_code != 200:
            raise WatchError(f"server returned HTTP {detail.status_code}")
        mission: dict[str, Any] = detail.json()
        last = (mission.get("last_event") or {}).get("id") or 0
        after = max(0, int(last) - limit)
        events_response = client.get(
            f"{base_url}/api/v1/missions/{mission_id}/events",
            params={"after": after, "limit": limit},
            headers=headers,
        )
        if events_response.status_code == 404:
            raise WatchError(f"no mission {mission_id!r}")
        if events_response.status_code != 200:
            raise WatchError(f"server returned HTTP {events_response.status_code}")
    except httpx.HTTPError as exc:
        raise WatchError(f"cannot reach the server at {base_url}: {exc}") from exc
    events: list[dict[str, Any]] = events_response.json().get("events", [])
    return mission, events[-limit:]
