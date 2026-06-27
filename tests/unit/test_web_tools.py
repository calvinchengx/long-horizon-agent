"""fetch_url error paths (headers, redirects, HTTP errors, size cap) and web_search."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

import httpx
import pytest

from lha.contracts.tools import ToolContext
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools.web import FetchUrlTool, WebSearchTool
from lha.safety.egress import EgressPolicy

PUBLIC_IP = "93.184.216.34"


async def _public(host: str, port: int) -> list[str]:
    return [PUBLIC_IP]


async def _ctx(tmp_path: Path) -> ToolContext:
    return ToolContext(mission_id="m", session=await LocalSandbox().open(workdir=str(tmp_path)))


def _fetch(handler: Callable[[httpx.Request], httpx.Response]) -> FetchUrlTool:
    return FetchUrlTool(
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        egress_policy=EgressPolicy(allow_hosts={"a.test", "b.test"}),
        resolver=_public,
    )


# --- fetch_url --------------------------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("headers", "error"),
    [
        (["not", "a", "dict"], "must be an object"),
        ({"X-Count": 3}, "must be an object"),
        ({"Host": "evil.test"}, "header not allowed"),
        ({"Proxy-Authorization": "x"}, "header not allowed"),
    ],
)
async def test_fetch_rejects_bad_headers(tmp_path: Path, headers: object, error: str) -> None:
    tool = _fetch(lambda r: httpx.Response(200, text="unreachable"))
    result = await tool.run({"url": "https://a.test", "headers": headers}, await _ctx(tmp_path))
    assert not result.ok and error in (result.error or "")


@pytest.mark.asyncio
async def test_fetch_strips_html_and_scripts(tmp_path: Path) -> None:
    page = "<html><script>steal()</script><style>p{}</style><p>Hello <b>world</b></p></html>"
    tool = _fetch(lambda r: httpx.Response(200, text=page))
    result = await tool.run({"url": "https://a.test/page"}, await _ctx(tmp_path))
    assert result.ok and result.content == "Hello world"


@pytest.mark.asyncio
async def test_fetch_redirect_without_location_fails(tmp_path: Path) -> None:
    tool = _fetch(lambda r: httpx.Response(302))
    result = await tool.run({"url": "https://a.test"}, await _ctx(tmp_path))
    assert not result.ok and "redirect without location" in (result.error or "")


@pytest.mark.asyncio
async def test_fetch_gives_up_after_max_redirects(tmp_path: Path) -> None:
    hops: list[str] = []

    def loop(request: httpx.Request) -> httpx.Response:
        hops.append(str(request.url))
        return httpx.Response(302, headers={"location": f"/hop{len(hops)}"})

    result = await _fetch(loop).run({"url": "https://a.test"}, await _ctx(tmp_path))
    assert not result.ok and "more than 5 redirects" in (result.error or "")
    assert len(hops) == FetchUrlTool.MAX_REDIRECTS + 1


@pytest.mark.asyncio
async def test_fetch_reports_http_errors(tmp_path: Path) -> None:
    result = await _fetch(lambda r: httpx.Response(503)).run(
        {"url": "https://a.test"}, await _ctx(tmp_path)
    )
    assert not result.ok and "fetch_url failed" in (result.error or "")


@pytest.mark.asyncio
async def test_fetch_reports_transport_errors(tmp_path: Path) -> None:
    def boom(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    result = await _fetch(boom).run({"url": "https://a.test"}, await _ctx(tmp_path))
    assert not result.ok and "connection refused" in (result.error or "")


@pytest.mark.asyncio
async def test_fetch_caps_the_response_body(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(FetchUrlTool, "MAX_RESPONSE_BYTES", 10)
    tool = _fetch(lambda r: httpx.Response(200, content=b"x" * 1000))
    result = await tool.run({"url": "https://a.test"}, await _ctx(tmp_path))
    assert result.ok and result.content == "x" * 10


# --- web_search -------------------------------------------------------------------------------


def _search(provider: str, handler: Callable[[httpx.Request], httpx.Response]) -> WebSearchTool:
    return WebSearchTool(
        api_key="key",
        provider=provider,  # type: ignore[arg-type]
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )


@pytest.mark.asyncio
async def test_tavily_search_formats_results(tmp_path: Path) -> None:
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        results = [
            {"title": "T1", "url": "https://1.test", "content": "c" * 500},
            "not-a-dict",
            {"title": "T2", "url": "https://2.test", "snippet": "s2"},
        ]
        return httpx.Response(200, json={"results": results})

    result = await _search("tavily", handler).run(
        {"query": "q", "max_results": 3}, await _ctx(tmp_path)
    )
    assert result.ok
    assert "- T1\n  https://1.test\n  " + "c" * 300 + "\n" in result.content
    assert "- T2\n  https://2.test\n  s2" in result.content
    assert str(seen[0].url) == "https://api.tavily.com/search"
    assert json.loads(seen[0].content) == {"api_key": "key", "query": "q", "max_results": 3}


@pytest.mark.asyncio
async def test_exa_search_sends_key_as_header_and_defaults_count(tmp_path: Path) -> None:
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"results": []})

    result = await _search("exa", handler).run(
        {"query": "q", "max_results": -1}, await _ctx(tmp_path)
    )
    assert result.ok and result.content == "(no results)"
    assert seen[0].headers["x-api-key"] == "key"
    assert json.loads(seen[0].content) == {"query": "q", "numResults": 5}


@pytest.mark.asyncio
async def test_search_errors_and_odd_payloads(tmp_path: Path) -> None:
    failed = await _search("tavily", lambda r: httpx.Response(500)).run(
        {"query": "q"}, await _ctx(tmp_path)
    )
    assert not failed.ok and "web_search failed" in (failed.error or "")
    odd = await _search("tavily", lambda r: httpx.Response(200, json=["x"])).run(
        {"query": "q"}, await _ctx(tmp_path)
    )
    assert odd.ok and odd.content == "(no results)"
