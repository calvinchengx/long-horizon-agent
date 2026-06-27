"""S7: egress policy (scheme/port/host), private-address blocking, redirect re-checks, broker."""

from __future__ import annotations

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
    is_public_address,
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
    client = httpx.AsyncClient(
        transport=httpx.MockTransport(lambda r: calls.append(r) or httpx.Response(200))
    )
    result = await FetchUrlTool(client=client).run({"url": "https://a.test"}, await _ctx(tmp_path))
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

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    tool = FetchUrlTool(
        client=client,
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

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    tool = FetchUrlTool(
        client=client, egress_policy=EgressPolicy(allow_hosts={"a.test"}), resolver=_resolver({})
    )
    ok = await tool.run({"url": "https://a.test/start"}, await _ctx(tmp_path))
    assert ok.ok and ok.content == "hello"
    away = await tool.run({"url": "https://a.test/away"}, await _ctx(tmp_path))
    assert not away.ok and "other.test" in (away.error or "")


@pytest.mark.asyncio
async def test_fetch_blocks_host_resolving_to_private_ip(tmp_path: Path) -> None:
    client = httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(200)))
    tool = FetchUrlTool(
        client=client,
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
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
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


def test_default_client_ignores_env_proxies() -> None:
    tool = FetchUrlTool(egress_policy=EgressPolicy())
    assert tool._client._trust_env is False
    assert tool._client.follow_redirects is False
