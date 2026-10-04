"""``lha serve``: the UI API (spec/serve/openapi.json) over the mission store, anchors and Temporal.

Every route reads shared contracts only: the store schema (``missions``, ``cost_ledger``,
``hitl_gates``, ``mission_events``), the ``.lha/`` anchor at a mission's recorded ``workdir``, and
the workflow's queries and signals (the wire contract). So this server shows any implementation's
missions, and any implementation's server shows this one's (docs/27-mission-ui.md).

Performance: one shared reader (``EventHub``) follows ``mission_events`` and the mission rows and
fans out to every stream; Temporal queries are cached per mission for a few seconds; nothing polls
per client.

Security: the server binds to loopback, refuses a ``Host`` that is not its own loopback address
(DNS rebinding), and needs the start-up token on every request: the ``X-LHA-Token`` header, or
for reads the ``lha_token`` cookie the start-up URL sets (``SameSite=Strict``, ``HttpOnly``).
"""

from __future__ import annotations

import asyncio
import contextlib
import hmac
import html
import json
import re
import time
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, Response, StreamingResponse
from starlette.routing import Route

from lha.config import Settings
from lha.persistence.store import (
    TERMINAL_STATUSES,
    CostSummary,
    EventRow,
    GateRow,
    MissionRow,
    MissionStore,
)
from lha.state.mission_anchor import ANCHOR_DIR, GitMissionAnchor

API_VERSION = "1.0.0"
IMPLEMENTATION = "python"
#: How long a mission's live state (its workflow's query answers) is reused.
LIVE_TTL_S = 3.0
#: How long Temporal stays "unavailable" after a failed connection before it is tried again.
TEMPORAL_RETRY_S = 5.0
TEMPORAL_CONNECT_TIMEOUT_S = 3.0
KEEPALIVE_S = 15.0
POLL_EVENTS_S = 0.5
POLL_MISSIONS_S = 2.0
STREAM_QUEUE = 1000
_DECISIONS = ("approve", "reject", "retry", "abort", "impossible")
_EDIT_FIELDS: dict[str, dict[str, str]] = {
    "add": {
        "description": "str",
        "id": "str",
        "witnesses": "list",
        "depends_on": "list",
        "after": "str",
        "allow_harness_edits": "bool",
        "notes": "str",
    },
    "remove": {"id": "str"},
    "edit": {
        "id": "str",
        "description": "str",
        "witnesses": "list",
        "depends_on": "list",
        "allow_harness_edits": "bool",
        "notes": "str",
    },
    "reopen": {"id": "str"},
    "block": {"id": "str"},
    "unblock": {"id": "str"},
}


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


def _error(status: int, code: str, message: str) -> JSONResponse:
    return JSONResponse({"error": {"code": code, "message": message}}, status_code=status)


# --- Temporal: one client, live state cached per mission ---------------------------------------
class Temporal:
    """The Temporal client (connected lazily) and each mission's cached live state."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._client: Any = None
        self._failed_at = 0.0
        self._error = ""
        self._lock = asyncio.Lock()
        self._live: dict[str, tuple[float, dict[str, Any] | None, str | None]] = {}

    async def client(self) -> Any:
        """The connected client; ``ApiError`` 503 when Temporal is unreachable."""
        async with self._lock:
            if self._client is not None:
                return self._client
            if time.monotonic() - self._failed_at < TEMPORAL_RETRY_S:
                raise ApiError(503, "temporal_unavailable", self._error)
            from lha.durable.worker import connect_client

            try:
                self._client = await asyncio.wait_for(
                    connect_client(self._settings), TEMPORAL_CONNECT_TIMEOUT_S
                )
            except Exception as exc:
                self._failed_at = time.monotonic()
                self._error = (
                    f"Temporal at {self._settings.temporal_address} is unreachable: "
                    f"{type(exc).__name__}: {exc}"
                )[:500]
                raise ApiError(503, "temporal_unavailable", self._error) from None
            return self._client

    async def status(self) -> str:
        try:
            await self.client()
        except ApiError:
            return "unavailable"
        return "connected"

    async def live(self, workflow_id: str) -> tuple[dict[str, Any] | None, str | None]:
        """The workflow's query answers (cached ``LIVE_TTL_S``), or ``(None, why)``."""
        cached = self._live.get(workflow_id)
        if cached is not None and time.monotonic() - cached[0] < LIVE_TTL_S:
            return cached[1], cached[2]
        try:
            client = await self.client()
            state, error = await _query_live(client.get_workflow_handle(workflow_id)), None
        except ApiError as exc:
            state, error = None, exc.message
        except Exception as exc:
            state, error = None, f"{type(exc).__name__}: {exc}"[:500]
        self._live[workflow_id] = (time.monotonic(), state, error)
        return state, error

    def forget(self, workflow_id: str) -> None:
        self._live.pop(workflow_id, None)


