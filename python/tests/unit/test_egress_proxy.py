"""The sandbox egress proxy: allow-list policy, SSRF checks, and a real proxy on localhost.

No internet: DNS is faked with an injectable resolver, and the upstream TCP connection is
redirected to a local server by an injectable connector (which records the address the proxy
chose, proving it connects to the checked address rather than re-resolving).
"""

from __future__ import annotations

import asyncio
import os
import re
import subprocess
import sys
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass, field

import pytest

from lha.execution import egress_proxy
from lha.execution.egress_proxy import (
    AllowEntry,
    Denied,
    EgressProxy,
    check_host,
    is_public_address,
    parse_allow_list,
    resolve_checked,
)
from lha.safety.egress import is_public_address as reference_is_public_address

PUBLIC_IP = "93.184.215.14"


# --- pure policy ------------------------------------------------------------------------------


def test_parse_allow_list() -> None:
    entries = parse_allow_list(" pypi.org, .golang.org\nexample.com:8443  Registry.NPMJS.org. ")
    assert entries == (
        AllowEntry("pypi.org", suffix=False, port=None),
        AllowEntry("golang.org", suffix=True, port=None),
        AllowEntry("example.com", suffix=False, port=8443),
        AllowEntry("registry.npmjs.org", suffix=False, port=None),
    )
    assert parse_allow_list(["a.org", ""]) == (AllowEntry("a.org", suffix=False, port=None),)
    assert parse_allow_list("") == ()


@pytest.mark.parametrize(
    "bad", ["10.0.0.1", "[::1]", "https://pypi.org", "pypi.org:0", "pypi.org:http", "a b/c", "."]
)
def test_parse_allow_list_rejects_non_hostnames(bad: str) -> None:
    with pytest.raises(ValueError):
        parse_allow_list([bad])


def test_check_host_exact_and_suffix_matching() -> None:
    allow = parse_allow_list("pypi.org,.golang.org")
    assert check_host("pypi.org", 443, allow) == "pypi.org"
    assert check_host("PyPI.org.", 80, allow) == "pypi.org"
    assert check_host("proxy.golang.org", 443, allow) == "proxy.golang.org"
    assert check_host("golang.org", 443, allow) == "golang.org"
    for denied in ("files.pypi.org", "evilpypi.org", "pypi.org.evil.com", "evilgolang.org"):
        with pytest.raises(Denied, match="not in egress allow-list"):
            check_host(denied, 443, allow)


def test_check_host_denies_ip_literals_and_junk() -> None:
    allow = parse_allow_list("pypi.org")
    for host in ("151.101.0.223", "::1", "[2a04:4e42::223]", "127.0.0.1"):
        with pytest.raises(Denied, match="IP-literal"):
            check_host(host, 443, allow)
    for host in ("", "pypi.org\x00.evil", "pypi.org/x", "pÿpi.org"):
        with pytest.raises(Denied):
            check_host(host, 443, allow)


def test_check_host_ports() -> None:
    allow = parse_allow_list("pypi.org, example.com:8443")
    assert check_host("pypi.org", 80, allow) == "pypi.org"
    with pytest.raises(Denied, match="port not allowed"):
        check_host("pypi.org", 22, allow)
    assert check_host("example.com", 8443, allow) == "example.com"
    with pytest.raises(Denied, match="port not allowed"):
        check_host("example.com", 443, allow)  # only the listed port for a host:port entry


@pytest.mark.parametrize(
    "address",
    [
        "8.8.8.8",
        PUBLIC_IP,
        "127.0.0.1",
        "10.1.2.3",
        "172.17.0.1",
        "192.168.1.1",
        "169.254.169.254",
        "100.64.0.1",
        "224.0.0.1",
        "240.0.0.1",
        "0.0.0.0",
        "198.18.0.1",
        "::1",
        "::",
        "fe80::1",
        "fc00::1",
        "ff02::1",
        "::ffff:127.0.0.1",
        "::ffff:8.8.8.8",
        "2002:a00:1::",
        "2606:4700:4700::1111",
        "not-an-ip",
    ],
)
def test_is_public_address_mirrors_the_safety_module(address: str) -> None:
    assert is_public_address(address) == reference_is_public_address(address)


def _static_resolver(
    table: dict[str, list[str]], calls: list[str] | None = None
) -> Callable[[str, int], Awaitable[list[str]]]:
    async def resolve(host: str, port: int) -> list[str]:
        if calls is not None:
            calls.append(host)
        if host not in table:
            raise OSError("NXDOMAIN")
        return table[host]

    return resolve


