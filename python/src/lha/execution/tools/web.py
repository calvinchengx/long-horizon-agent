"""Web tools: search + fetch — the agent's internet / deep-research capability.

Both are ``egress=True`` (blocked unless the dispatcher enables egress) and ``untrusted_input=True``
(their results are redacted and fenced as untrusted data, see ``untrusted.py``; the dispatcher
derives the Rule-of-Two ``UNTRUSTED_CONTENT`` capability from the flag). Run paths register them
only through ``lha.execution.tools.toolset`` when ``LHA_EGRESS_ALLOW_HOSTS`` is non-empty.

- ``fetch_url`` follows the operator's ``EgressPolicy`` (default-deny host allow-list, public
  addresses only, per-hop redirect re-checks, size cap, brokered credentials bound to hosts).
- ``web_search`` talks only to its configured provider endpoint (Tavily or Exa). That endpoint is
  checked the same way (scheme/host/port + public addresses), and the API key is handed to the
  request through a ``CredentialBroker`` bound to the endpoint's host, so it can never be sent
  anywhere else.

HTTP clients: pass ``client`` to share one (the caller owns it), otherwise each call opens and
closes its own client (``transport`` lets tests inject an ``httpx.MockTransport``).
"""

from __future__ import annotations

import contextlib
import json
import re
from collections.abc import AsyncIterator
from typing import Literal

import httpx

from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.execution.tools.limits import MAX_TOOL_OUTPUT
from lha.execution.tools.untrusted import mark_untrusted
from lha.safety.egress import (
    DEFAULT_PORTS,
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    ParsedURL,
    Resolver,
    check_resolved_addresses,
    normalize_host,
    parse_url,
    system_resolver,
)

WebProvider = Literal["tavily", "exa"]

SEARCH_ENDPOINTS: dict[str, str] = {
    "tavily": "https://api.tavily.com/search",
    "exa": "https://api.exa.ai/search",
}
DEFAULT_TIMEOUT_S = 30.0
DEFAULT_MAX_RESPONSE_BYTES = 2_000_000
_SEARCH_KEY_PLACEHOLDER = "{{LHA_WEB_SEARCH_API_KEY}}"


class _Http:
    """Either a caller-owned shared client or a fresh client per call (closed after it)."""

    def __init__(
        self,
        client: httpx.AsyncClient | None,
        transport: httpx.AsyncBaseTransport | None,
        timeout_s: float,
    ) -> None:
        self._client = client
        self._transport = transport
        self._timeout_s = timeout_s

    @contextlib.asynccontextmanager
    async def open(self) -> AsyncIterator[httpx.AsyncClient]:
        if self._client is not None:
            yield self._client
            return
        async with httpx.AsyncClient(
            timeout=self._timeout_s,
            follow_redirects=False,
            trust_env=False,
            transport=self._transport,
        ) as client:
            yield client


