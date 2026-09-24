"""DNS-rebinding defence: the web tools connect only to the addresses they vetted.

A rebinding resolver answers the egress check with a public address and every later lookup with
a private one. The tools resolve once per hop, pin the vetted addresses on a
``PinnedNetworkBackend`` and dial those, so the private answer is never used. A fake socket layer
records what was dialled, the TLS SNI and the bytes sent (the ``Host`` header).
"""

from __future__ import annotations

import ssl
from collections.abc import Iterable
from pathlib import Path

import httpcore
import pytest

from lha.contracts.tools import ToolContext
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools.web import FetchUrlTool, WebSearchTool
from lha.safety.egress import EgressPolicy
from lha.safety.pinned_http import PinnedNetworkBackend, PinnedTransport

PUBLIC_A = "93.184.216.34"
PUBLIC_B = "93.184.216.35"


class RebindingResolver:
    """First lookup of each host: its public address. Every later lookup: loopback."""

    def __init__(self, first: dict[str, str], later: str = "127.0.0.1") -> None:
        self.first = first
        self.later = later
        self.calls: list[str] = []

    async def __call__(self, host: str, port: int) -> list[str]:
        seen = host in self.calls
        self.calls.append(host)
        return [self.later] if seen else [self.first[host]]


class FakeStream(httpcore.AsyncNetworkStream):
    def __init__(self, backend: FakeSockets, address: str) -> None:
        self._backend = backend
        self._address = address
        self._response = backend.responses[address]
        self.sent = bytearray()

    async def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        chunk, self._response = self._response[:max_bytes], self._response[max_bytes:]
        return chunk

    async def write(self, buffer: bytes, timeout: float | None = None) -> None:
        self.sent += buffer
        self._backend.sent[self._address] = bytes(self.sent)

    async def aclose(self) -> None:
        return None

    async def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> httpcore.AsyncNetworkStream:
        self._backend.sni.append((self._address, server_hostname, ssl_context.check_hostname))
        return self

    def get_extra_info(self, info: str) -> object:
        return None


class FakeSockets(httpcore.AsyncNetworkBackend):
    """The socket layer under the pinning: records every dial; answers canned HTTP responses."""

    def __init__(self, responses: dict[str, bytes], refuse: Iterable[str] = ()) -> None:
        self.responses = responses
        self.refuse = set(refuse)
        self.dialled: list[str] = []
        self.sni: list[tuple[str, str | None, bool]] = []
        self.sent: dict[str, bytes] = {}

    async def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: object = None,
    ) -> httpcore.AsyncNetworkStream:
        self.dialled.append(host)
        if host in self.refuse:
            raise httpcore.ConnectError(f"refused {host}")
        return FakeStream(self, host)

    async def sleep(self, seconds: float) -> None:
        return None


def _http(status: str, body: str = "", headers: str = "") -> bytes:
    data = body.encode()
    return (
        f"HTTP/1.1 {status}\r\nContent-Length: {len(data)}\r\n{headers}"
        f"Content-Type: text/html\r\n\r\n"
    ).encode() + data


async def _ctx(tmp_path: Path) -> ToolContext:
    return ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))


@pytest.mark.asyncio
async def test_rebinding_resolver_cannot_reach_a_private_ip(tmp_path: Path) -> None:
    resolver = RebindingResolver({"a.test": PUBLIC_A})
    sockets = FakeSockets({PUBLIC_A: _http("200 OK", "<p>public page</p>")})
    tool = FetchUrlTool(
        egress_policy=EgressPolicy(allow_hosts={"a.test"}),
        resolver=resolver,
        network_backend=sockets,
    )
    first = await tool.run({"url": "https://a.test/page"}, await _ctx(tmp_path))
    assert first.ok and "public page" in first.content
    # The next fetch resolves again, gets loopback, and is refused before any connection.
    second = await tool.run({"url": "https://a.test/page"}, await _ctx(tmp_path))
    assert not second.ok and "non-public" in (second.error or "")
    # Only the vetted public address was ever dialled; never the name, never loopback.
    assert sockets.dialled == [PUBLIC_A]
    # TLS still names (and verifies) the hostname; the Host header is the hostname.
    assert sockets.sni == [(PUBLIC_A, "a.test", True)]
    assert b"\r\nHost: a.test\r\n" in sockets.sent[PUBLIC_A]


@pytest.mark.asyncio
async def test_each_fetch_resolves_once_and_dials_the_vetted_address(tmp_path: Path) -> None:
    resolver = RebindingResolver({"a.test": PUBLIC_A})
    sockets = FakeSockets({PUBLIC_A: _http("200 OK", "ok")})
    tool = FetchUrlTool(
        egress_policy=EgressPolicy(allow_hosts={"a.test"}),
        resolver=resolver,
        network_backend=sockets,
    )
    result = await tool.run({"url": "https://a.test/"}, await _ctx(tmp_path))
    assert result.ok
    # One lookup (the check); the connection did not look the name up again.
    assert resolver.calls == ["a.test"]
    assert sockets.dialled == [PUBLIC_A]