async def _optional_query(handle: Any, name: str) -> Any:
    from temporalio.client import WorkflowQueryFailedError

    try:
        return await handle.query(name)
    except WorkflowQueryFailedError:
        return None  # an older worker that does not answer it


async def _query_live(handle: Any) -> dict[str, Any]:
    from lha.durable.signals import (
        QUERY_CYCLES,
        QUERY_GATE,
        QUERY_PENDING_EDITS,
        QUERY_STATUS,
        QUERY_STEER_NOTES,
    )

    gate = await _optional_query(handle, QUERY_GATE)
    resume_at = await _optional_query(handle, "resume_at")
    return {
        "status": str(await handle.query(QUERY_STATUS)),
        "cycles": int(await handle.query(QUERY_CYCLES) or 0),
        "gate": _open_gate(gate) if gate else None,
        "open_question": (await _optional_query(handle, "open_question")) or None,
        "resume_at": datetime.fromtimestamp(float(resume_at), UTC).isoformat()
        if resume_at
        else None,
        "steer_notes": [str(n) for n in (await _optional_query(handle, QUERY_STEER_NOTES) or [])],
        "pending_edits": int(await _optional_query(handle, QUERY_PENDING_EDITS) or 0),
    }


def _open_gate(gate: Any) -> dict[str, Any]:
    g = gate if isinstance(gate, dict) else asdict(gate)
    request = g.get("request")
    if request is not None and not isinstance(request, dict):
        request = asdict(request)
    return {
        "gate_id": str(g.get("gate_id", "")),
        "kind": str(g.get("kind", "")),
        "question": str(g.get("question", "")),
        "options": [str(o) for o in g.get("options", [])],
        "default_action": str(g.get("default_action", "")),
        "opened_at": str(g.get("opened_at", "")),
        "deadline": str(g.get("deadline", "")),
        "escalations_sent": int(g.get("escalations_sent", 0)),
        "next_escalation_at": str(g.get("next_escalation_at", "")),
        "recommended": str(g.get("recommended", "")),
        "request": {
            "tool": str(request.get("tool", "")),
            "arguments": str(request.get("arguments", "")),
            "reason": str(request.get("reason", "")),
            "fingerprint": str(request.get("fingerprint", "")),
        }
        if request
        else None,
    }


# --- reading the store and anchors ------------------------------------------------------------
async def _items(row: MissionRow) -> list[dict[str, Any]] | None:
    """The mission's checklist from its anchor, or ``None`` when it cannot be read here."""
    if not row.workdir or not (Path(row.workdir) / ANCHOR_DIR).is_dir():
        return None
    try:
        checklist = await GitMissionAnchor(row.workdir).read_checklist()
    except Exception:
        return None
    return [item.model_dump() for item in checklist.items]


