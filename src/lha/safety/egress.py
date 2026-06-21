"""Default-deny network egress + credential brokering.

The agent only ever holds placeholder tokens; the broker swaps in the real secret at the egress
boundary — and only for the host that secret is bound to — so a hijacked agent exfiltrates useless
placeholders. ``EgressPolicy`` enforces an allow-list of (scheme, normalized host, port);
``check_resolved_addresses`` rejects hosts that resolve to private/loopback/link-local/multicast
addresses (SSRF / DNS rebinding to internal services). Together these are the network half of the
safety boundary for in-process HTTP tools; ``run_command`` egress is enforced by the sandbox
(Docker ``network_mode="none"``), not here.
"""

from __future__ import annotations

import asyncio
import ipaddress
import socket
from collections.abc import Awaitable, Callable, Iterable
from urllib.parse import urlsplit

from pydantic import BaseModel, Field, field_validator

# (host, port) -> list of IP address strings.
Resolver = Callable[[str, int], Awaitable[list[str]]]

_DEFAULT_PORTS = {"http": 80, "https": 443}


class EgressDenied(PermissionError):
    """Raised when a URL or its resolved addresses are not permitted."""


def normalize_host(host: str) -> str:
    """Lower-case, strip a trailing dot, IDNA-encode; IPv6 literals lose their brackets."""
    host = host.strip().strip("[]").rstrip(".").lower()
    if not host:
        return ""
    try:
        ipaddress.ip_address(host)
        return host
    except ValueError:
        pass
    try:
        return host.encode("idna").decode("ascii")
    except UnicodeError:
        return host


class ParsedURL(BaseModel):
    scheme: str
    host: str
    port: int


def parse_url(url: str) -> ParsedURL:
    """Parse + validate a URL for egress (http/https, host present, no userinfo, valid port)."""
    try:
        parts = urlsplit(url.strip())
        port = parts.port
    except ValueError as exc:
        raise EgressDenied(f"malformed url: {url!r}") from exc
    scheme = parts.scheme.lower()
    if scheme not in _DEFAULT_PORTS:
        raise EgressDenied(f"scheme not allowed: {scheme!r}")
    if parts.username is not None or parts.password is not None:
        raise EgressDenied("credentials in the url are not allowed")
    host = normalize_host(parts.hostname or "")
    if not host:
        raise EgressDenied(f"url has no host: {url!r}")
    return ParsedURL(scheme=scheme, host=host, port=port or _DEFAULT_PORTS[scheme])


class EgressPolicy(BaseModel):
    """Allow-list of permitted egress (default-deny: empty = nothing allowed).

    A URL is permitted iff its scheme is in ``allow_schemes``, its normalized host is in
    ``allow_hosts`` and its port is the scheme default or listed in ``allow_ports``.
    """

    allow_hosts: set[str] = Field(default_factory=set)
    allow_schemes: set[str] = Field(default_factory=lambda: {"https", "http"})
    allow_ports: set[int] = Field(default_factory=set)  # beyond the scheme defaults

    @field_validator("allow_hosts")
    @classmethod
    def _normalize_hosts(cls, hosts: set[str]) -> set[str]:
        return {normalize_host(h) for h in hosts if normalize_host(h)}

    def check(self, url: str) -> ParsedURL:
        """Return the parsed URL if permitted, else raise ``EgressDenied`` with the reason."""
        parsed = parse_url(url)
        if parsed.scheme not in self.allow_schemes:
            raise EgressDenied(f"scheme not allowed: {parsed.scheme!r}")
        if parsed.host not in self.allow_hosts:
            raise EgressDenied(f"host not in egress allow-list: {parsed.host!r}")
        if parsed.port != _DEFAULT_PORTS[parsed.scheme] and parsed.port not in self.allow_ports:
            raise EgressDenied(f"port not allowed: {parsed.port}")
        return parsed

    def permits(self, url: str) -> bool:
        try:
            self.check(url)
        except EgressDenied:
            return False
        return True


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
    """Resolve ``host`` with the OS resolver (all address families)."""
    infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
    return [str(info[4][0]) for info in infos]


async def check_resolved_addresses(host: str, port: int, resolver: Resolver) -> list[str]:
    """Resolve ``host`` and raise ``EgressDenied`` unless EVERY address is public.

    IP-literal hosts are checked as-is (no resolver involved).
    """
    try:
        ipaddress.ip_address(host)
        literal = True
    except ValueError:
        literal = False
    if literal:
        if not is_public_address(host):
            raise EgressDenied(f"{host!r} is a non-public address")
        return [host]
    try:
        addresses = await resolver(host, port)
    except OSError as exc:
        raise EgressDenied(f"cannot resolve {host!r}: {exc}") from exc
    if not addresses:
        raise EgressDenied(f"{host!r} did not resolve")
    blocked = [a for a in addresses if not is_public_address(a)]
    if blocked:
        raise EgressDenied(f"{host!r} resolves to a non-public address: {blocked[0]}")
    return addresses


class CredentialBroker:
    """Maps placeholder tokens to real secrets, injected only at egress to their bound host(s)."""

    def __init__(self) -> None:
        self._secrets: dict[str, tuple[str, frozenset[str]]] = {}

    def register(self, placeholder: str, real_secret: str, *, hosts: Iterable[str]) -> None:
        """Bind a placeholder (the agent may see it) to a secret (it never does) for ``hosts``."""
        bound = frozenset(normalize_host(h) for h in hosts if normalize_host(h))
        if not bound:
            raise ValueError("a brokered credential must be bound to at least one host")
        self._secrets[placeholder] = (real_secret, bound)

    def resolve(self, value: str, *, host: str) -> str:
        """Replace placeholders bound to ``host`` in ``value``; others stay as placeholders."""
        target = normalize_host(host)
        for placeholder, (secret, hosts) in self._secrets.items():
            if target in hosts:
                value = value.replace(placeholder, secret)
        return value

    def resolve_headers(self, headers: dict[str, str], *, host: str) -> dict[str, str]:
        """Resolve placeholders across header values for a request to ``host``."""
        return {key: self.resolve(val, host=host) for key, val in headers.items()}
