"""An httpx transport that connects only to addresses the egress check already vetted.

Checking a host's DNS answer and then letting the HTTP client resolve the name again leaves a
DNS-rebinding window: a 0-TTL server can answer the check with a public address and the
connection with ``127.0.0.1``. Here the name is resolved once, by the caller, through
``check_resolved_addresses``; the caller pins the vetted addresses for that host on a
``PinnedNetworkBackend``, and the backend's ``connect_tcp`` dials those addresses instead of the
name. The HTTP request itself is unchanged: the ``Host`` header, the TLS SNI and the certificate
check all use the original hostname (httpcore passes the URL's host as ``server_hostname`` to
``start_tls``, independent of the address the TCP socket reached).

The backend never resolves anything: a host with no pin is refused (fail closed), and an IP
literal is dialled only if it is public.
"""

from __future__ import annotations

import ipaddress
from collections.abc import Iterable
from typing import cast

import httpcore
import httpx

from lha.safety.egress import is_public_address, normalize_host

SocketOption = (
    tuple[int, int, int] | tuple[int, int, bytes | bytearray] | tuple[int, int, None, int]
)


class PinnedNetworkBackend(httpcore.AsyncNetworkBackend):
    """Dials only pinned (already vetted) addresses; never resolves a hostname itself."""

    def __init__(self, inner: httpcore.AsyncNetworkBackend | None = None) -> None:
        # httpx depends on anyio, so this is the real AnyIOBackend (not httpcore's stub).
        self._inner = inner or cast(httpcore.AsyncNetworkBackend, httpcore.AnyIOBackend())
        self._pins: dict[str, tuple[str, ...]] = {}
        #: Every address dialled, as ``(host, address, port)`` (observable in tests and logs).
        self.dialled: list[tuple[str, str, int]] = []

    def pin(self, host: str, addresses: Iterable[str]) -> None:
        """Allow connections to ``host`` only at ``addresses`` (each must be public)."""
        vetted = tuple(addresses)
        if not vetted or not all(is_public_address(a) for a in vetted):
            raise ValueError(f"refusing to pin {host!r} to non-public or no addresses: {vetted}")
        self._pins[normalize_host(host)] = vetted

    def addresses_for(self, host: str) -> tuple[str, ...]:
        """The addresses ``connect_tcp`` may dial for ``host`` (raises ``ConnectError`` if none)."""
        name = normalize_host(host)
        try:
            ipaddress.ip_address(name)
        except ValueError:
            pinned = self._pins.get(name)
            if pinned is None:
                raise httpcore.ConnectError(
                    f"egress: {host!r} has no vetted address (it was not checked before connect)"
                ) from None
            return pinned
        if not is_public_address(name):
            raise httpcore.ConnectError(f"egress: {host!r} is a non-public address")
        return (name,)

    async def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: Iterable[SocketOption] | None = None,
    ) -> httpcore.AsyncNetworkStream:
        last: Exception | None = None
        for address in self.addresses_for(host):
            self.dialled.append((normalize_host(host), address, port))
            try:
                return await self._inner.connect_tcp(
                    address,
                    port,
                    timeout=timeout,
                    local_address=local_address,
                    socket_options=socket_options,
                )
            except (httpcore.ConnectError, httpcore.ConnectTimeout) as exc:
                last = exc
        assert last is not None  # addresses_for never returns an empty tuple
        raise last

    async def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: Iterable[SocketOption] | None = None,
    ) -> httpcore.AsyncNetworkStream:
        raise httpcore.ConnectError("egress: unix sockets are not allowed")

    async def sleep(self, seconds: float) -> None:
        await self._inner.sleep(seconds)


class PinnedTransport(httpx.AsyncHTTPTransport):
    """``httpx.AsyncHTTPTransport`` whose connection pool dials through a pinned backend.

    No proxy and no environment settings (``trust_env=False``); TLS verification is on and uses
    the request's hostname.
    """

    def __init__(self, backend: PinnedNetworkBackend) -> None:
        super().__init__(trust_env=False)
        self._pool = httpcore.AsyncConnectionPool(
            ssl_context=httpx.create_ssl_context(trust_env=False),
            http1=True,
            http2=False,
            network_backend=backend,
        )