def _counts(items: list[dict[str, Any]] | None) -> dict[str, int] | None:
    if items is None:
        return None
    counts = {"total": len(items), "todo": 0, "in_progress": 0, "blocked": 0, "done": 0, "split": 0}
    for item in items:
        if item["status"] in counts:
            counts[item["status"]] += 1
    return counts


def _spend(cost: CostSummary) -> dict[str, Any]:
    return {
        "calls": cost.calls,
        "known_usd": cost.known_usd,
        "unknown_cost_calls": cost.unknown_cost_calls,
        "input_tokens": cost.input_tokens,
        "output_tokens": cost.output_tokens,
    }


def _event(row: EventRow) -> dict[str, Any]:
    return {
        "id": row.id,
        "mission_id": row.mission_id,
        "cycle_id": row.cycle_id,
        "ts": row.ts,
        "kind": row.kind,
        "payload": row.payload,
        "schema_version": row.schema_version,
    }


def _gate(row: GateRow) -> dict[str, Any]:
    return {
        "gate_id": row.gate_id,
        "kind": row.kind,
        "status": row.status,
        "question": row.question,
        "options": list(row.options),
        "default_action": row.default_action,
        "risk": row.risk,
        "deadline": row.deadline,
        "decision": row.decision or None,
        "resolved_by": row.resolved_by or None,
        "reminders": row.reminders,
        "request": row.request,
        "opened_at": row.opened_at,
        "resolved_at": row.resolved_at or "",
    }


@dataclass
class App:
    """What every route shares."""

    settings: Settings
    store: MissionStore
    token: str
    port: int = 0
    temporal: Temporal = field(init=False)
    hub: EventHub = field(init=False)

    def __post_init__(self) -> None:
        self.temporal = Temporal(self.settings)
        self.hub = EventHub(self)

    async def mission(self, mission_id: str) -> MissionRow:
        row = await self.store.get_mission(mission_id)
        if row is None:
            raise ApiError(404, "not_found", f"no mission {mission_id!r}")
        return row

    async def summary(self, row: MissionRow, items: list[dict[str, Any]] | None = None) -> dict:
        cost, last, items = await asyncio.gather(
            self.store.cost_summary(row.mission_id),
            self.store.last_mission_event(row.mission_id),
            _items(row) if items is None else _done(items),
        )
        return {
            "mission_id": row.mission_id,
            "title": row.title,
            "status": row.status,
            "durable": bool(row.workflow_id),
            "workflow_id": row.workflow_id or None,
            "head_sha": row.head_sha or None,
            "created_at": row.created_at,
            "updated_at": row.updated_at,
            "spend": _spend(cost),
            "items": _counts(items),
            "last_event": {"id": last.id, "ts": last.ts, "kind": last.kind} if last else None,
        }


async def _done(value: Any) -> Any:
    return value


# --- the shared event reader --------------------------------------------------------------------
@dataclass(eq=False)  # compared and hashed by identity: each stream is its own subscriber
class _Subscriber:
    mission_id: str | None
    queue: asyncio.Queue[tuple[str, dict[str, Any]] | None]


