"""Web tools: search + fetch — the agent's internet / deep-research capability.

Both are ``egress=True`` so they are blocked unless the dispatcher explicitly enables network
access (default-deny). Search supports Tavily and Exa (real APIs, real keys); fetch retrieves a
URL and reduces it to readable text. The deep-research *pattern* (many searches → fetch →
synthesize) is the Researcher sub-agent built on these primitives.
"""

from __future__ import annotations

import re
from typing import Literal

import httpx

from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.execution.tools.limits import MAX_TOOL_OUTPUT
from lha.safety.egress import (
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    Resolver,
    check_resolved_addresses,
    system_resolver,
)

WebProvider = Literal["tavily", "exa"]


class WebSearchTool:
    spec = ToolSpec(
        name="web_search",
        description="Search the web; returns top results (title, url, snippet) for research.",
        parameters={
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "max_results": {"type": "integer"},
            },
            "required": ["query"],
        },
        egress=True,
        untrusted_input=True,
    )

    def __init__(
        self,
        *,
        api_key: str,
        provider: WebProvider = "tavily",
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self._api_key = api_key
        self._provider = provider
        self._client = client or httpx.AsyncClient(timeout=30.0, trust_env=False)

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        query = str(arguments.get("query", ""))
        raw_n = arguments.get("max_results", 5)
        n = raw_n if isinstance(raw_n, int) and raw_n > 0 else 5
        try:
            if self._provider == "tavily":
                resp = await self._client.post(
                    "https://api.tavily.com/search",
                    json={"api_key": self._api_key, "query": query, "max_results": n},
                )
            else:
                resp = await self._client.post(
                    "https://api.exa.ai/search",
                    headers={"x-api-key": self._api_key},
                    json={"query": query, "numResults": n},
                )
            resp.raise_for_status()
            data = resp.json()
        except httpx.HTTPError as exc:
            return ToolResult.failure(f"web_search failed: {exc}")

        results = data.get("results", []) if isinstance(data, dict) else []
        lines: list[str] = []
        for item in results[:n]:
            if not isinstance(item, dict):
                continue
            title = item.get("title") or ""
            url = item.get("url") or ""
            snippet = item.get("content") or item.get("snippet") or item.get("text") or ""
            lines.append(f"- {title}\n  {url}\n  {str(snippet)[:300]}")
        body = "\n".join(lines) if lines else "(no results)"
        return ToolResult.success(body[:MAX_TOOL_OUTPUT])


_FORBIDDEN_HEADERS = frozenset({"host", "connection", "content-length", "transfer-encoding"})


class FetchUrlTool:
    """Fetch a URL under a default-deny egress policy with SSRF protection.

    - ``egress_policy=None`` denies everything (fail closed).
    - Redirects are NOT auto-followed: each hop (max ``MAX_REDIRECTS``) is re-checked against the
      policy and re-resolved, so an allowed host cannot bounce the request to an internal one.
    - Every hop's host must resolve only to public addresses (``resolver`` is injectable for
      tests). Residual risk: the HTTP client resolves again when connecting, so a DNS-rebinding
      server with a 0 TTL can still race this check — sandbox-level network isolation is the
      backstop.
    - ``trust_env=False``: proxy / netrc settings from the host environment are ignored.
    - Brokered credentials are substituted only into requests to the host they are bound to.
    - The response body is read up to ``MAX_RESPONSE_BYTES``.
    """

    MAX_REDIRECTS = 5
    MAX_RESPONSE_BYTES = 2_000_000

    spec = ToolSpec(
        name="fetch_url",
        description=(
            "Fetch a URL and return its readable text content (HTML stripped). Optional "
            "'headers' may contain credential placeholders, filled in only for their bound host."
        ),
        parameters={
            "type": "object",
            "properties": {
                "url": {"type": "string"},
                "headers": {"type": "object", "additionalProperties": {"type": "string"}},
            },
            "required": ["url"],
        },
        egress=True,
        untrusted_input=True,
    )

    def __init__(
        self,
        *,
        client: httpx.AsyncClient | None = None,
        egress_policy: EgressPolicy | None = None,
        broker: CredentialBroker | None = None,
        resolver: Resolver | None = None,
    ) -> None:
        self._client = client or httpx.AsyncClient(
            timeout=30.0, follow_redirects=False, trust_env=False
        )
        self._egress_policy = egress_policy
        self._broker = broker
        self._resolver = resolver or system_resolver

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        url = str(arguments.get("url", ""))
        raw_headers = arguments.get("headers") or {}
        if not isinstance(raw_headers, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in raw_headers.items()
        ):
            return ToolResult.failure("'headers' must be an object of string values")
        forbidden = [
            k
            for k in raw_headers
            if k.lower() in _FORBIDDEN_HEADERS or k.lower().startswith("proxy-")
        ]
        if forbidden:
            return ToolResult.failure(f"header not allowed: {forbidden[0]!r}")
        if self._egress_policy is None:
            return ToolResult.failure("egress blocked: no egress policy configured (default-deny)")

        for _hop in range(self.MAX_REDIRECTS + 1):
            try:
                parsed = self._egress_policy.check(url)
                await check_resolved_addresses(parsed.host, parsed.port, self._resolver)
            except EgressDenied as exc:
                return ToolResult.failure(f"egress blocked by policy: {exc} ({url!r})")
            headers = dict(raw_headers)
            if self._broker is not None:
                headers = self._broker.resolve_headers(headers, host=parsed.host)
            try:
                request = self._client.build_request("GET", url, headers=headers or None)
                resp = await self._client.send(request, stream=True, follow_redirects=False)
                try:
                    if resp.is_redirect:
                        location = resp.headers.get("location")
                        if not location:
                            return ToolResult.failure("fetch_url failed: redirect without location")
                        url = str(resp.url.join(location))
                        continue
                    resp.raise_for_status()
                    body = await _read_capped(resp, self.MAX_RESPONSE_BYTES)
                finally:
                    await resp.aclose()
            except httpx.HTTPError as exc:
                return ToolResult.failure(f"fetch_url failed: {exc}")
            text = body.decode(resp.encoding or "utf-8", errors="replace")
            return ToolResult.success(_html_to_text(text)[:MAX_TOOL_OUTPUT])
        return ToolResult.failure(f"fetch_url failed: more than {self.MAX_REDIRECTS} redirects")


async def _read_capped(resp: httpx.Response, limit: int) -> bytes:
    data = bytearray()
    async for chunk in resp.aiter_bytes():
        data += chunk
        if len(data) >= limit:
            break
    return bytes(data[:limit])


def _html_to_text(html: str) -> str:
    html = re.sub(r"(?is)<(script|style)\b.*?</\1>", " ", html)
    text = re.sub(r"(?s)<[^>]+>", " ", html)
    return re.sub(r"\s+", " ", text).strip()