async def test_resolve_checked_denies_any_non_public_address() -> None:
    resolver = _static_resolver(
        {"ok.org": [PUBLIC_IP], "mixed.org": [PUBLIC_IP, "10.0.0.5"], "none.org": []}
    )
    assert await resolve_checked("ok.org", 443, resolver) == [PUBLIC_IP]
    with pytest.raises(Denied, match=r"non-public address: 10\.0\.0\.5"):
        await resolve_checked("mixed.org", 443, resolver)
    with pytest.raises(Denied, match="did not resolve"):
        await resolve_checked("none.org", 443, resolver)
    with pytest.raises(Denied, match="cannot resolve"):
        await resolve_checked("missing.org", 443, resolver)


async def test_system_resolver_resolves_localhost() -> None:
    addresses = await egress_proxy.system_resolver("localhost", 80)
    assert addresses and all(not is_public_address(a) for a in addresses)


# --- the proxy server on localhost ------------------------------------------------------------


@dataclass
class _Upstream:
    """A local TCP server standing in for the internet: echoes, or answers one HTTP request."""

    mode: str = "echo"
    received: list[bytes] = field(default_factory=list)
    port: int = 0

    async def handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        if self.mode == "echo":
            while data := await reader.read(1024):
                self.received.append(data)
                writer.write(data)
                await writer.drain()
        else:
            head = await reader.readuntil(b"\r\n\r\n")
            self.received.append(head)
            body = b"hello from upstream"
            writer.write(
                b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s"
                % (len(body), body)
            )
            await writer.drain()
        writer.close()


@dataclass
class _Harness:
    port: int
    upstream: _Upstream
    connects: list[tuple[str, int]]
    resolved: list[str]


@pytest.fixture
async def harness() -> AsyncIterator[_Harness]:
    upstream = _Upstream()
    server = await asyncio.start_server(upstream.handle, "127.0.0.1", 0)
    upstream.port = server.sockets[0].getsockname()[1]
    connects: list[tuple[str, int]] = []
    resolved: list[str] = []

    async def connector(
        address: str, port: int
    ) -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
        connects.append((address, port))  # the address the proxy CHOSE; redirect to the fake
        return await asyncio.open_connection("127.0.0.1", upstream.port)

    table = {
        "pypi.org": [PUBLIC_IP],
        "internal.example.org": ["10.0.0.5"],
        "rebind.example.org": [PUBLIC_IP, "127.0.0.1"],
    }
    proxy = EgressProxy(
        "pypi.org,.example.org",
        resolver=_static_resolver(table, resolved),
        connector=connector,
    )
    _, port = await proxy.start("127.0.0.1", 0)
    try:
        yield _Harness(port=port, upstream=upstream, connects=connects, resolved=resolved)
    finally:
        await proxy.close()
        server.close()
        await server.wait_closed()


async def _exchange(
    port: int, request: bytes
) -> tuple[asyncio.StreamReader, asyncio.StreamWriter, bytes]:
    reader, writer = await asyncio.open_connection("127.0.0.1", port)
    writer.write(request)
    await writer.drain()
    head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=5)
    return reader, writer, head


async def _denied_body(port: int, request: bytes) -> tuple[bytes, str]:
    reader, writer, head = await _exchange(port, request)
    body = await asyncio.wait_for(reader.read(), timeout=5)
    writer.close()
    return head.split(b"\r\n", 1)[0], body.decode()


async def test_connect_to_allowed_host_is_tunnelled_to_the_checked_address(
    harness: _Harness,
) -> None:
    reader, writer, head = await _exchange(
        harness.port, b"CONNECT pypi.org:443 HTTP/1.1\r\nHost: pypi.org:443\r\n\r\n"
    )
    assert head.startswith(b"HTTP/1.1 200")
    writer.write(b"tls-bytes")
    await writer.drain()
    assert await asyncio.wait_for(reader.readexactly(9), timeout=5) == b"tls-bytes"
    writer.close()
    assert harness.connects == [(PUBLIC_IP, 443)]
    assert harness.resolved == ["pypi.org"]  # resolved once; the connect used that result


async def test_connect_to_host_outside_the_allow_list_is_403(harness: _Harness) -> None:
    status, body = await _denied_body(harness.port, b"CONNECT example.com:443 HTTP/1.1\r\n\r\n")
    assert status == b"HTTP/1.1 403 Forbidden"
    assert "not in egress allow-list: example.com" in body
    assert harness.connects == [] and harness.resolved == []