class EventHub:
    """One reader that follows ``mission_events`` and the mission rows for every stream."""

    def __init__(self, app: App) -> None:
        self._app = app
        self._subs: set[_Subscriber] = set()
        self.cursor = 0
        self._updated: dict[str, str] = {}
        self._task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        last = await self._app.store.read_mission_events(after_id=0, limit=1)
        # Start at the newest event: a stream replays older ones itself (``after``).
        self.cursor = await self._tail_id() if last else 0
        for row in await self._app.store.list_missions(limit=1000):
            self._updated[row.mission_id] = row.updated_at
        self._task = asyncio.create_task(self._run())

    async def _tail_id(self) -> int:
        cursor = 0
        while True:
            rows = await self._app.store.read_mission_events(after_id=cursor, limit=1000)
            if not rows:
                return cursor
            cursor = rows[-1].id

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task

    def subscribe(self, mission_id: str | None) -> _Subscriber:
        sub = _Subscriber(mission_id, asyncio.Queue(maxsize=STREAM_QUEUE))
        self._subs.add(sub)
        return sub

    def unsubscribe(self, sub: _Subscriber) -> None:
        self._subs.discard(sub)

    def _publish(self, mission_id: str, message: tuple[str, dict[str, Any]]) -> None:
        for sub in list(self._subs):
            if sub.mission_id not in (None, mission_id):
                continue
            try:
                sub.queue.put_nowait(message)
            except asyncio.QueueFull:  # too slow: it reconnects and resumes from Last-Event-ID
                self._subs.discard(sub)
                with contextlib.suppress(asyncio.QueueFull):
                    sub.queue.get_nowait()
                    sub.queue.put_nowait(None)

    async def _run(self) -> None:
        last_missions = 0.0
        while True:
            try:
                rows = await self._app.store.read_mission_events(after_id=self.cursor, limit=500)
                for row in rows:
                    self._publish(row.mission_id, ("mission_event", _event(row)))
                    self.cursor = row.id
                if time.monotonic() - last_missions >= POLL_MISSIONS_S:
                    last_missions = time.monotonic()
                    await self._missions()
                if len(rows) < 500:
                    await asyncio.sleep(POLL_EVENTS_S)
            except asyncio.CancelledError:
                raise
            except Exception:  # a store hiccup: keep following
                await asyncio.sleep(POLL_EVENTS_S)

    async def _missions(self) -> None:
        if not self._subs:
            return
        for row in await self._app.store.list_missions(limit=1000):
            if self._updated.get(row.mission_id) == row.updated_at:
                continue
            self._updated[row.mission_id] = row.updated_at
            self._publish(row.mission_id, ("mission", await self._app.summary(row)))


# --- request handling ---------------------------------------------------------------------------
def _app(request: Request) -> App:
    return request.app.state.lha


def _guard_host(request: Request) -> None:
    port = _app(request).port
    host = request.headers.get("host", "")
    if host not in (f"127.0.0.1:{port}", f"localhost:{port}"):
        raise ApiError(403, "forbidden_host", f"Host {host!r} is not this server")


def _guard(request: Request, *, write: bool) -> None:
    app = _app(request)
    _guard_host(request)
    token = request.headers.get("x-lha-token")
    if token is None and not write:
        token = request.cookies.get("lha_token")
    if token is None or not hmac.compare_digest(token.encode(), app.token.encode()):
        raise ApiError(401, "unauthorized", "a valid X-LHA-Token header is required")


def _int(request: Request, name: str, default: int | None, lo: int, hi: int | None) -> int | None:
    raw = request.query_params.get(name)
    if raw is None:
        return default
    try:
        value = int(raw)
    except ValueError:
        raise ApiError(422, "invalid_request", f"{name} must be an integer") from None
    if value < lo or (hi is not None and value > hi):
        bound = f"between {lo} and {hi}" if hi is not None else f"at least {lo}"
        raise ApiError(422, "invalid_request", f"{name} must be {bound}")
    return value


Handler = Callable[[Request], Awaitable[Response]]


def _route(*, write: bool = False) -> Callable[[Handler], Handler]:
    def wrap(handler: Handler) -> Handler:
        async def guarded(request: Request) -> Response:
            try:
                _guard(request, write=write)
                return await handler(request)
            except ApiError as exc:
                return _error(exc.status, exc.code, exc.message)

        return guarded

    return wrap


@_route()
async def health(request: Request) -> Response:
    from lha import __version__

    app = _app(request)
    return JSONResponse(
        {
            "api_version": API_VERSION,
            "implementation": IMPLEMENTATION,
            "lha_version": __version__,
            "store": {"backend": app.store.backend, "degraded_reason": app.store.degraded_reason},
            "temporal": await app.temporal.status(),
        }
    )


