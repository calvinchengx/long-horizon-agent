"""S7: egress policy (scheme/port/host), private-address blocking, redirect re-checks, broker."""

from __future__ import annotations

import asyncio
import socket
from pathlib import Path

import httpx
import pytest

from lha.contracts.tools import ToolContext
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools.web import FetchUrlTool
from lha.safety.egress import (
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    check_resolved_addresses,
    is_ambiguous_idn,
    is_public_address,
    normalize_host,
    parse_url,
    system_resolver,
)

PUBLIC_IP = "93.184.216.34"


def _resolver(table: dict[str, list[str]]):  # type: ignore[no-untyped-def]
    async def resolve(host: str, port: int) -> list[str]:
        return table.get(host, [PUBLIC_IP])

    return resolve


async def _ctx(tmp_path: Path) -> ToolContext:
    return ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))


def test_policy_checks_scheme_port_and_normalizes_host() -> None:
    policy = EgressPolicy(allow_hosts={"Docs.Example.com"})
    assert policy.permits("https://docs.example.com/x")
    assert policy.permits("https://DOCS.EXAMPLE.COM./x")
    assert policy.permits("http://docs.example.com:80/x")
    assert not policy.permits("ftp://docs.example.com/x")
    assert not policy.permits("file:///etc/passwd")
    assert not policy.permits("https://docs.example.com:8443/x")
    assert not policy.permits("https://user:pw@docs.example.com/x")
    assert not policy.permits("https://docs.example.com.evil.test/x")
    assert EgressPolicy(allow_hosts={"docs.example.com"}, allow_ports={8443}).permits(
        "https://docs.example.com:8443/x"
    )


@pytest.mark.parametrize(
    "addr",
    [
        "127.0.0.1",
        "10.0.0.5",
        "192.168.1.1",
        "169.254.169.254",
        "::1",
        "fe80::1",
        "224.0.0.1",
        "::ffff:127.0.0.1",
        "0.0.0.0",
        "100.64.0.1",
        "fc00::1",
    ],
)
def test_non_public_addresses(addr: str) -> None:
    assert not is_public_address(addr)


def test_public_address() -> None:
    assert is_public_address(PUBLIC_IP)


@pytest.mark.asyncio
async def test_check_resolved_rejects_any_private_answer() -> None:
    resolver = _resolver({"rebind.test": [PUBLIC_IP, "127.0.0.1"]})
    with pytest.raises(EgressDenied):
        await check_resolved_addresses("rebind.test", 443, resolver)
    assert await check_resolved_addresses("ok.test", 443, resolver) == [PUBLIC_IP]


@pytest.mark.asyncio
async def test_fetch_denies_without_policy(tmp_path: Path) -> None:
    calls: list[httpx.Request] = []
    mock = httpx.MockTransport(lambda r: calls.append(r) or httpx.Response(200))
    result = await FetchUrlTool(transport=mock).run({"url": "https://a.test"}, await _ctx(tmp_path))
    assert not result.ok and "no egress policy" in (result.error or "")
    assert calls == []


@pytest.mark.asyncio
async def test_fetch_rechecks_every_redirect_hop(tmp_path: Path) -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        if request.url.host == "a.test":
            return httpx.Response(302, headers={"location": "http://169.254.169.254/latest"})
        return httpx.Response(200, text="metadata!")

    mock = httpx.MockTransport(handler)
    tool = FetchUrlTool(
        transport=mock,
        egress_policy=EgressPolicy(allow_hosts={"a.test", "169.254.169.254"}),
        resolver=_resolver({}),
    )
    result = await tool.run({"url": "https://a.test/"}, await _ctx(tmp_path))
    assert not result.ok and "egress blocked" in (result.error or "")
    assert seen == ["https://a.test/"]  # the internal hop was never requested


@pytest.mark.asyncio
async def test_fetch_follows_allowed_redirect_and_blocks_disallowed(tmp_path: Path) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/start":
            return httpx.Response(301, headers={"location": "/final"})
        if request.url.path == "/away":
            return httpx.Response(302, headers={"location": "https://other.test/"})
        return httpx.Response(200, text="<html><body><p>hello</p></body></html>")

    mock = httpx.MockTransport(handler)
    tool = FetchUrlTool(
        transport=mock, egress_policy=EgressPolicy(allow_hosts={"a.test"}), resolver=_resolver({})
    )
    ok = await tool.run({"url": "https://a.test/start"}, await _ctx(tmp_path))
    assert ok.ok and "\nhello\n</untrusted_content>" in ok.content
    away = await tool.run({"url": "https://a.test/away"}, await _ctx(tmp_path))
    assert not away.ok and "other.test" in (away.error or "")


