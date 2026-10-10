"""``lha serve`` paths no black-box case can reach (tests/serve has the rest): a stream that falls
behind, a store hiccup in the shared reader, a duplicate in a stream's live part, and a Temporal
error other than "no such workflow"."""

from __future__ import annotations

import asyncio
from types import SimpleNamespace
from typing import Any

import pytest

pytest.importorskip("starlette")

from lha.serve import app as serve_app


def test_a_stream_that_falls_behind_is_dropped_and_told_so() -> None:
    hub = serve_app.EventHub(SimpleNamespace())  # type: ignore[arg-type]
    slow = hub.subscribe("mission_a")
    other = hub.subscribe("mission_b")
    for n in range(serve_app.STREAM_QUEUE):
        slow.queue.put_nowait(("mission_event", {"id": n}))
    hub._publish("mission_a", ("mission_event", {"id": 99_999}))
    assert slow not in hub._subs and other in hub._subs
    assert other.queue.empty()  # another mission's stream does not get it
    items = [slow.queue.get_nowait() for _ in range(slow.queue.qsize())]
    assert items[-1] is None  # its stream ends; the client resumes from Last-Event-ID


def test_the_shared_reader_keeps_following_after_a_store_hiccup(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(serve_app, "POLL_EVENTS_S", 0.01)
    reads: list[int] = []
    again = asyncio.Event()

    async def read_mission_events(**_: Any) -> list[Any]:
        reads.append(1)
        if len(reads) == 1:
            raise OSError("database is locked")
        again.set()
        return []

    async def list_missions(**_: Any) -> list[Any]:
        return []

    store = SimpleNamespace(read_mission_events=read_mission_events, list_missions=list_missions)
    hub = serve_app.EventHub(SimpleNamespace(store=store))  # type: ignore[arg-type]

    async def run() -> None:
        hub._task = asyncio.create_task(hub._run())
        await asyncio.wait_for(again.wait(), 5)
        await hub.stop()

    asyncio.run(run())
    assert len(reads) >= 2


def test_a_streams_live_part_skips_what_it_sent_and_ends_when_dropped() -> None:
    sub = serve_app._Subscriber(None, asyncio.Queue())
    for message in (
        ("mission_event", {"id": 5}),  # already sent by the replay
        ("mission_event", {"id": 6}),
        ("mission", {"mission_id": "m"}),
        None,
    ):
        sub.queue.put_nowait(message)

    async def collect() -> list[str]:
        return [chunk async for chunk in serve_app._follow(sub, 5, 60)]

    chunks = asyncio.run(collect())
    assert chunks == [
        'id: 6\nevent: mission_event\ndata: {"id":6}\n\n',
        'event: mission\ndata: {"mission_id":"m"}\n\n',
    ]


def test_a_temporal_error_other_than_not_found_is_503() -> None:
    from temporalio.service import RPCStatusCode

    gone = serve_app._temporal_error(SimpleNamespace(status=RPCStatusCode.NOT_FOUND))
    assert (gone.status, gone.code) == (409, "finished")
    down = serve_app._temporal_error(SimpleNamespace(status=RPCStatusCode.UNAVAILABLE))
    assert (down.status, down.code) == (503, "temporal_unavailable")


def test_witness_results_pick_the_latest_and_ignore_junk() -> None:
    """The defensive branches: a non-verify event, a checks value that is not a list, a check that
    is not a mapping, a check naming another item's check, missing fields, a duplicate witness and
    a witness that is not a string."""

    def event(kind: str, payload: dict[str, Any]) -> Any:
        return SimpleNamespace(kind=kind, payload=payload)

    item = {"witnesses": ["cmd:true", "cmd:true", "go:T", 7]}
    about = [
        event("tool_call", {"tool": "x"}),
        event("verify", {"checks": "not a list"}),
        event(
            "verify",
            {
                "checks": [
                    {
                        "name": "cmd:true",
                        "passed": False,
                        "exit_code": 1,
                        "gating": True,
                        "timed_out": False,
                        "duration_s": 0.2,
                    },
                    {"name": "other", "passed": True},
                    "nope",
                ]
            },
        ),
        event("verify", {"checks": [{"name": "cmd:true", "passed": True}]}),  # later result wins
    ]
    assert serve_app._witness_results(item, about) == [
        {
            "witness": "cmd:true",
            "latest": {
                "passed": True,
                "exit_code": 0,
                "gating": True,
                "timed_out": False,
                "duration_s": 0.0,
            },
        },
        {"witness": "go:T", "latest": None},
    ]
    assert serve_app._witness_results({}, []) == []