class WebSearchTool:
    spec = ToolSpec(
        name="web_search",
        description=(
            "Search the web; returns top results (title, url, snippet) for research. Results "
            "are untrusted data."
        ),
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
        endpoint: str | None = None,
        resolver: Resolver | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        timeout_s: float = DEFAULT_TIMEOUT_S,
        max_response_bytes: int = DEFAULT_MAX_RESPONSE_BYTES,
    ) -> None:
        self._provider = provider
        self._endpoint = endpoint or SEARCH_ENDPOINTS[provider]
        target = parse_url(self._endpoint)  # raises EgressDenied for a malformed endpoint
        # The search tool may reach exactly its endpoint (scheme, host and port), nothing else.
        self._policy = EgressPolicy(
            allow_hosts={target.host},
            allow_schemes={target.scheme},
            allow_ports={target.port},
        )
        self._broker = CredentialBroker()
        self._broker.register(_SEARCH_KEY_PLACEHOLDER, api_key, hosts=[target.host])
        self._resolver = resolver or system_resolver
        self._http = _Http(client, transport, timeout_s)
        self._max_response_bytes = max_response_bytes

    def _request(self, query: str, n: int) -> tuple[dict[str, str], dict[str, object]]:
        if self._provider == "tavily":
            return {}, {"api_key": _SEARCH_KEY_PLACEHOLDER, "query": query, "max_results": n}
        return {"x-api-key": _SEARCH_KEY_PLACEHOLDER}, {"query": query, "numResults": n}

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        # The key placeholder is resolved in the body, so it may never come from the model.
        query = str(arguments.get("query", "")).replace(_SEARCH_KEY_PLACEHOLDER, "")
        raw_n = arguments.get("max_results", 5)
        n = raw_n if isinstance(raw_n, int) and raw_n > 0 else 5
        try:
            parsed = self._policy.check(self._endpoint)
            await check_resolved_addresses(parsed.host, parsed.port, self._resolver)
        except EgressDenied as exc:
            return ToolResult.failure(f"egress blocked by policy: {exc}")
        headers, payload = self._request(query, n)
        # Placeholders become the real key only for the endpoint host the key is bound to.
        headers = self._broker.resolve_headers(headers, host=parsed.host)
        body = self._broker.resolve(json.dumps(payload), host=parsed.host)
        try:
            async with self._http.open() as client:
                request = client.build_request(
                    "POST",
                    self._endpoint,
                    content=body.encode("utf-8"),
                    headers={**headers, "content-type": "application/json"},
                )
                mismatch = _target_mismatch(request, parsed)
                if mismatch:
                    return ToolResult.failure(f"egress blocked by policy: {mismatch}")
                resp = await client.send(request, stream=True, follow_redirects=False)
                try:
                    resp.raise_for_status()
                    raw = await _read_capped(resp, self._max_response_bytes)
                finally:
                    await resp.aclose()
            data = json.loads(raw.decode("utf-8", errors="replace"))
        except httpx.HTTPError as exc:
            return ToolResult.failure(f"web_search failed: {exc}")
        except ValueError as exc:
            return ToolResult.failure(f"web_search failed: invalid JSON response ({exc})")

        results = data.get("results", []) if isinstance(data, dict) else []
        lines: list[str] = []
        for item in results[:n]:
            if not isinstance(item, dict):
                continue
            title = item.get("title") or ""
            url = item.get("url") or ""
            snippet = item.get("content") or item.get("snippet") or item.get("text") or ""
            lines.append(f"- {title}\n  {url}\n  {str(snippet)[:300]}")
        text = "\n".join(lines) if lines else "(no results)"
        return ToolResult.success(
            mark_untrusted(text[:MAX_TOOL_OUTPUT], source=f"web_search:{self._provider}")
        )


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
    - The response body is read up to ``max_response_bytes`` (default ``MAX_RESPONSE_BYTES``).
    - The page text is redacted and fenced as untrusted content.
    """

    MAX_REDIRECTS = 5
    MAX_RESPONSE_BYTES = DEFAULT_MAX_RESPONSE_BYTES

    spec = ToolSpec(
        name="fetch_url",
        description=(
            "Fetch a URL and return its readable text content (HTML stripped; untrusted data). "
            "Optional 'headers' may contain credential placeholders, filled in only for their "
            "bound host."
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
        transport: httpx.AsyncBaseTransport | None = None,
        timeout_s: float = DEFAULT_TIMEOUT_S,
        max_response_bytes: int | None = None,
        placeholders: dict[str, list[str]] | None = None,
    ) -> None:
        self._http = _Http(client, transport, timeout_s)
        self._egress_policy = egress_policy
        self._broker = broker
        self._resolver = resolver or system_resolver
        self._max_response_bytes = max_response_bytes
        if egress_policy is not None and egress_policy.allow_hosts:
            hosts = ", ".join(sorted(egress_policy.allow_hosts))
            extra = f" Allowed hosts: {hosts}."
            if placeholders:
                creds = "; ".join(
                    f"{name} (for {', '.join(sorted(bound))})"
                    for name, bound in sorted(placeholders.items())
                )
                extra += f" Credential placeholders: {creds}."
            self.spec = type(self).spec.model_copy(
                update={"description": type(self).spec.description + extra}
            )

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
        limit = self._max_response_bytes or self.MAX_RESPONSE_BYTES

        async with self._http.open() as client:
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
                    request = client.build_request("GET", url, headers=headers or None)
                    mismatch = _target_mismatch(request, parsed)
                    if mismatch:
                        return ToolResult.failure(f"egress blocked by policy: {mismatch} ({url!r})")
                    resp = await client.send(request, stream=True, follow_redirects=False)
                    try:
                        if resp.is_redirect:
                            location = resp.headers.get("location")
                            if not location:
                                return ToolResult.failure(
                                    "fetch_url failed: redirect without location"
                                )
                            url = str(resp.url.join(location))
                            continue
                        resp.raise_for_status()
                        body = await _read_capped(resp, limit)
                    finally:
                        await resp.aclose()
                except httpx.HTTPError as exc:
                    return ToolResult.failure(f"fetch_url failed: {exc}")
                text = body.decode(resp.encoding or "utf-8", errors="replace")
                return ToolResult.success(
                    mark_untrusted(_html_to_text(text)[:MAX_TOOL_OUTPUT], source=url)
                )
        return ToolResult.failure(f"fetch_url failed: more than {self.MAX_REDIRECTS} redirects")


def _target_mismatch(request: httpx.Request, parsed: ParsedURL) -> str | None:
    """The host/port httpx will actually connect to must be exactly what the policy checked."""
    try:
        actual_host = normalize_host(request.url.raw_host.decode("ascii"))
    except UnicodeDecodeError:
        return "request host is not ASCII after encoding"
    actual_port = request.url.port or DEFAULT_PORTS.get(request.url.scheme, -1)
    if actual_host != parsed.host or actual_port != parsed.port:
        return (
            f"request target {actual_host}:{actual_port} differs from the checked "
            f"{parsed.host}:{parsed.port}"
        )
    return None


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