@_route()
async def list_missions(request: Request) -> Response:
    app = _app(request)
    limit = _int(request, "limit", 50, 1, 500)
    rows = await app.store.list_missions(limit=limit or 50)
    return JSONResponse({"missions": list(await asyncio.gather(*map(app.summary, rows)))})


@_route()
async def get_mission(request: Request) -> Response:
    app = _app(request)
    row = await app.mission(request.path_params["mission_id"])
    items = await _items(row)
    detail = await app.summary(row, items)
    live, live_error = None, None
    if row.workflow_id and row.status not in TERMINAL_STATUSES:
        live, live_error = await app.temporal.live(row.workflow_id)
    detail.update(
        description=row.description, workdir=row.workdir, live=live, live_error=live_error
    )
    return JSONResponse(detail)


@_route()
async def list_items(request: Request) -> Response:
    row = await _app(request).mission(request.path_params["mission_id"])
    items = await _items(row)
    if items is None:
        raise ApiError(
            404, "anchor_unavailable", f"the anchor at {row.workdir!r} cannot be read here"
        )
    return JSONResponse({"items": items})


@_route()
async def list_events(request: Request) -> Response:
    app = _app(request)
    after = _int(request, "after", 0, 0, None) or 0
    limit = _int(request, "limit", 500, 1, 1000) or 500
    row = await app.mission(request.path_params["mission_id"])
    events = await app.store.read_mission_events(
        mission_id=row.mission_id, after_id=after, limit=limit
    )
    return JSONResponse(
        {"events": [_event(e) for e in events], "next_after": events[-1].id if events else after}
    )


@_route()
async def list_costs(request: Request) -> Response:
    app = _app(request)
    limit = _int(request, "limit", 100, 1, 1000) or 100
    row = await app.mission(request.path_params["mission_id"])
    summary, rows = await asyncio.gather(
        app.store.cost_summary(row.mission_id), app.store.list_costs(row.mission_id, limit=limit)
    )
    calls = [
        {
            "cycle_id": c.cycle_id,
            "role": c.role,
            "model": c.model,
            "input_tokens": c.input_tokens,
            "output_tokens": c.output_tokens,
            "usd": c.usd if c.cost_known else None,
            "ts": c.ts,
        }
        for c in reversed(rows)
    ]
    return JSONResponse({"summary": _spend(summary), "calls": calls})


@_route()
async def list_gates(request: Request) -> Response:
    app = _app(request)
    limit = _int(request, "limit", 100, 1, 1000) or 100
    row = await app.mission(request.path_params["mission_id"])
    gates = await app.store.list_gates(row.mission_id, limit=limit)
    return JSONResponse({"gates": [_gate(g) for g in gates]})


