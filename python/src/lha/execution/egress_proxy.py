"""A tiny allow-list HTTP forward proxy: the egress gate for the Docker sandbox.

When the operator lists egress hosts (``LHA_SANDBOX_EGRESS``), the sandbox container is attached
ONLY to an ``--internal`` Docker network (no route out), and this proxy — running in its own
container on both that network and the default bridge — is the one way out. Package managers
(``pip``/``uv``/``go``/``npm``) honour ``HTTP(S)_PROXY``; anything that ignores it simply has no
route. The policy is enforced here, by infrastructure, not by the agent's goodwill:

- a host is allowed iff it equals an allow-list entry, or is a subdomain of an entry written with
  a leading dot (``.golang.org`` allows ``proxy.golang.org`` and ``golang.org``);
- IP-literal hosts are always denied (the allow-list names hosts, not addresses);
- the host is resolved and denied if ANY address is non-public (loopback, private, link-local,
  CGNAT, multicast, reserved, unspecified; IPv4-mapped/6to4/Teredo addresses are unwrapped), which
  keeps the sandbox away from the Docker host, cloud metadata and internal services;
- the proxy connects to the address it checked, never re-resolving (no DNS-rebinding window);
- ports 443 and 80 are allowed; any other port only via an explicit ``host:port`` entry;
- ``CONNECT`` (HTTPS, end-to-end TLS — the proxy never sees plaintext) and absolute-URI plain
  HTTP requests are supported; everything else is refused. Denials get ``403`` + a short reason.

One log line per request (allowed/denied) goes to stderr.

This file is deliberately self-contained (stdlib only, no ``lha`` imports): its SOURCE is shipped
into the proxy container and run as ``python -c <source>``. Standalone configuration comes from
the environment: ``LHA_PROXY_ALLOW`` (comma/whitespace separated entries), ``LHA_PROXY_PORT``
(default 3128) and ``LHA_PROXY_BIND`` (default ``0.0.0.0``). ``is_public_address`` mirrors
``lha.safety.egress.is_public_address``.
"""

from __future__ import annotations

import asyncio
import contextlib
import ipaddress
import logging
import os
import re
import socket
import sys
from collections.abc import Awaitable, Callable, Iterable
from dataclasses import dataclass
from urllib.parse import urlsplit

DEFAULT_PORT = 3128
DEFAULT_PORTS = frozenset({80, 443})
READY_MESSAGE = "lha-egress-proxy listening"

_MAX_HEADER_BYTES = 64 * 1024
_HEADER_TIMEOUT_S = 30.0
_CONNECT_TIMEOUT_S = 10.0
_PIPE_CHUNK = 65_536
_HOST_RE = re.compile(r"^[a-z0-9_]([a-z0-9_-]{0,62})(\.[a-z0-9_]([a-z0-9_-]{0,62}))*$")
_HOP_BY_HOP = frozenset(
    {
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "proxy-connection",
        "te",
        "trailer",
        "upgrade",
    }
)

log = logging.getLogger("lha.egress_proxy")

# (host, port) -> list of IP address strings.
Resolver = Callable[[str, int], Awaitable[list[str]]]
# (address, port) -> an open stream pair (``asyncio.open_connection``; tests inject a fake).
Connector = Callable[[str, int], Awaitable[tuple[asyncio.StreamReader, asyncio.StreamWriter]]]


async def tcp_connector(
    address: str, port: int
) -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
    return await asyncio.open_connection(address, port)


class Denied(Exception):
    """A request the policy refuses; ``str(exc)`` is the short reason sent back with the 403."""


@dataclass(frozen=True)
class AllowEntry:
    host: str  # normalized, without the leading dot
    suffix: bool  # True for ".example.org" (the domain and all subdomains)
    port: int | None  # None = the default ports (80/443)

    def matches_host(self, host: str) -> bool:
        if host == self.host:
            return True
        return self.suffix and host.endswith("." + self.host)


def _clean_host(host: str) -> str:
    return host.strip().strip("[]").rstrip(".").lower()


def _is_ip_literal(host: str) -> bool:
    try:
        ipaddress.ip_address(host.split("%", 1)[0])
    except ValueError:
        return False
    return True


def parse_allow_list(spec: str | Iterable[str]) -> tuple[AllowEntry, ...]:
    """Parse ``"pypi.org, .golang.org, example.com:8443"`` (or an iterable of entries).

    Raises ``ValueError`` for an entry that is not a plain DNS name (an IP literal, a URL, a bad
    port) so a typo is loud instead of silently allowing nothing (or too much).
    """
    items = re.split(r"[,\s]+", spec) if isinstance(spec, str) else list(spec)
    entries: list[AllowEntry] = []
    for raw in items:
        item = raw.strip()
        if not item:
            continue
        port: int | None = None
        host = item
        if ":" in item:
            host, _, port_s = item.rpartition(":")
            if not port_s.isdigit() or not 0 < int(port_s) < 65536:
                raise ValueError(f"invalid port in egress allow-list entry {raw!r}")
            port = int(port_s)
        suffix = host.startswith(".")
        host = _clean_host(host.lstrip("."))
        if not host or _is_ip_literal(host) or not _HOST_RE.match(host):
            raise ValueError(f"invalid egress allow-list entry {raw!r} (expected a DNS name)")
        entries.append(AllowEntry(host=host, suffix=suffix, port=port))
    return tuple(entries)


