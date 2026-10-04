"""``lha serve``: the UI API every LHA implementation provides (spec/serve/openapi.json)."""

from __future__ import annotations

import asyncio
import contextlib
import secrets
import socket
import sys

from lha.config import Settings

#: Hosts ``lha serve`` may bind: loopback only (the API has no TLS and a single token).
LOOPBACK = ("127.0.0.1", "localhost")


def serve(
    settings: Settings, *, host: str = "127.0.0.1", port: int = 8765, token: str = ""
) -> None:
    """Serve until interrupted. Prints ``lha serve: http://127.0.0.1:<port>/?token=<token>`` first."""
    import uvicorn

    from lha.persistence.store import open_store
    from lha.serve.app import App, create_app

    if host not in LOOPBACK:
        raise ValueError(f"lha serve binds to loopback only, not {host!r}")
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", port))
    bound = int(sock.getsockname()[1])
    token = token or secrets.token_urlsafe(24)

    async def _main() -> None:
        store = await open_store(settings)
        try:
            app = App(settings=settings, store=store, token=token, port=bound)
            config = uvicorn.Config(create_app(app), log_level="warning", access_log=False)
            server = uvicorn.Server(config)
            print(f"lha serve: http://127.0.0.1:{bound}/?token={token}", flush=True)
            await server.serve(sockets=[sock])
        finally:
            await store.close()

    with contextlib.suppress(KeyboardInterrupt):  # Ctrl-C: a clean stop
        asyncio.run(_main())


def mcp_stdio(settings: Settings) -> None:
    """``lha mcp``: spec/serve/mcp.json's tools on stdin and stdout until stdin ends. Everything
    else that would print goes to stderr: stdout carries only the protocol."""
    from lha.persistence.store import open_store
    from lha.serve.app import App, create_app
    from lha.serve.mcp import serve_stdio

    out, sys.stdout = sys.stdout, sys.stderr

    async def _main() -> None:
        store = await open_store(settings)
        try:
            app = App(settings=settings, store=store, token=secrets.token_urlsafe(24))
            await serve_stdio(create_app(app).state.mcp, out)
        finally:
            await store.close()

    with contextlib.suppress(KeyboardInterrupt):  # Ctrl-C: a clean stop
        asyncio.run(_main())
