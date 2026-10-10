"""``lha watch``: the render is the exact bytes both implementations print, and the fetch reads
the UI API (a 404 and an unreachable server are clean operator errors)."""

from __future__ import annotations

import httpx
import pytest

from lha.cli.watch import WatchError, resolve_target, watch_fetch, watch_render

MISSION: dict = {
    "mission_id": "mission_abc123",
    "title": "Ship the fabric emulator",
    "status": "WAITING_ON_HUMAN",
    "durable": True,
    "items": {"total": 4, "todo": 1, "in_progress": 1, "blocked": 1, "done": 1, "split": 0},
    "spend": {
        "calls": 12,
        "known_usd": 3.5,
        "unknown_cost_calls": 0,
        "input_tokens": 100,
        "output_tokens": 200,
    },
    "live": {
        "status": "WAITING_ON_HUMAN",
        "cycles": 2,
        "gate": {"gate_id": "g1", "question": "Approve `git push`?"},
        "open_question": None,
        "resume_at": None,
        "steer_notes": [],
        "pending_edits": 0,
    },
}

EVENTS: list[dict] = [
    {
        "id": 1,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:01+00:00",
        "kind": "tool_call",
        "payload": {"tool": "run_command", "ok": True},
    },
    {
        "id": 2,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:02+00:00",
        "kind": "tool_call",
        "payload": {"tool": "run_command", "ok": False},
    },
    {
        "id": 3,
        "cycle_id": "",
        "ts": "2026-10-04T00:00:03+00:00",
        "kind": "cycle_started",
        "payload": {"item_id": "01"},
    },
    {
        "id": 4,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:04+00:00",
        "kind": "verify",
        "payload": {"verdict": "passed"},
    },
    {
        "id": 5,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:05+00:00",
        "kind": "session_progress",
        "payload": {"turns": 3},
    },
    {
        "id": 6,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:06+00:00",
        "kind": "parallel_wave",
        "payload": {"items": ["01", "02"]},
    },
    {
        "id": 7,
        "cycle_id": "",
        "ts": "2026-10-04T00:00:07+00:00",
        "kind": "tool_approval",
        "payload": {"decision": "reject"},
    },
    {
        "id": 8,
        "cycle_id": "c1",
        "ts": "2026-10-04T00:00:08+00:00",
        "kind": "llm_turn",
        "payload": {},
    },
]

EXPECTED = (
    "mission_abc123  WAITING_ON_HUMAN  durable\n"
    "Ship the fabric emulator\n"
    "items 1/4 done, 1 in progress, 1 blocked, 0 split\n"
    "spend $3.5000 over 12 calls\n"
    "gate g1: Approve `git push`?\n"
    "\n"
    "events:\n"
    "  2026-10-04T00:00:08+00:00  c1  llm_turn\n"
    "  2026-10-04T00:00:07+00:00  -  tool_approval  reject\n"
    "  2026-10-04T00:00:06+00:00  c1  parallel_wave  01,02\n"
    "  2026-10-04T00:00:05+00:00  c1  session_progress  turns 3\n"
    "  2026-10-04T00:00:04+00:00  c1  verify  passed\n"
    "  2026-10-04T00:00:03+00:00  -  cycle_started  01\n"
    "  2026-10-04T00:00:02+00:00  c1  tool_call  run_command failed\n"
    "  2026-10-04T00:00:01+00:00  c1  tool_call  run_command\n"
)


def test_watch_render_durable_with_open_gate() -> None:
    assert watch_render(MISSION, EVENTS) == EXPECTED


def test_watch_render_local_no_gate_unknown_cost() -> None:
    mission = {
        "mission_id": "mission_local",
        "title": "Local mission",
        "status": "RUNNING",
        "durable": False,
        "items": None,
        "spend": {"calls": 3, "known_usd": 0.0, "unknown_cost_calls": 2},
        "live": None,
    }
    expected = (
        "mission_local  RUNNING\n"
        "Local mission\n"
        "items -\n"
        "spend $0.0000 over 3 calls (2 unpriced)\n"
        "\n"
        "events:\n"
    )
    assert watch_render(mission, []) == expected


def test_watch_render_no_events_ends_with_events_line() -> None:
    assert watch_render(MISSION, []).endswith("events:\n")


def test_resolve_target_takes_token_from_serve_url() -> None:
    base, token = resolve_target("http://127.0.0.1:8765/?token=abc", None)
    assert (base, token) == ("http://127.0.0.1:8765", "abc")


def test_resolve_target_explicit_token_wins() -> None:
    base, token = resolve_target("http://127.0.0.1:8765/?token=abc", "xyz")
    assert (base, token) == ("http://127.0.0.1:8765", "xyz")


def test_watch_fetch_ok_sends_token_and_returns_payload() -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.headers.get("X-LHA-Token", ""))
        if request.url.path.endswith("/events"):
            assert request.url.params["after"] == "0"
            assert request.url.params["limit"] == "10"
            return httpx.Response(200, json={"events": EVENTS, "next_after": 8})
        return httpx.Response(200, json=MISSION)

    client = httpx.Client(transport=httpx.MockTransport(handler))
    mission, events = watch_fetch(client, "http://127.0.0.1:9999", "tok", "mission_abc123", 10)
    assert mission["mission_id"] == "mission_abc123"
    assert events == EVENTS
    assert seen == ["tok", "tok"]


def test_watch_fetch_asks_for_the_newest_events() -> None:
    seen: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/events"):
            seen["after"] = request.url.params["after"]
            return httpx.Response(200, json={"events": [], "next_after": 30})
        return httpx.Response(
            200, json={**MISSION, "last_event": {"id": 30, "kind": "llm_turn", "ts": "x"}}
        )

    client = httpx.Client(transport=httpx.MockTransport(handler))
    watch_fetch(client, "http://127.0.0.1:9999", "", "mission_abc123", 10)
    assert seen["after"] == "20"  # the newest 10 are those after last_event.id - limit


def test_watch_fetch_404_is_a_watch_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404, json={"error": "not_found"})

    client = httpx.Client(transport=httpx.MockTransport(handler))
    with pytest.raises(WatchError, match="no mission 'mission_missing'"):
        watch_fetch(client, "http://127.0.0.1:9999", "", "mission_missing", 10)


def test_watch_fetch_unreachable_is_a_watch_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    client = httpx.Client(transport=httpx.MockTransport(handler))
    with pytest.raises(WatchError, match="cannot reach the server"):
        watch_fetch(client, "http://127.0.0.1:9999", "", "mission_abc123", 10)