@pytest.mark.asyncio
async def test_fetch_blocks_host_resolving_to_private_ip(tmp_path: Path) -> None:
    mock = httpx.MockTransport(lambda r: httpx.Response(200))
    tool = FetchUrlTool(
        transport=mock,
        egress_policy=EgressPolicy(allow_hosts={"a.test"}),
        resolver=_resolver({"a.test": ["10.1.2.3"]}),
    )
    result = await tool.run({"url": "https://a.test/"}, await _ctx(tmp_path))
    assert not result.ok and "non-public" in (result.error or "")


@pytest.mark.asyncio
async def test_fetch_broker_injects_only_for_bound_host(tmp_path: Path) -> None:
    auth_seen: dict[str, str | None] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        auth_seen[request.url.host] = request.headers.get("authorization")
        if request.url.host == "api.test":
            return httpx.Response(302, headers={"location": "https://cdn.test/file"})
        return httpx.Response(200, text="ok")

    broker = CredentialBroker()
    broker.register("{{API}}", "real-secret", hosts={"api.test"})
    tool = FetchUrlTool(
        transport=httpx.MockTransport(handler),
        egress_policy=EgressPolicy(allow_hosts={"api.test", "cdn.test"}),
        broker=broker,
        resolver=_resolver({}),
    )
    result = await tool.run(
        {"url": "https://api.test/", "headers": {"Authorization": "Bearer {{API}}"}},
        await _ctx(tmp_path),
    )
    assert result.ok
    assert auth_seen["api.test"] == "Bearer real-secret"
    assert auth_seen["cdn.test"] == "Bearer {{API}}"  # the redirect target never gets the secret

    bad = await tool.run(
        {"url": "https://api.test/", "headers": {"Host": "internal"}}, await _ctx(tmp_path)
    )
    assert not bad.ok


@pytest.mark.asyncio
async def test_default_client_ignores_env_proxies() -> None:
    tool = FetchUrlTool(egress_policy=EgressPolicy())
    async with tool._http.open() as session:  # a fresh per-call client
        assert session.client._trust_env is False
        assert session.client.follow_redirects is False
        assert session.backend is not None  # connections go through the pinned backend


# --- Rules pinned by the mutation audit (docs/20-testing.md#mutation-audit) ---------------------


def _denial(url: str, policy: EgressPolicy | None = None) -> str:
    with pytest.raises(EgressDenied) as exc:
        (policy or EgressPolicy(allow_hosts={"a.test"})).check(url)
    return str(exc.value)


# Host normalization: brackets, a trailing dot and case go; nothing else is stripped.
def test_normalize_host_strips_only_brackets_and_trailing_dots() -> None:
    assert normalize_host(" [X.Example.X] ") == "x.example.x"
    assert normalize_host("X.Example.X.") == "x.example.x"
    assert normalize_host("[::1]") == "::1"


# IP literals are returned verbatim, never IDNA-encoded (even with a non-ASCII zone id).
def test_normalize_host_keeps_ip_literals_verbatim() -> None:
    assert normalize_host("[fe80::1%é]") == "fe80::1%é"


# Non-ASCII hosts use UTS#46 mapping (fullwidth / compatibility forms), as httpx does.
def test_normalize_host_applies_uts46_mapping() -> None:
    fullwidth = "".join(chr(ord(c) + 0xFEE0) for c in "example")  # U+FF45 ...
    assert normalize_host(f"{fullwidth}.com") == "example.com"
    assert normalize_host("\u2177.com") == "viii.com"  # SMALL ROMAN NUMERAL EIGHT
    assert EgressPolicy(allow_hosts={"example.com"}).permits(f"https://{fullwidth}.com/")


# An unencodable host normalizes to "" (invalid), so it can never be allow-listed.
def test_unencodable_host_normalizes_to_empty() -> None:
    assert normalize_host("😀.com") == ""
    assert EgressPolicy(allow_hosts={"😀.com"}).allow_hosts == set()
    with pytest.raises(ValueError, match="at least one host"):
        CredentialBroker().register("{{K}}", "s", hosts={"😀.com"})


# Ambiguity is IDNA 2003 vs 2008 disagreeing: ASCII hosts never are, and a host that IDNA 2003
# cannot encode at all is invalid there, not a second domain.
def test_is_ambiguous_idn_only_when_both_encoders_disagree() -> None:
    assert is_ambiguous_idn("faß.de")
    assert not is_ambiguous_idn("[ example.com]")
    assert not is_ambiguous_idn("é\u180e.com")  # IDNA 2003 rejects U+180E; 2008 maps it away


