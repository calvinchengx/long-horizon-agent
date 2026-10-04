"""The UI API conformance runner: every case in spec/serve/cases.json against a running server.

Black box: it seeds a mission store and anchors from spec/serve/fixture.json (the store schema
and the anchor format are shared contracts, so any implementation's server reads them), starts
the server under test, sends each case's request and checks the status, the error code, the body
(a subset) and the response's schema in spec/serve/openapi.json. See spec/serve/README.md.

- ``LHA_SERVE_CMD``: the server command (default: this Python's ``lha serve``), e.g.
  ``/tmp/lha serve`` for the Go binary.
- ``LHA_MCP_CMD``: the stdio MCP server command (default: this Python's ``lha mcp``); the MCP
  cases (spec/serve/mcp_cases.json) run over it and over the server's ``/mcp``.
- ``LHA_SERVE_TEMPORAL``: a Temporal address. Set, a fake mission workflow runs there
  (``fake_mission.py``) and the ``running`` cases run; unset, Temporal is unreachable and the
  ``absent`` cases run.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import shlex
import socket
import subprocess
import sys
import time
from collections.abc import Iterator
from pathlib import Path
from typing import Any
from urllib.parse import quote

import httpx
import pytest
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

from lha.contracts.state import Checklist, ChecklistItem
from lha.governor.cost import CostEntry
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import GateEvent, MissionEvent
from lha.state.mission_anchor import GitMissionAnchor
from tests.serve import fake_mission

SPEC = Path(__file__).resolve().parents[3] / "spec"
API = json.loads((SPEC / "serve/openapi.json").read_text())
CASES = json.loads((SPEC / "serve/cases.json").read_text())["cases"]
FIXTURE = json.loads((SPEC / "serve/fixture.json").read_text())
MCP = json.loads((SPEC / "serve/mcp.json").read_text())
MCP_CASES = json.loads((SPEC / "serve/mcp_cases.json").read_text())["cases"]
TOKEN = "conformance-token"
_API_URI = "https://lha.invalid/spec/serve/openapi.json"
_EVENTS_URI = "https://lha.invalid/spec/obs/mission_events.json"
_REGISTRY = Registry().with_resources(
    [
        (_API_URI, Resource.from_contents(API, default_specification=DRAFT202012)),
        (
            _EVENTS_URI,
            Resource.from_contents(
                json.loads((SPEC / "obs/mission_events.json").read_text()),
                default_specification=DRAFT202012,
            ),
        ),
    ]
)
TEMPORAL = os.environ.get("LHA_SERVE_TEMPORAL", "")
MODE = "running" if TEMPORAL else "absent"


# --- the server under test ------------------------------------------------------------------
def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


async def _seed(root: Path, db: Path) -> None:
    store = SqliteStore(db)
    await store.open()
    try:
        for mission in FIXTURE["missions"]:
            workdir = mission.get("workdir")
            if mission["anchor"] is not None:
                anchor = FIXTURE["anchors"][mission["anchor"]]
                path = root / mission["anchor"]
                path.mkdir()
                await GitMissionAnchor(str(path)).initialize(
                    title=anchor["title"],
                    description=anchor["description"],
                    items=Checklist(items=[ChecklistItem(**i) for i in anchor["items"]]),
                )
                workdir = str(path.resolve())
            await store.upsert_mission(
                mission_id=mission["mission_id"],
                title=mission["title"],
                description=mission["description"],
                status=mission["status"],
                workflow_id=mission["workflow_id"],
                workdir=workdir,
            )
        for n, cost in enumerate(FIXTURE["costs"]):
            usd = cost["usd"]
            entry = CostEntry(
                cycle_id=cost["cycle_id"],
                model=cost["model"],
                role=cost["role"],
                input_tokens=cost["input_tokens"],
                output_tokens=cost["output_tokens"],
                usd=usd or 0.0,
                cost_known=usd is not None,
            )
            await store.record_cost(cost["mission_id"], entry, call_key=f"fixture#{n}")
            time.sleep(0.002)  # distinct timestamps: the API lists the most recent call first
        for gate in FIXTURE["gates"]:
            await store.record_gate_event(GateEvent(**gate))
        await store.append_mission_events(
            [
                MissionEvent(e["mission_id"], e["cycle_id"], e["kind"], e["payload"], ts=e["ts"])
                for e in FIXTURE["events"]
            ]
        )
    finally:
        await store.close()


def _server_command() -> list[str]:
    configured = os.environ.get("LHA_SERVE_CMD")
    if configured:
        return shlex.split(configured)
    return [str(Path(sys.executable).parent / "lha"), "serve"]


class Server:
    def __init__(self, base: str, proc: subprocess.Popen[str], env: dict[str, str]) -> None:
        self.base = base
        self.proc = proc
        self.host = base.removeprefix("http://")
        self.env = env


@pytest.fixture(scope="module")
def fake_workflow() -> Iterator[None]:
    if not TEMPORAL:
        yield
        return
    proc = subprocess.Popen(
        [sys.executable, "-m", "tests.serve.fake_mission", TEMPORAL],
        cwd=Path(__file__).resolve().parents[2],
        stdout=subprocess.PIPE,
        text=True,
    )
    assert proc.stdout is not None
    line = proc.stdout.readline()
    assert line.strip() == "ready", f"fake mission workflow did not start: {line!r}"
    try:
        yield
    finally:
        proc.terminate()
        proc.wait(timeout=10)


@pytest.fixture(scope="module")
def server(tmp_path_factory: pytest.TempPathFactory, fake_workflow: None) -> Iterator[Server]:
    root = tmp_path_factory.mktemp("serve")
    db = root / "lha.sqlite3"
    asyncio.run(_seed(root, db))
    env = {k: v for k, v in os.environ.items() if not k.startswith("LHA_")}
    env.update(
        LHA_SQLITE_PATH=str(db),
        LHA_SERVE_TOKEN=TOKEN,
        LHA_TEMPORAL_ADDRESS=TEMPORAL or f"127.0.0.1:{_free_port()}",  # nothing listens there
    )
    proc = subprocess.Popen(
        [*_server_command(), "--host", "127.0.0.1", "--port", "0"],
        env=env,
        cwd=Path(__file__).resolve().parents[2],  # where coverage.py writes a Python server's data
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    assert proc.stdout is not None
    line = proc.stdout.readline()
    match = re.match(r"lha serve: (http://127\.0\.0\.1:\d+)/\?token=(\S+)$", line.strip())
    if match is None:
        proc.kill()
        _, err = proc.communicate(timeout=10)
        pytest.fail(f"the server did not announce itself: {line!r}\n{err[-2000:]}")
    assert match.group(2) == TOKEN
    try:
        yield Server(match.group(1), proc, env)
    finally:
        proc.terminate()
        try:
            _, err = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            _, err = proc.communicate()
        if err.strip():
            print(f"--- server stderr ---\n{err[-5000:]}")  # shown when a case failed


# --- matching ---------------------------------------------------------------------------------
def _subset(expected: Any, actual: Any, path: str = "$") -> list[str]:
    """Where ``actual`` differs from ``expected`` (see spec/serve/README.md for the markers)."""
    if expected == "$any":
        return []
    if isinstance(expected, dict) and set(expected) == {"$len"}:
        if not isinstance(actual, list) or len(actual) != expected["$len"]:
            return [f"{path}: expected a list of {expected['$len']}, got {_short(actual)}"]
        return []
    if isinstance(expected, dict) and set(expected) == {"$contains"}:
        if not isinstance(actual, list):
            return [f"{path}: expected a list, got {_short(actual)}"]
        return [
            f"{path}: no element matches {_short(want)}"
            for want in expected["$contains"]
            if not any(not _subset(want, item) for item in actual)
        ]
    if isinstance(expected, dict):
        if not isinstance(actual, dict):
            return [f"{path}: expected an object, got {_short(actual)}"]
        errors = []
        for key, want in expected.items():
            if key not in actual:
                errors.append(f"{path}: missing {key!r}")
            else:
                errors += _subset(want, actual[key], f"{path}.{key}")
        return errors
    if isinstance(expected, list):
        if not isinstance(actual, list) or len(actual) != len(expected):
            return [f"{path}: expected {len(expected)} elements, got {_short(actual)}"]
        return [e for i, w in enumerate(expected) for e in _subset(w, actual[i], f"{path}[{i}]")]
    if isinstance(expected, float) or isinstance(actual, float):
        ok = isinstance(actual, int | float) and abs(float(actual) - float(expected)) < 1e-9
        return [] if ok else [f"{path}: expected {expected!r}, got {actual!r}"]
    return [] if expected == actual else [f"{path}: expected {expected!r}, got {_short(actual)}"]


def _short(value: Any) -> str:
    text = json.dumps(value)
    return text if len(text) <= 300 else text[:300] + "..."


def _operation(op_id: str) -> tuple[str, str, dict[str, Any]]:
    for template, methods in API["paths"].items():
        for method, op in methods.items():
            if op["operationId"] == op_id:
                return template, method, op
    raise KeyError(op_id)


def _schema_errors(op_id: str, status: int, content_type: str, body: Any) -> list[str]:
    if op_id:
        template, method, op = _operation(op_id)
        content = op["responses"][str(status)]["content"]
        assert content_type in content, f"{content_type} is not documented for {op_id} {status}"
        pointer = "/".join(
            p.replace("~", "~0").replace("/", "~1")
            for p in ("paths", template, method, "responses", str(status), "content", content_type)
        )
        ref = f"{_API_URI}#/{quote(pointer)}/schema"
    else:
        ref = f"{_API_URI}#/components/schemas/Error"
    validator = Draft202012Validator({"$ref": ref}, registry=_REGISTRY)
    return [
        f"{'/'.join(map(str, e.absolute_path))}: {e.message}" for e in validator.iter_errors(body)
    ]


# --- requests ---------------------------------------------------------------------------------
def _headers(server: Server, request: dict[str, Any]) -> dict[str, str]:
    headers = {"Host": request.get("host", server.host), **request.get("headers", {})}
    auth = request.get("auth", "header")
    if auth == "header":
        headers["X-LHA-Token"] = TOKEN
    elif auth == "cookie":
        headers["Cookie"] = f"lha_token={TOKEN}"
    elif auth == "wrong":
        headers["X-LHA-Token"] = "not-the-token"
    elif auth == "bearer":
        headers["Authorization"] = f"Bearer {TOKEN}"
    return headers


def _send(server: Server, request: dict[str, Any]) -> httpx.Response:
    body = request.get("body")
    kwargs: dict[str, Any] = {}
    if body == "$not-json":
        kwargs["content"] = b"{not json"
        kwargs["headers"] = {"Content-Type": "application/json"}
    elif body is not None:
        kwargs["json"] = body
    headers = {**_headers(server, request), **kwargs.pop("headers", {})}
    return httpx.request(
        request["method"],
        server.base + request["path"],
        params=request.get("query"),
        headers=headers,
        timeout=30,
        **kwargs,
    )


def _read_stream(server: Server, request: dict[str, Any], want: int) -> list[dict[str, Any]]:
    """The first ``want`` ``mission_event`` messages (each as {"id", **data}), within 10s."""
    messages: list[dict[str, Any]] = []
    deadline = time.monotonic() + 10
    url = server.base + request["path"]
    with httpx.stream(
        "GET", url, params=request.get("query"), headers=_headers(server, request), timeout=10
    ) as resp:
        assert resp.status_code == 200, resp.read()
        assert resp.headers["content-type"].startswith("text/event-stream")
        event, data, ident = "", [], ""
        for line in resp.iter_lines():
            if line.startswith(":"):
                continue
            if line.startswith("event:"):
                event = line[6:].strip()
            elif line.startswith("data:"):
                data.append(line[5:].lstrip())
            elif line.startswith("id:"):
                ident = line[3:].strip()
            elif line == "":
                if event == "mission_event" and data:
                    payload = json.loads("\n".join(data))
                    assert ident == str(payload["id"]), "the SSE id must be the event's id"
                    errors = _schema_errors(
                        "listEvents",
                        200,
                        "application/json",
                        {"events": [payload], "next_after": payload["id"]},
                    )
                    assert not errors, errors
                    messages.append(payload)
                    if len(messages) >= want:
                        break
                event, data, ident = "", [], ""
            if time.monotonic() > deadline:
                break
    return messages


async def _signals() -> tuple[list[list[Any]], str]:
    from temporalio.client import Client

    client = await Client.connect(TEMPORAL)
    handle = client.get_workflow_handle(fake_mission.WORKFLOW_ID)
    status = (await handle.describe()).status
    received = (
        []
        if status and status.name != "RUNNING"
        else await handle.query(fake_mission.SIGNALS_QUERY)
    )
    return received, status.name if status else ""


def _check_signal(expected: dict[str, Any]) -> None:
    deadline = time.monotonic() + 10
    while True:
        received, status = asyncio.run(_signals())
        if expected["name"] == "$cancel":
            if status in ("CANCELED", "COMPLETED", "TERMINATED"):
                return
        elif [expected["name"], expected["arg"]] in received:
            return
        if time.monotonic() > deadline:
            pytest.fail(f"the workflow never got {expected}; it has {received} ({status})")
        time.sleep(0.2)


def _ordered(cases: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Every case, with the abort that ends the fake workflow last."""
    return sorted(cases, key=lambda c: c["expect"].get("signal", {}).get("name") == "$cancel")