@_route()
async def stream(request: Request) -> Response:
    app = _app(request)
    mission_id = request.query_params.get("mission_id") or None
    after = _int(request, "after", None, 0, None)
    last_id = request.headers.get("last-event-id")
    if last_id is not None:
        if not last_id.isdigit():
            raise ApiError(422, "invalid_request", "Last-Event-ID must be an event id")
        after = int(last_id)
    sub = app.hub.subscribe(mission_id)

    async def messages() -> AsyncIterator[str]:
        sent = after if after is not None else app.hub.cursor
        try:
            if after is not None:  # replay what was recorded after the cursor, then follow
                while True:
                    rows = await app.store.read_mission_events(
                        mission_id=mission_id, after_id=sent, limit=500
                    )
                    for row in rows:
                        yield _sse("mission_event", _event(row), row.id)
                        sent = row.id
                    if len(rows) < 500:
                        break
            while True:
                try:
                    message = await asyncio.wait_for(sub.queue.get(), KEEPALIVE_S)
                except TimeoutError:
                    yield ": keepalive\n\n"
                    continue
                if message is None:
                    return  # dropped for falling behind: the client resumes
                kind, data = message
                if kind == "mission_event":
                    if data["id"] <= sent:
                        continue
                    sent = data["id"]
                    yield _sse(kind, data, data["id"])
                else:
                    yield _sse(kind, data, None)
        finally:
            app.hub.unsubscribe(sub)

    return StreamingResponse(
        messages(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-store", "X-Accel-Buffering": "no"},
    )


def _sse(event: str, data: dict[str, Any], ident: int | None) -> str:
    head = f"id: {ident}\n" if ident is not None else ""
    return f"{head}event: {event}\ndata: {json.dumps(data, separators=(',', ':'))}\n\n"


# --- controls -----------------------------------------------------------------------------------
async def _body(request: Request) -> dict[str, Any]:
    try:
        body = await request.json()
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise ApiError(422, "invalid_request", "the body is not JSON") from None
    if not isinstance(body, dict):
        raise ApiError(422, "invalid_request", "the body must be a JSON object")
    return body


def _fields(body: dict[str, Any], allowed: set[str]) -> None:
    extra = sorted(set(body) - allowed)
    if extra:
        raise ApiError(422, "invalid_request", f"unexpected field(s): {', '.join(extra)}")


async def _durable(request: Request) -> tuple[App, MissionRow]:
    app = _app(request)
    row = await app.mission(request.path_params["mission_id"])
    if not row.workflow_id:
        raise ApiError(409, "not_durable", "a local run has no control channel")
    if row.status in TERMINAL_STATUSES:
        raise ApiError(409, "finished", f"the mission has finished ({row.status})")
    return app, row


async def _signal(app: App, row: MissionRow, name: str, arg: Any) -> Response:
    from temporalio.service import RPCError, RPCStatusCode

    client = await app.temporal.client()
    try:
        await client.get_workflow_handle(row.workflow_id).signal(name, arg)
    except RPCError as exc:
        if exc.status == RPCStatusCode.NOT_FOUND:
            raise ApiError(409, "finished", "the mission's workflow is not running") from None
        raise ApiError(503, "temporal_unavailable", f"{exc}"[:500]) from None
    app.temporal.forget(row.workflow_id or "")
    return JSONResponse({"accepted": True}, status_code=202)


@_route(write=True)
async def steer(request: Request) -> Response:
    from lha.durable.signals import MAX_STEER_CHARS, SIGNAL_STEER

    app, row = await _durable(request)
    body = await _body(request)
    _fields(body, {"note"})
    note = body.get("note")
    if not isinstance(note, str) or not note.strip() or len(note) > MAX_STEER_CHARS:
        raise ApiError(422, "invalid_request", f"note must be 1-{MAX_STEER_CHARS} characters")
    return await _signal(app, row, SIGNAL_STEER, note.strip())


@_route(write=True)
async def snooze(request: Request) -> Response:
    from lha.durable.signals import SIGNAL_SNOOZE

    app, row = await _durable(request)
    body = await _body(request)
    _fields(body, {"seconds"})
    seconds = body.get("seconds")
    if not isinstance(seconds, int) or isinstance(seconds, bool) or not 0 <= seconds <= 31_536_000:
        raise ApiError(422, "invalid_request", "seconds must be an integer from 0 to 31536000")
    return await _signal(app, row, SIGNAL_SNOOZE, seconds)


def _check_edit(n: int, edit: Any) -> None:
    if not isinstance(edit, dict) or edit.get("op") not in _EDIT_FIELDS:
        raise ApiError(422, "invalid_request", f"edit {n}: op must be one of {list(_EDIT_FIELDS)}")
    fields = _EDIT_FIELDS[edit["op"]]
    _fields({k: v for k, v in edit.items() if k != "op"}, set(fields))
    required = "description" if edit["op"] == "add" else "id"
    if not isinstance(edit.get(required), str) or not edit[required]:
        raise ApiError(422, "invalid_request", f"edit {n}: {required} is required")
    for key, kind in fields.items():
        value = edit.get(key)
        if value is None:
            continue
        ok = (
            isinstance(value, str)
            if kind == "str"
            else isinstance(value, bool)
            if kind == "bool"
            else isinstance(value, list) and all(isinstance(v, str) for v in value)
        )
        if not ok:
            raise ApiError(422, "invalid_request", f"edit {n}: {key} has the wrong type")


@_route(write=True)
async def edit_checklist(request: Request) -> Response:
    from lha.durable.signals import SIGNAL_CHECKLIST_EDIT
    from lha.state.checklist_edit import MAX_EDIT_OPS

    app, row = await _durable(request)
    body = await _body(request)
    _fields(body, {"edits", "by"})
    edits, by = body.get("edits"), body.get("by", "")
    if not isinstance(edits, list) or not 1 <= len(edits) <= MAX_EDIT_OPS:
        raise ApiError(422, "invalid_request", f"edits must be a list of 1-{MAX_EDIT_OPS} edits")
    for n, edit in enumerate(edits, start=1):
        _check_edit(n, edit)
    if not isinstance(by, str) or len(by) > 200:
        raise ApiError(422, "invalid_request", "by must be at most 200 characters")
    return await _signal(app, row, SIGNAL_CHECKLIST_EDIT, {"edits": edits, "by": by})


@_route(write=True)
async def decide(request: Request) -> Response:
    from lha.durable.signals import QUERY_GATE, SIGNAL_HUMAN_DECISION_V2

    app, row = await _durable(request)
    body = await _body(request)
    _fields(body, {"decision", "by"})
    decision, by = body.get("decision"), body.get("by")
    if decision not in _DECISIONS:
        raise ApiError(422, "invalid_request", f"decision must be one of {list(_DECISIONS)}")
    if not isinstance(by, str) or not by.strip() or len(by) > 200:
        raise ApiError(422, "invalid_request", "by (who answers) must be 1-200 characters")
    client = await app.temporal.client()
    try:
        gate = await _optional_query(client.get_workflow_handle(row.workflow_id), QUERY_GATE)
    except Exception as exc:
        raise ApiError(503, "temporal_unavailable", f"{exc}"[:500]) from None
    if gate:
        options = _open_gate(gate)["options"]
        if decision not in options:
            raise ApiError(
                422, "invalid_request", f"the open gate takes {options}, not {decision!r}"
            )
    return await _signal(
        app, row, SIGNAL_HUMAN_DECISION_V2, {"decision": decision, "by": by.strip()}
    )


@_route(write=True)
async def abort(request: Request) -> Response:
    from temporalio.service import RPCError, RPCStatusCode

    app, row = await _durable(request)
    body = await _body(request)
    _fields(body, set())
    client = await app.temporal.client()
    try:
        await client.get_workflow_handle(row.workflow_id).cancel()
    except RPCError as exc:
        if exc.status == RPCStatusCode.NOT_FOUND:
            raise ApiError(409, "finished", "the mission's workflow is not running") from None
        raise ApiError(503, "temporal_unavailable", f"{exc}"[:500]) from None
    app.temporal.forget(row.workflow_id or "")
    return JSONResponse({"accepted": True}, status_code=202)


@_route()
async def not_found(request: Request) -> Response:
    raise ApiError(404, "not_found", f"no route {request.url.path}")


#: The UI bundle (built from ui/ by ui/bundle.sh; the Go server embeds the same files).
UI_DIR = Path(__file__).parent / "ui"
_TOKEN_META = '<meta name="lha-token" content="" />'
_ASSET_NAME = re.compile(r"^[A-Za-z0-9._-]+$")
#: The page and its assets come from this server only; nothing may frame it; the start-up URL's
#: token never leaves in a Referer.
_PAGE_HEADERS = {
    "Content-Security-Policy": (
        "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "
        "connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
    ),
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
}
_SIGN_IN = (
    "<!doctype html><title>LHA</title><p>Open the URL <code>lha serve</code> printed "
    "(it carries the token) to use this page.</p>"
)


async def app_page(request: Request) -> Response:
    """The UI (``/``, ``/missions/...``): the start-up URL's ``?token=`` sets the cookie; the page
    carries the token for the UI's writes (X-LHA-Token), only to a browser that already has it."""
    app = _app(request)
    try:
        _guard_host(request)
    except ApiError as exc:
        return _error(exc.status, exc.code, exc.message)
    from_url = request.query_params.get("token", "")
    url_ok = bool(from_url) and hmac.compare_digest(from_url.encode(), app.token.encode())
    cookie = request.cookies.get("lha_token", "")
    if not url_ok and not hmac.compare_digest(cookie.encode(), app.token.encode()):
        return HTMLResponse(_SIGN_IN, status_code=401, headers=_PAGE_HEADERS)
    index = UI_DIR / "index.html"
    page = index.read_text(encoding="utf-8") if index.is_file() else _SIGN_IN
    page = page.replace(
        _TOKEN_META, f'<meta name="lha-token" content="{html.escape(app.token)}" />'
    )
    response = HTMLResponse(page, headers={**_PAGE_HEADERS, "Cache-Control": "no-store"})
    if url_ok:
        response.set_cookie("lha_token", app.token, httponly=True, samesite="strict", path="/")
    return response


async def asset(request: Request) -> Response:
    """A file of the UI bundle; its name carries its content hash, so it is cached for good."""
    try:
        _guard_host(request)
    except ApiError as exc:
        return _error(exc.status, exc.code, exc.message)
    name = request.path_params["name"]
    path = UI_DIR / "assets" / name
    if not _ASSET_NAME.match(name) or not path.is_file():
        return _error(404, "not_found", f"no asset {name!r}")
    kind = {".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml"}.get(
        path.suffix, "application/octet-stream"
    )
    return Response(
        path.read_bytes(),
        media_type=kind,
        headers={**_PAGE_HEADERS, "Cache-Control": "public, max-age=31536000, immutable"},
    )


#: Every API route (method, path, handler); a test checks they are exactly spec/serve/openapi.json.
API_ROUTES: list[tuple[str, str, Handler]] = [
    ("GET", "/api/v1/health", health),
    ("GET", "/api/v1/missions", list_missions),
    ("GET", "/api/v1/missions/{mission_id}", get_mission),
    ("GET", "/api/v1/missions/{mission_id}/items", list_items),
    ("GET", "/api/v1/missions/{mission_id}/events", list_events),
    ("GET", "/api/v1/missions/{mission_id}/costs", list_costs),
    ("GET", "/api/v1/missions/{mission_id}/gates", list_gates),
    ("GET", "/api/v1/stream", stream),
    ("POST", "/api/v1/missions/{mission_id}/steer", steer),
    ("POST", "/api/v1/missions/{mission_id}/snooze", snooze),
    ("POST", "/api/v1/missions/{mission_id}/checklist-edits", edit_checklist),
    ("POST", "/api/v1/missions/{mission_id}/decision", decide),
    ("POST", "/api/v1/missions/{mission_id}/abort", abort),
]


def create_app(app: App) -> Starlette:
    @contextlib.asynccontextmanager
    async def lifespan(_: Starlette) -> AsyncIterator[None]:
        await app.hub.start()
        try:
            yield
        finally:
            await app.hub.stop()

    routes = [Route(path, handler, methods=[method]) for method, path, handler in API_ROUTES]
    routes += [
        Route("/", app_page, methods=["GET"]),
        Route("/missions/{rest:path}", app_page, methods=["GET"]),
        Route("/assets/{name}", asset, methods=["GET"]),
        Route("/api/{rest:path}", not_found, methods=["GET", "POST", "PUT", "PATCH", "DELETE"]),
    ]
    starlette = Starlette(routes=routes, lifespan=lifespan)
    starlette.state.lha = app
    return starlette
