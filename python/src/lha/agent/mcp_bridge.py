"""A minimal in-process MCP server that hands LHA's tools to a ``claude -p`` session.

The ``claude_code`` lead engine switches Claude Code's built-in tools off and gives it these
instead, so every file write and command still goes through LHA's dispatcher: the sandbox, the
path rules, the irreversible-command gate and the egress policy apply exactly as they do to the
built-in loop.

It speaks MCP's Streamable HTTP transport in its simplest form: JSON-RPC over ``POST``, answered
with a plain ``application/json`` body (no SSE stream; ``GET`` is 405). It listens on 127.0.0.1 on
a random port for the length of one cycle and requires a random bearer token, so only the
``claude`` process LHA started (which gets the token in its ``--mcp-config``) can call it.

Tool calls are served one at a time: the sandbox session is not built for concurrent commands.
Methods: ``initialize``, ``ping``, ``tools/list`` and ``tools/call``; notifications get 202.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import secrets
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from types import TracebackType

import structlog

_log = structlog.get_logger("lha.mcp_bridge")

PROTOCOL_VERSION = "2025-06-18"
_MAX_BODY = 8 * 1024 * 1024
_MAX_HEADER_LINES = 100

# A tool handler takes the call's arguments and returns (text for the model, is_error).
Handler = Callable[[dict[str, object]], Awaitable[tuple[str, bool]]]


@dataclass(frozen=True)
class BridgeTool:
    name: str
    description: str
    input_schema: dict[str, object]
    handler: Handler


class _BadRequest(Exception):
    pass


def _schema(parameters: dict[str, object]) -> dict[str, object]:
    """A JSON Schema MCP accepts: an object schema, even for a tool with no parameters."""
    schema = dict(parameters) if parameters else {}
    schema.setdefault("type", "object")
    schema.setdefault("properties", {})
    return schema


class McpBridge:
    """Serve ``tools`` over MCP on 127.0.0.1 while the ``async with`` block runs."""

    def __init__(self, tools: list[BridgeTool], *, name: str = "lha", version: str = "0") -> None:
        self.name = name
        self._version = version
        self._tools = {t.name: t for t in tools}
        self._token = secrets.token_urlsafe(32)
        self._lock = asyncio.Lock()
        self._server: asyncio.Server | None = None
        self._port = 0
        self.calls = 0

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self._port}/mcp"

    def mcp_config(self) -> dict[str, object]:
        """The ``--mcp-config`` JSON that points Claude Code at this server."""
        return {
            "mcpServers": {
                self.name: {
                    "type": "http",
                    "url": self.url,
                    "headers": {"Authorization": f"Bearer {self._token}"},
                }
            }
        }

    def allowed_tools(self) -> list[str]:
        """``--allowedTools`` entries that pre-approve every bridged tool."""
        return [f"mcp__{self.name}__{name}" for name in self._tools]

    def opencode_server(self) -> dict[str, object]:
        """The ``mcp.servers.<name>`` entry that points OpenCode at this server.

        ``codemode`` is off so the tools keep their native ``<server>_<tool>`` names and the agent
        calls them directly (``tools.<server>.<tool>`` is Code Mode's JavaScript shape).
        """
        return {
            "type": "remote",
            "url": self.url,
            "headers": {"Authorization": f"Bearer {self._token}"},
            "codemode": False,
        }

    async def __aenter__(self) -> McpBridge:
        self._server = await asyncio.start_server(self._serve, host="127.0.0.1", port=0)
        self._port = self._server.sockets[0].getsockname()[1]
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    # --- HTTP --------------------------------------------------------------------------
    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            while True:
                request = await self._read_request(reader)
                if request is None:
                    break
                method, headers, body = request
                status, payload = await self._handle_http(method, headers, body)
                await self._respond(writer, status, payload)
                if headers.get("connection", "").lower() == "close":
                    break
        except (_BadRequest, ValueError) as exc:
            await self._respond(writer, 400, {"error": str(exc)})
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()
            with contextlib.suppress(ConnectionError):
                await writer.wait_closed()

    async def _read_request(
        self, reader: asyncio.StreamReader
    ) -> tuple[str, dict[str, str], bytes] | None:
        line = await reader.readline()
        if not line:
            return None
        parts = line.decode("latin-1").split()
        if len(parts) < 2:
            raise _BadRequest("malformed request line")
        method = parts[0].upper()
        headers: dict[str, str] = {}
        for _ in range(_MAX_HEADER_LINES):
            raw = await reader.readline()
            text = raw.decode("latin-1").rstrip("\r\n")
            if not text:
                break
            key, _, value = text.partition(":")
            headers[key.strip().lower()] = value.strip()
        else:
            raise _BadRequest("too many headers")
        if headers.get("transfer-encoding", "").lower() == "chunked":
            body = await self._read_chunked(reader)
        else:
            length = int(headers.get("content-length") or 0)
            if length > _MAX_BODY:
                raise _BadRequest("body too large")
            body = await reader.readexactly(length) if length else b""
        return method, headers, body

    @staticmethod
    async def _read_chunked(reader: asyncio.StreamReader) -> bytes:
        body = bytearray()
        while True:
            size = int((await reader.readline()).split(b";")[0].strip() or b"0", 16)
            if size == 0:
                await reader.readline()  # the blank line after the last chunk (no trailers)
                return bytes(body)
            body += await reader.readexactly(size)
            await reader.readexactly(2)
            if len(body) > _MAX_BODY:
                raise _BadRequest("body too large")

    @staticmethod
    async def _respond(writer: asyncio.StreamWriter, status: int, payload: object | None) -> None:
        reason = {200: "OK", 202: "Accepted", 400: "Bad Request", 401: "Unauthorized"}.get(
            status, "Method Not Allowed"
        )
        body = b"" if payload is None else json.dumps(payload).encode()
        head = f"HTTP/1.1 {status} {reason}\r\nContent-Length: {len(body)}\r\n"
        if payload is not None:
            head += "Content-Type: application/json\r\n"
        writer.write(head.encode() + b"\r\n" + body)
        await writer.drain()

    async def _handle_http(
        self, method: str, headers: dict[str, str], body: bytes
    ) -> tuple[int, object | None]:
        expected = f"Bearer {self._token}".encode()
        if not secrets.compare_digest(headers.get("authorization", "").encode(), expected):
            return 401, {"error": "unauthorized"}
        if method == "DELETE":
            return 200, None  # session end; there is no session state to drop
        if method != "POST":
            return 405, None
        message = json.loads(body or b"null")
        if isinstance(message, list):  # a JSON-RPC batch
            replies = [r for r in [await self._rpc(m) for m in message] if r is not None]
            return (200, replies) if replies else (202, None)
        reply = await self._rpc(message)
        return (200, reply) if reply is not None else (202, None)

    # --- JSON-RPC ------------------------------------------------------------------------
    async def _rpc(self, message: object) -> dict[str, object] | None:
        if not isinstance(message, dict) or not isinstance(message.get("method"), str):
            return _error(None, -32600, "invalid request")
        msg_id = message.get("id")
        method = str(message["method"])
        if msg_id is None:  # a notification (initialized, cancelled, ...): nothing to answer
            return None
        params = message.get("params")
        params = params if isinstance(params, dict) else {}
        if method == "initialize":
            version = params.get("protocolVersion")
            return _ok(
                msg_id,
                {
                    "protocolVersion": version if isinstance(version, str) else PROTOCOL_VERSION,
                    "capabilities": {"tools": {"listChanged": False}},
                    "serverInfo": {"name": self.name, "version": self._version},
                },
            )
        if method == "ping":
            return _ok(msg_id, {})
        if method == "tools/list":
            tools = [
                {"name": t.name, "description": t.description, "inputSchema": t.input_schema}
                for t in self._tools.values()
            ]
            return _ok(msg_id, {"tools": tools})
        if method == "tools/call":
            return _ok(msg_id, await self._call(params))
        return _error(msg_id, -32601, f"method not found: {method}")

    async def _call(self, params: dict[str, object]) -> dict[str, object]:
        name = params.get("name")
        tool = self._tools.get(name) if isinstance(name, str) else None
        if tool is None:
            return _text(f"unknown tool: {name!r}", is_error=True)
        arguments = params.get("arguments")
        async with self._lock:
            self.calls += 1
            try:
                text, is_error = await tool.handler(
                    arguments if isinstance(arguments, dict) else {}
                )
            except Exception as exc:  # a handler bug must reach the model, not kill the server
                _log.warning("bridge_tool_failed", tool=tool.name, error=repr(exc))
                text, is_error = f"tool {tool.name} failed: {type(exc).__name__}: {exc}", True
        return _text(text, is_error=is_error)


def _ok(msg_id: object, result: dict[str, object]) -> dict[str, object]:
    return {"jsonrpc": "2.0", "id": msg_id, "result": result}


def _error(msg_id: object, code: int, message: str) -> dict[str, object]:
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


def _text(text: str, *, is_error: bool) -> dict[str, object]:
    return {"content": [{"type": "text", "text": text}], "isError": is_error}


def bridge_tool(
    name: str, description: str, parameters: dict[str, object], handler: Handler
) -> BridgeTool:
    return BridgeTool(name, description, _schema(parameters), handler)