# --- MCP (spec/serve/mcp.json, mcp_cases.json) --------------------------------------------------
def _mcp_command() -> list[str]:
    configured = os.environ.get("LHA_MCP_CMD")
    if configured:
        return shlex.split(configured)
    return [str(Path(sys.executable).parent / "lha"), "mcp"]


class Stdio:
    """``lha mcp`` on a pipe: one JSON-RPC message per line each way."""

    def __init__(self, proc: subprocess.Popen[str]) -> None:
        self.proc = proc
        self.pings = 0

    def send(self, body: Any) -> None:
        assert self.proc.stdin is not None
        line = "{not json" if body == "$not-json" else json.dumps(body)
        self.proc.stdin.write(line + "\n")
        self.proc.stdin.flush()

    def receive(self) -> dict[str, Any]:
        assert self.proc.stdout is not None
        line = self.proc.stdout.readline()
        assert line, f"lha mcp exited ({self.proc.poll()})"
        return json.loads(line)

    def no_response(self) -> None:
        """Nothing was answered: the next line answers a ping sent after the message."""
        self.pings += 1
        self.send({"jsonrpc": "2.0", "id": f"after-{self.pings}", "method": "ping"})
        assert self.receive()["id"] == f"after-{self.pings}"


@pytest.fixture(scope="module")
def stdio(server: Server) -> Iterator[Stdio]:
    proc = subprocess.Popen(
        _mcp_command(),
        env=server.env,
        cwd=Path(__file__).resolve().parents[2],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        yield Stdio(proc)
    finally:
        try:
            _, err = proc.communicate(timeout=10)  # closes stdin: lha mcp exits
        except subprocess.TimeoutExpired:
            proc.kill()
            _, err = proc.communicate()
        if err.strip():
            print(f"--- lha mcp stderr ---\n{err[-5000:]}")


def _mcp_tools_as_listed() -> list[dict[str, Any]]:
    keys = ("name", "title", "description", "inputSchema", "annotations")
    return [{k: t[k] for k in keys} for t in MCP["tools"]]


def _check_mcp(case: dict[str, Any], response: dict[str, Any]) -> None:
    expect = case["expect"]
    body = case["request"]["body"]
    assert response["jsonrpc"] == "2.0"
    if "id" in expect:
        assert response["id"] == expect["id"]
    elif isinstance(body, dict) and "id" in body:
        assert response["id"] == body["id"]
    if "error" in expect:
        assert "result" not in response, response
        errors = _subset(expect["error"], response["error"])
        assert not errors, (errors, response)
        return
    assert "error" not in response, response
    result = response["result"]
    if "result" in expect:
        want = expect["result"]
        if want.get("tools") == "$spec":
            assert result["tools"] == _mcp_tools_as_listed()
            want = {k: v for k, v in want.items() if k != "tools"}
        errors = _subset(want, result)
        assert not errors, (errors, result)
    if body.get("method") == "tools/call":
        tool = next(t for t in MCP["tools"] if t["name"] == body["params"]["name"])
        if "tool_error" in expect:
            assert result["isError"] is True, result
            assert result["structuredContent"]["error"]["code"] == expect["tool_error"], result
        content = result["structuredContent"]
        assert result["content"][0]["type"] == "text"
        if result["isError"]:
            assert not _schema_errors("", 0, "", content)
        else:
            # The tool answers exactly its operation's response.
            assert json.loads(result["content"][0]["text"]) == content
            status = 202 if tool["method"] == "POST" else 200
            errors = _schema_errors(tool["operation"], status, "application/json", content)
            assert not errors, errors
    if "signal" in expect:
        _check_signal(expect["signal"])


def _mcp_cases(transport: str) -> list[dict[str, Any]]:
    return [c for c in MCP_CASES if c.get("transport", transport) == transport]


@pytest.mark.parametrize("case", _mcp_cases("http"), ids=[c["name"] for c in _mcp_cases("http")])
def test_mcp_http(server: Server, case: dict[str, Any]) -> None:
    if case.get("temporal") not in (None, MODE):
        pytest.skip(f"needs Temporal {case['temporal']}")
    request = case["request"]
    method = request.get("method", "POST")
    headers = _headers(server, request)
    if request.get("auth", "header") == "bearer":
        headers.pop("X-LHA-Token", None)
    body = request["body"]
    content = b"{not json" if body == "$not-json" else json.dumps(body).encode()
    if method == "POST":
        headers["Content-Type"] = "application/json"
        headers["Accept"] = "application/json, text/event-stream"
    resp = httpx.request(
        method,
        server.base + "/mcp",
        headers=headers,
        content=content if method == "POST" else None,
        timeout=30,
    )
    expect = case["expect"]
    assert resp.status_code == expect.get("status", 200), resp.text[:1000]
    if expect.get("no_response") or resp.status_code in (401, 403, 405):
        if resp.status_code in (401, 403):
            assert not _schema_errors("", 0, "", resp.json())
        if expect.get("no_response"):
            assert not resp.content
        return
    assert resp.headers["content-type"].startswith("application/json")
    _check_mcp(case, resp.json())


@pytest.mark.parametrize("case", _mcp_cases("stdio"), ids=[c["name"] for c in _mcp_cases("stdio")])
def test_mcp_stdio(stdio: Stdio, case: dict[str, Any]) -> None:
    if case.get("temporal") not in (None, MODE):
        pytest.skip(f"needs Temporal {case['temporal']}")
    stdio.send(case["request"]["body"])
    if case["expect"].get("no_response"):
        stdio.no_response()
        return
    _check_mcp(case, stdio.receive())


@pytest.mark.parametrize("case", _ordered(CASES), ids=[c["name"] for c in _ordered(CASES)])
def test_case(server: Server, case: dict[str, Any]) -> None:
    if case.get("temporal") not in (None, MODE):
        pytest.skip(f"needs Temporal {case['temporal']}")
    expect = case["expect"]
    if "stream" in expect:
        want = expect["stream"]["mission_event"]
        got = _read_stream(server, case["request"], len(want))
        errors = _subset(want, got)
        assert not errors, errors
        return
    resp = _send(server, case["request"])
    body = resp.json() if resp.content else None
    assert resp.status_code == expect["status"], f"{resp.status_code}: {resp.text[:1000]}"
    content_type = resp.headers.get("content-type", "").split(";")[0]
    errors = _schema_errors(case["operation"], resp.status_code, content_type, body)
    assert not errors, errors
    if "error" in expect:
        assert body["error"]["code"] == expect["error"], body
    if "body" in expect:
        errors = _subset(expect["body"], body)
        assert not errors, errors
    if "signal" in expect:
        _check_signal(expect["signal"])


def test_the_startup_url_signs_the_browser_in_and_serves_the_ui(server: Server) -> None:
    """The UI is served by every implementation alike (spec/serve/README.md, "The UI")."""
    host = {"Host": server.host}
    resp = httpx.get(f"{server.base}/?token={TOKEN}", headers=host, timeout=10)
    assert resp.status_code == 200 and resp.headers["content-type"].startswith("text/html")
    cookie = resp.headers.get("set-cookie", "")
    assert f"lha_token={TOKEN}" in cookie
    assert "samesite=strict" in cookie.lower() and "httponly" in cookie.lower()
    # The page carries the token for the UI's writes, and its headers keep it in this origin.
    assert f'<meta name="lha-token" content="{TOKEN}"' in resp.text
    assert resp.headers["referrer-policy"] == "no-referrer"
    assert "frame-ancestors 'none'" in resp.headers["content-security-policy"]
    assert resp.headers["cache-control"] == "no-store"
    assets = re.findall(r'(?:src|href)="(/assets/[^"]+)"', resp.text)
    assert any(a.endswith(".js") for a in assets) and any(a.endswith(".css") for a in assets)
    for path in assets:
        got = httpx.get(server.base + path, headers=host, timeout=10)
        assert got.status_code == 200 and got.content
        assert got.headers["content-type"].startswith(
            "text/javascript" if path.endswith(".js") else "text/css"
        )
        assert "immutable" in got.headers["cache-control"]
    # A deep link works with the cookie; without the token or cookie the page has no token.
    cookies = {"lha_token": TOKEN}
    deep = httpx.get(f"{server.base}/missions/mission_local", headers=host, cookies=cookies)
    assert deep.status_code == 200 and f'content="{TOKEN}"' in deep.text
    anonymous = httpx.get(f"{server.base}/missions/mission_local", headers=host, timeout=10)
    assert anonymous.status_code == 401 and TOKEN not in anonymous.text
    wrong = httpx.get(f"{server.base}/?token=not-the-token", headers=host, timeout=10)
    assert wrong.status_code == 401 and "set-cookie" not in wrong.headers
    foreign = httpx.get(f"{server.base}/?token={TOKEN}", headers={"Host": "evil.example:80"})
    assert foreign.status_code == 403 and TOKEN not in foreign.text
    assert httpx.get(f"{server.base}/assets/nope.js", headers=host).status_code == 404


def test_the_server_registers_exactly_the_documented_routes() -> None:
    """No undocumented route: the API routes are exactly spec/serve/openapi.json's operations."""
    from lha.serve.app import API_ROUTES

    documented = {
        (method.upper(), template) for template, ops in API["paths"].items() for method in ops
    }
    assert {(method, path) for method, path, _ in API_ROUTES} == documented