async def test_allowed_name_resolving_to_a_private_address_is_403(harness: _Harness) -> None:
    status, body = await _denied_body(
        harness.port, b"CONNECT internal.example.org:443 HTTP/1.1\r\n\r\n"
    )
    assert status == b"HTTP/1.1 403 Forbidden" and "non-public address: 10.0.0.5" in body
    status, body = await _denied_body(
        harness.port, b"CONNECT rebind.example.org:443 HTTP/1.1\r\n\r\n"
    )
    assert status == b"HTTP/1.1 403 Forbidden" and "127.0.0.1" in body
    assert harness.connects == []


@pytest.mark.parametrize(
    ("request_line", "reason"),
    [
        (b"CONNECT 127.0.0.1:443 HTTP/1.1", "IP-literal"),
        (b"CONNECT [::1]:443 HTTP/1.1", "IP-literal"),
        (b"CONNECT pypi.org:22 HTTP/1.1", "port not allowed"),
        (b"CONNECT pypi.org HTTP/1.1", "malformed authority"),
        (b"GET https://pypi.org/simple/ HTTP/1.1", "only CONNECT and absolute http://"),
        (b"GET /simple/ HTTP/1.1", "only CONNECT and absolute http://"),
        (b"GET http://user:pw@pypi.org/ HTTP/1.1", "credentials"),
        (b"GET http://169.254.169.254/latest/meta-data/ HTTP/1.1", "IP-literal"),
    ],
)
async def test_other_requests_are_refused(
    harness: _Harness, request_line: bytes, reason: str
) -> None:
    status, body = await _denied_body(harness.port, request_line + b"\r\nHost: x\r\n\r\n")
    assert status == b"HTTP/1.1 403 Forbidden"
    assert reason in body
    assert harness.connects == []


async def test_malformed_request_is_400(harness: _Harness) -> None:
    status, _ = await _denied_body(harness.port, b"NONSENSE\r\n\r\n")
    assert status == b"HTTP/1.1 400 Bad Request"


async def test_plain_http_is_forwarded_in_origin_form(harness: _Harness) -> None:
    harness.upstream.mode = "http"
    reader, writer, head = await _exchange(
        harness.port,
        b"GET http://pypi.org/simple/six/?q=1 HTTP/1.1\r\nHost: evil.internal\r\n"
        b"Proxy-Authorization: Basic eA==\r\nAccept: */*\r\n\r\n",
    )
    body = await asyncio.wait_for(reader.read(), timeout=5)
    writer.close()
    assert head.startswith(b"HTTP/1.1 200 OK") and body == b"hello from upstream"
    sent = harness.upstream.received[0].decode()
    assert sent.startswith("GET /simple/six/?q=1 HTTP/1.1\r\n")
    assert "Host: pypi.org\r\n" in sent and "evil.internal" not in sent
    assert "Proxy-Authorization" not in sent and "Accept: */*" in sent
    assert harness.connects == [(PUBLIC_IP, 80)]


async def test_unreachable_upstream_is_502() -> None:
    async def refuse(address: str, port: int) -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
        raise ConnectionRefusedError("refused")

    proxy = EgressProxy(
        ["pypi.org"], resolver=_static_resolver({"pypi.org": [PUBLIC_IP]}), connector=refuse
    )
    _, port = await proxy.start()
    try:
        status, body = await _denied_body(port, b"CONNECT pypi.org:443 HTTP/1.1\r\n\r\n")
    finally:
        await proxy.close()
    assert status == b"HTTP/1.1 502 Bad Gateway" and "unreachable" in body


def test_runs_standalone_as_a_script_from_its_source_alone() -> None:
    """What the proxy container does: ``python -c <source>`` with the env configuration."""
    source = egress_proxy.__file__
    assert source is not None
    with open(source, encoding="utf-8") as fh:
        code = fh.read()
    assert "from lha" not in code and "import lha" not in code  # self-contained
    env = {
        "PATH": os.environ.get("PATH", ""),
        "LHA_PROXY_ALLOW": "pypi.org",
        "LHA_PROXY_PORT": "0",
        "LHA_PROXY_BIND": "127.0.0.1",
    }
    proc = subprocess.Popen(
        [sys.executable, "-c", code], env=env, stderr=subprocess.PIPE, text=True
    )
    try:
        assert proc.stderr is not None
        line = proc.stderr.readline()
        match = re.search(r"listening on 127\.0\.0\.1:(\d+) allow=pypi\.org", line)
        assert match, line

        async def probe() -> tuple[bytes, str]:
            return await _denied_body(
                int(match.group(1)), b"CONNECT example.com:443 HTTP/1.1\r\n\r\n"
            )

        status, body = asyncio.run(probe())
        assert status == b"HTTP/1.1 403 Forbidden" and "example.com" in body
        assert "deny CONNECT example.com:443" in proc.stderr.readline()
    finally:
        proc.terminate()
        proc.wait(timeout=10)