@pytest.mark.asyncio
async def test_every_redirect_hop_is_pinned(tmp_path: Path) -> None:
    resolver = RebindingResolver({"a.test": PUBLIC_A, "b.test": PUBLIC_B})
    sockets = FakeSockets(
        {
            PUBLIC_A: _http("302 Found", headers="Location: http://b.test/next\r\n"),
            PUBLIC_B: _http("200 OK", "<p>second hop</p>"),
        }
    )
    tool = FetchUrlTool(
        egress_policy=EgressPolicy(allow_hosts={"a.test", "b.test"}),
        resolver=resolver,
        network_backend=sockets,
    )
    result = await tool.run({"url": "https://a.test/"}, await _ctx(tmp_path))
    assert result.ok and "second hop" in result.content
    assert resolver.calls == ["a.test", "b.test"]
    assert sockets.dialled == [PUBLIC_A, PUBLIC_B]
    assert sockets.sni == [(PUBLIC_A, "a.test", True)]  # the http hop has no TLS
    assert b"\r\nHost: b.test\r\n" in sockets.sent[PUBLIC_B]


@pytest.mark.asyncio
async def test_redirect_to_a_host_resolving_privately_is_never_dialled(tmp_path: Path) -> None:
    resolver = RebindingResolver({"a.test": PUBLIC_A, "b.test": "10.0.0.7"})
    sockets = FakeSockets({PUBLIC_A: _http("302 Found", headers="Location: https://b.test/\r\n")})
    tool = FetchUrlTool(
        egress_policy=EgressPolicy(allow_hosts={"a.test", "b.test"}),
        resolver=resolver,
        network_backend=sockets,
    )
    result = await tool.run({"url": "https://a.test/"}, await _ctx(tmp_path))
    assert not result.ok and "non-public" in (result.error or "")
    assert sockets.dialled == [PUBLIC_A]


@pytest.mark.asyncio
async def test_web_search_dials_its_vetted_endpoint_address(tmp_path: Path) -> None:
    resolver = RebindingResolver({"api.tavily.com": PUBLIC_A})
    body = '{"results": [{"title": "T", "url": "https://x", "content": "snippet"}]}'
    sockets = FakeSockets({PUBLIC_A: _http("200 OK", body)})
    tool = WebSearchTool(api_key="k", resolver=resolver, network_backend=sockets)
    result = await tool.run({"query": "q"}, await _ctx(tmp_path))
    assert result.ok and "snippet" in result.content
    assert sockets.dialled == [PUBLIC_A]
    assert sockets.sni == [(PUBLIC_A, "api.tavily.com", True)]
    again = await tool.run({"query": "q"}, await _ctx(tmp_path))  # now resolves to loopback
    assert not again.ok and "non-public" in (again.error or "")
    assert sockets.dialled == [PUBLIC_A]


# --- PinnedNetworkBackend ---------------------------------------------------------------------


@pytest.mark.asyncio
async def test_backend_refuses_unpinned_hosts_and_private_literals() -> None:
    sockets = FakeSockets({})
    backend = PinnedNetworkBackend(sockets)
    with pytest.raises(httpcore.ConnectError, match="no vetted address"):
        await backend.connect_tcp("a.test", 443)
    with pytest.raises(httpcore.ConnectError, match="non-public"):
        await backend.connect_tcp("127.0.0.1", 80)
    with pytest.raises(httpcore.ConnectError, match="unix"):
        await backend.connect_unix_socket("/var/run/docker.sock")
    assert sockets.dialled == []
    with pytest.raises(ValueError, match="non-public"):
        backend.pin("a.test", ["93.184.216.34", "169.254.169.254"])
    with pytest.raises(ValueError, match="no addresses"):
        backend.pin("a.test", [])
    await backend.sleep(0)


@pytest.mark.asyncio
async def test_backend_dials_public_literals_and_falls_back_across_vetted_addresses() -> None:
    sockets = FakeSockets({PUBLIC_A: b"", PUBLIC_B: b""}, refuse={PUBLIC_A})
    backend = PinnedNetworkBackend(sockets)
    await backend.connect_tcp(PUBLIC_B, 80)
    backend.pin("A.Test.", [PUBLIC_A, PUBLIC_B])  # normalized like the policy's hosts
    await backend.connect_tcp("a.test", 443)
    assert sockets.dialled == [PUBLIC_B, PUBLIC_A, PUBLIC_B]
    assert backend.dialled[-2:] == [("a.test", PUBLIC_A, 443), ("a.test", PUBLIC_B, 443)]
    backend.pin("c.test", [PUBLIC_A])
    with pytest.raises(httpcore.ConnectError, match="refused"):
        await backend.connect_tcp("c.test", 443)


def test_pinned_transport_verifies_tls_and_uses_the_pinned_backend() -> None:
    backend = PinnedNetworkBackend()
    transport = PinnedTransport(backend)
    pool = transport._pool
    assert isinstance(pool, httpcore.AsyncConnectionPool)
    assert pool._network_backend is backend
    ctx = pool._ssl_context
    assert ctx is not None and ctx.check_hostname and ctx.verify_mode == ssl.CERT_REQUIRED