def check_host(host: str, port: int, allow: Iterable[AllowEntry]) -> str:
    """The pure name/port policy: return the normalized host, or raise ``Denied``."""
    name = _clean_host(host)
    if not name:
        raise Denied("missing host")
    if _is_ip_literal(name):
        raise Denied(f"IP-literal hosts are not allowed: {name}")
    if not name.isascii() or not _HOST_RE.match(name):
        raise Denied(f"invalid host: {host!r}")
    matching = [entry for entry in allow if entry.matches_host(name)]
    if not matching:
        raise Denied(f"host not in egress allow-list: {name}")
    for entry in matching:
        if (entry.port is None and port in DEFAULT_PORTS) or entry.port == port:
            return name
    raise Denied(f"port not allowed for {name}: {port}")


def is_public_address(address: str) -> bool:
    """True only for globally routable unicast addresses (v4-mapped/6to4-embedded v4 unwrapped)."""
    try:
        ip = ipaddress.ip_address(address.split("%", 1)[0])
    except ValueError:
        return False
    if isinstance(ip, ipaddress.IPv6Address):
        embedded = ip.ipv4_mapped or ip.sixtofour
        if embedded is not None:
            ip = embedded
        elif ip.teredo is not None:
            ip = ip.teredo[1]
    return not (
        ip.is_private
        or ip.is_loopback
        or ip.is_link_local
        or ip.is_multicast
        or ip.is_reserved
        or ip.is_unspecified
        or not ip.is_global
    )


async def system_resolver(host: str, port: int) -> list[str]:
    """Resolve ``host`` with the OS resolver (all address families, de-duplicated, v4 first)."""
    infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
    seen: dict[str, int] = {}
    for family, *_rest, sockaddr in infos:
        seen.setdefault(str(sockaddr[0]), 0 if family == socket.AF_INET else 1)
    return sorted(seen, key=lambda addr: seen[addr])


async def resolve_checked(host: str, port: int, resolver: Resolver) -> list[str]:
    """Resolve ``host`` and return its addresses, or raise ``Denied`` unless ALL are public."""
    try:
        addresses = await resolver(host, port)
    except OSError as exc:
        raise Denied(f"cannot resolve {host}") from exc
    if not addresses:
        raise Denied(f"{host} did not resolve")
    blocked = [addr for addr in addresses if not is_public_address(addr)]
    if blocked:
        raise Denied(f"{host} resolves to a non-public address: {blocked[0]}")
    return addresses


@dataclass(frozen=True)
class _Request:
    method: str
    target: str
    version: str
    headers: list[tuple[str, str]]


async def _read_head(reader: asyncio.StreamReader) -> _Request | None:
    try:
        raw = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=_HEADER_TIMEOUT_S)
    except (asyncio.IncompleteReadError, asyncio.LimitOverrunError, TimeoutError):
        return None
    lines = raw.decode("latin-1").split("\r\n")
    parts = lines[0].split(" ")
    if len(parts) != 3:
        return None
    headers: list[tuple[str, str]] = []
    for line in lines[1:]:
        if not line:
            continue
        name, sep, value = line.partition(":")
        if not sep:
            return None
        headers.append((name.strip(), value.strip()))
    return _Request(method=parts[0].upper(), target=parts[1], version=parts[2], headers=headers)


def _split_authority(authority: str) -> tuple[str, int]:
    """``host:port`` (brackets for IPv6) -> (host, port); raises ``Denied`` when malformed."""
    try:
        parts = urlsplit("//" + authority)
        host, port = parts.hostname, parts.port
    except ValueError as exc:
        raise Denied(f"malformed authority: {authority!r}") from exc
    if not host or port is None or parts.path or parts.username is not None:
        raise Denied(f"malformed authority: {authority!r}")
    return host, port


async def _respond(writer: asyncio.StreamWriter, status: str, reason: str) -> None:
    body = (reason + "\n").encode("utf-8", errors="replace")
    head = (
        f"HTTP/1.1 {status}\r\nContent-Type: text/plain; charset=utf-8\r\n"
        f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n"
    )
    with contextlib.suppress(OSError):
        writer.write(head.encode("latin-1") + body)
        await writer.drain()


async def _pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while chunk := await reader.read(_PIPE_CHUNK):
            writer.write(chunk)
            await writer.drain()
    except (OSError, asyncio.IncompleteReadError):
        pass
    finally:
        with contextlib.suppress(OSError, RuntimeError):
            if writer.can_write_eof():
                writer.write_eof()