def test_parse_url_denial_reasons() -> None:
    cases = {
        "https://a.test:99999/": "malformed url: 'https://a.test:99999/'",
        "ftp://a.test/": "scheme not allowed: 'ftp'",
        "https://faß.de/": (
            "ambiguous internationalized host (IDNA 2003/2008 differ): 'https://faß.de/'"
        ),
        "https:///x": "url has no valid host: 'https:///x'",
    }
    for url, reason in cases.items():
        with pytest.raises(EgressDenied) as exc:
            parse_url(url)
        assert str(exc.value) == reason


# Any userinfo is refused: a user alone, a password alone, or both.
@pytest.mark.parametrize(
    "url", ["https://user@a.test/", "https://:pw@a.test/", "https://user:pw@a.test/"]
)
def test_parse_url_refuses_any_credentials(url: str) -> None:
    assert _denial(url) == "credentials in the url are not allowed"


def test_policy_denial_reasons() -> None:
    https_only = EgressPolicy(allow_hosts={"a.test"}, allow_schemes={"https"})
    assert _denial("http://a.test/", https_only) == "scheme not allowed: 'http'"
    assert _denial("https://b.test/") == "host not in egress allow-list: 'b.test'"
    assert _denial("https://a.test:8443/") == "port not allowed: 8443"


def test_broker_requires_a_bound_host() -> None:
    with pytest.raises(ValueError) as exc:
        CredentialBroker().register("{{K}}", "s", hosts={"", "."})
    assert str(exc.value) == "a brokered credential must be bound to at least one host"


# IPv6 forms that embed an IPv4 address are judged by that IPv4 address (both directions).
@pytest.mark.parametrize(
    ("addr", "public"),
    [
        ("::ffff:10.0.0.1", False),  # IPv4-mapped
        ("::ffff:8.8.8.8", True),
        ("2002:0a00:0001::", False),  # 6to4
        ("2002:0808:0808::", True),
        ("2001:0:0808:0808::f5ff:fffe", False),  # Teredo, client 10.0.0.1 (server 8.8.8.8)
        ("2001:0:0a00:0001::f7f7:f7f7", True),  # Teredo, client 8.8.8.8 (server 10.0.0.1)
    ],
)
def test_embedded_ipv4_is_unwrapped(addr: str, public: bool) -> None:
    assert is_public_address(addr) is public


# Reserved space is non-public even where the stdlib calls it global (4000::/3).
def test_reserved_ipv6_is_not_public() -> None:
    assert not is_public_address("4000::1")


# Everything from the first "%" on is a zone id and is ignored.
def test_zone_id_is_dropped_from_the_first_percent() -> None:
    assert is_public_address("8.8.8.8%eth0")
    assert is_public_address("8.8.8.8%a%b")
    assert not is_public_address("10.0.0.1%eth0")


async def test_check_resolved_denial_reasons_and_port() -> None:
    ports: list[int] = []

    async def resolver(host: str, port: int) -> list[str]:
        ports.append(port)
        if host == "down.test":
            raise OSError("no such host")
        return [] if host == "empty.test" else [PUBLIC_IP]

    assert await check_resolved_addresses("ok.test", 8443, resolver) == [PUBLIC_IP]
    assert ports == [8443]
    reasons = {
        "10.0.0.1": "'10.0.0.1' is a non-public address",
        "down.test": "cannot resolve 'down.test': no such host",
        "empty.test": "'empty.test' did not resolve",
    }
    for host, reason in reasons.items():
        with pytest.raises(EgressDenied) as exc:
            await check_resolved_addresses(host, 443, resolver)
        assert str(exc.value) == reason


# The system resolver asks for TCP (stream) addresses of (host, port) and returns the IPs.
async def test_system_resolver_passes_host_port_stream_and_returns_ips(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[tuple[object, ...]] = []

    async def getaddrinfo(host: object, port: object, **kwargs: object) -> list[tuple]:
        calls.append((host, port, kwargs.get("type")))
        return [
            (socket.AF_INET6, socket.SOCK_STREAM, 6, "", ("2001:db8::7", 443, 0, 0)),
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("192.0.2.7", 443)),
        ]

    monkeypatch.setattr(asyncio.get_running_loop(), "getaddrinfo", getaddrinfo)
    assert await system_resolver("a.test", 443) == ["2001:db8::7", "192.0.2.7"]
    assert calls == [("a.test", 443, socket.SOCK_STREAM)]


async def test_system_resolver_resolves_localhost() -> None:
    addresses = await system_resolver("localhost", 80)
    assert addresses and set(addresses) <= {"127.0.0.1", "::1"}
