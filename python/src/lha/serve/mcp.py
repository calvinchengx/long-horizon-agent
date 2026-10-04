"""MCP over the UI API: the tools of ``mcp.json`` (a copy of spec/serve/mcp.json), each one
operation of spec/serve/openapi.json.

A tool call is sent through this server's own routes, in process, so a tool answers exactly what
its operation answers and is checked by the same contract: a 2xx response is the tool's
``structuredContent``; an error response is a tool result with ``isError`` and the API's error
body. There is no tool for answering a gate, aborting or editing the checklist (``not_tools``):
an agent must not approve its own irreversible actions, which is what the human gate is for.

Transports: ``lha serve``'s ``/mcp`` (Streamable HTTP answered with plain JSON: no SSE, ``GET``
is 405; it needs the token as ``X-LHA-Token`` or ``Authorization: Bearer``) and ``lha mcp``
(newline-delimited JSON-RPC on stdin and stdout).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any, TextIO
from urllib.parse import quote, unquote, urlencode

from starlette.types import ASGIApp, Message

#: The tools (generated from spec/serve/ by python/scripts/export_spec.py; a test checks the copy).
SPEC: dict[str, Any] = json.loads((Path(__file__).parent / "mcp.json").read_text(encoding="utf-8"))
TOOLS: dict[str, dict[str, Any]] = {t["name"]: t for t in SPEC["tools"]}
#: The protocol versions this server speaks, its own first.
PROTOCOL_VERSIONS = (SPEC["protocol_version"], "2025-03-26")

PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602


class _RpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def error(ident: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": ident, "error": {"code": code, "message": message}}


class Mcp:
    """Answers MCP messages by calling ``asgi`` (this server's routes) as ``host`` with ``token``."""

    def __init__(self, asgi: ASGIApp, *, host: str, token: str, version: str) -> None:
        self._asgi = asgi
        self._host = host
        self._token = token
        self._version = version

    async def handle(self, message: Any) -> dict[str, Any] | None:
        """The response to one message; ``None`` for a notification."""
        if not isinstance(message, dict):
            return error(None, INVALID_REQUEST, "a message must be one JSON-RPC object")
        ident = message.get("id")
        method = message.get("method")
        if message.get("jsonrpc") != "2.0" or not isinstance(method, str):
            return error(ident, INVALID_REQUEST, "not a JSON-RPC 2.0 request")
        if "id" not in message:
            return None  # a notification (notifications/initialized, cancelled, ...)
        params = message.get("params") or {}
        try:
            if not isinstance(params, dict):
                raise _RpcError(INVALID_PARAMS, "params must be an object")
            result = await self._dispatch(method, params)
        except _RpcError as exc:
            return error(ident, exc.code, exc.message)
        return {"jsonrpc": "2.0", "id": ident, "result": result}

    async def _dispatch(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        if method == "initialize":
            asked = params.get("protocolVersion")
            return {
                "protocolVersion": asked if asked in PROTOCOL_VERSIONS else PROTOCOL_VERSIONS[0],
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {**SPEC["server"], "version": self._version},
            }
        if method == "ping":
            return {}
        if method == "tools/list":
            keys = ("name", "title", "description", "inputSchema", "annotations")
            return {"tools": [{k: t[k] for k in keys} for t in SPEC["tools"]]}
        if method == "tools/call":
            return await self._call(params)
        raise _RpcError(METHOD_NOT_FOUND, f"no method {method!r}")

    async def _call(self, params: dict[str, Any]) -> dict[str, Any]:
        tool = TOOLS.get(params.get("name"))  # type: ignore[arg-type]
        if tool is None:
            raise _RpcError(INVALID_PARAMS, f"unknown tool: {params.get('name')!r}")
        arguments = params.get("arguments")
        arguments = {} if arguments is None else arguments
        if not isinstance(arguments, dict):
            raise _RpcError(INVALID_PARAMS, "arguments must be an object")
        where: dict[str, str] = tool["arguments"]
        unexpected = sorted(set(arguments) - set(where))
        if unexpected:
            raise _RpcError(INVALID_PARAMS, f"unexpected argument(s): {', '.join(unexpected)}")
        missing = [a for a in tool["inputSchema"]["required"] if a not in arguments]
        if missing:
            raise _RpcError(INVALID_PARAMS, f"missing argument(s): {', '.join(missing)}")
        path, query, body = tool["path"], {}, {}
        for name, value in arguments.items():
            if where[name] == "path":
                if not isinstance(value, str) or not value:
                    raise _RpcError(INVALID_PARAMS, f"{name} must be a non-empty string")
                path = path.replace("{" + name + "}", quote(value, safe=""))
            elif where[name] == "query":
                query[name] = _query_value(name, value)
            else:
                body[name] = value
        status, response = await self._request(
            tool["method"], path, query, body if tool["method"] == "POST" else None
        )
        failed = not 200 <= status < 300
        if failed:
            err = response.get("error", {}) if isinstance(response, dict) else {}
            text = f"{status} {err.get('code', '')}: {err.get('message', '')}"
        else:
            text = json.dumps(response, ensure_ascii=False)
        return {
            "content": [{"type": "text", "text": text}],
            "structuredContent": response,
            "isError": failed,
        }

    async def _request(
        self, method: str, path: str, query: dict[str, str], body: dict[str, Any] | None
    ) -> tuple[int, Any]:
        """``method path`` through the server's routes, as an authorized local client."""
        raw = json.dumps(body).encode() if body is not None else b""
        scope = {
            "type": "http",
            "asgi": {"version": "3.0"},
            "http_version": "1.1",
            "method": method,
            "scheme": "http",
            "path": unquote(path),
            "raw_path": path.encode(),
            "root_path": "",
            "query_string": urlencode(query).encode(),
            "headers": [
                (b"host", self._host.encode()),
                (b"x-lha-token", self._token.encode()),
                (b"content-type", b"application/json"),
                (b"content-length", str(len(raw)).encode()),
            ],
            "client": ("127.0.0.1", 0),
            "server": ("127.0.0.1", 0),
        }
        sent = False
        status, chunks = 500, []

        async def receive() -> Message:
            nonlocal sent
            if sent:
                return {"type": "http.disconnect"}
            sent = True
            return {"type": "http.request", "body": raw, "more_body": False}

        async def send(message: Message) -> None:
            nonlocal status
            if message["type"] == "http.response.start":
                status = message["status"]
            elif message["type"] == "http.response.body":
                chunks.append(message.get("body", b""))

        await self._asgi(scope, receive, send)
        data = b"".join(chunks)
        return status, json.loads(data) if data else None


def _query_value(name: str, value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return str(int(value)) if value.is_integer() else repr(value)
    if isinstance(value, str):
        return value
    raise _RpcError(INVALID_PARAMS, f"{name} must be a string or a number")


async def serve_stdio(mcp: Mcp, out: TextIO) -> None:
    """Newline-delimited JSON-RPC on stdin, one response per line on ``out``, until stdin ends."""
    import asyncio

    loop = asyncio.get_running_loop()
    while True:
        line = await loop.run_in_executor(None, sys.stdin.buffer.readline)
        if not line:
            return
        if not line.strip():
            continue
        try:
            message = json.loads(line)
        except (json.JSONDecodeError, UnicodeDecodeError):
            response: dict[str, Any] | None = error(None, PARSE_ERROR, "not JSON")
        else:
            response = await mcp.handle(message)
        if response is not None:
            out.write(json.dumps(response, ensure_ascii=False) + "\n")
            out.flush()