async def _open_upstream(
    addresses: list[str], port: int, connector: Connector
) -> tuple[asyncio.StreamReader, asyncio.StreamWriter, str]:
    """Connect to the first reachable CHECKED address (never re-resolve the name)."""
    last: Exception | None = None
    for address in addresses:
        try:
            reader, writer = await asyncio.wait_for(
                connector(address, port), timeout=_CONNECT_TIMEOUT_S
            )
        except (OSError, TimeoutError) as exc:
            last = exc
            continue
        return reader, writer, address
    raise ConnectionError(f"upstream unreachable: {last}")


class EgressProxy:
    """The asyncio proxy server. ``resolver``/``connector`` are injectable (tests fake DNS/TCP)."""

    def __init__(
        self,
        allow: str | Iterable[str],
        *,
        resolver: Resolver = system_resolver,
        connector: Connector = tcp_connector,
    ) -> None:
        self.allow = parse_allow_list(allow)
        self._resolver = resolver
        self._connector = connector
        self._server: asyncio.Server | None = None

    async def start(self, host: str = "127.0.0.1", port: int = 0) -> tuple[str, int]:
        """Start listening; returns the bound (host, port) (``port=0`` picks a free one)."""
        self._server = await asyncio.start_server(self._handle, host, port, limit=_MAX_HEADER_BYTES)
        sockname = self._server.sockets[0].getsockname()
        return str(sockname[0]), int(sockname[1])

    async def close(self) -> None:
        if self._server is not None:
            self._server.close()
            with contextlib.suppress(Exception):
                await self._server.wait_closed()
            self._server = None

    async def serve_forever(self) -> None:
        assert self._server is not None, "call start() first"
        async with self._server:
            await self._server.serve_forever()

    async def _handle(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        upstream: asyncio.StreamWriter | None = None
        try:
            request = await _read_head(reader)
            if request is None:
                await _respond(writer, "400 Bad Request", "malformed request")
                return
            label = f"{request.method} {request.target}"
            try:
                if request.method == "CONNECT":
                    host, port = _split_authority(request.target)
                else:
                    host, port = self._absolute_target(request.target)
                name = check_host(host, port, self.allow)
                addresses = await resolve_checked(name, port, self._resolver)
            except Denied as exc:
                log.warning("deny %s: %s", label, exc)
                await _respond(writer, "403 Forbidden", f"lha egress proxy: {exc}")
                return
            try:
                up_reader, upstream, address = await _open_upstream(
                    addresses, port, self._connector
                )
            except ConnectionError as exc:
                log.warning("fail %s: %s", label, exc)
                await _respond(writer, "502 Bad Gateway", f"lha egress proxy: {exc}")
                return
            log.info("allow %s -> %s:%d", label, address, port)
            if request.method == "CONNECT":
                writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                await writer.drain()
            else:
                upstream.write(self._origin_request(request, name, port))
                await upstream.drain()
            # The exchange is over when the upstream side ends; then stop relaying the client side.
            to_upstream = asyncio.create_task(_pipe(reader, upstream))
            try:
                await _pipe(up_reader, writer)
            finally:
                to_upstream.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await to_upstream
        except Exception as exc:  # never let one bad connection kill the server
            log.error("error: %s", exc)
        finally:
            for w in (upstream, writer):
                if w is not None:
                    with contextlib.suppress(Exception):
                        w.close()

    @staticmethod
    def _absolute_target(target: str) -> tuple[str, int]:
        try:
            parts = urlsplit(target)
            port = parts.port
        except ValueError as exc:
            raise Denied(f"malformed url: {target!r}") from exc
        if parts.scheme.lower() != "http":
            raise Denied("only CONNECT and absolute http:// requests are proxied")
        if parts.username is not None or parts.password is not None:
            raise Denied("credentials in the url are not allowed")
        if not parts.hostname:
            raise Denied(f"url has no host: {target!r}")
        return parts.hostname, port or 80

    @staticmethod
    def _origin_request(request: _Request, host: str, port: int) -> bytes:
        parts = urlsplit(request.target)
        path = parts.path or "/"
        if parts.query:
            path += "?" + parts.query
        lines = [f"{request.method} {path} {request.version}"]
        lines.append(f"Host: {host}" if port == 80 else f"Host: {host}:{port}")
        for name, value in request.headers:
            lowered = name.lower()
            if lowered == "host" or lowered in _HOP_BY_HOP:
                continue
            lines.append(f"{name}: {value}")
        lines.append("Connection: close")
        return ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1")


async def _main() -> None:
    allow = os.environ.get("LHA_PROXY_ALLOW", "")
    port = int(os.environ.get("LHA_PROXY_PORT", str(DEFAULT_PORT)))
    bind = os.environ.get("LHA_PROXY_BIND", "0.0.0.0")
    proxy = EgressProxy(allow)
    host, bound = await proxy.start(bind, port)
    entries = ",".join(
        ("." if e.suffix else "") + e.host + (f":{e.port}" if e.port else "") for e in proxy.allow
    )
    log.info("%s on %s:%d allow=%s", READY_MESSAGE, host, bound, entries or "(nothing)")
    await proxy.serve_forever()


if __name__ == "__main__":
    logging.basicConfig(
        stream=sys.stderr, level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s"
    )
    with contextlib.suppress(KeyboardInterrupt):
        asyncio.run(_main())
