"""The System One HTTP client (``POST /v1/systemone``): hosted Jev or a self-hosted server.

One client covers every backend that speaks TypeSafe's API: the hosted endpoint
(``https://api.typesafe.ai/v1/systemone``) and self-hosted servers such as Kev
(``http://127.0.0.1:8009/v1/systemone``). No SDK dependency: the wire format is small and lives
in ``lha.systemone.wire`` (shared with Go).

Egress follows the Voyage embedder's rules. A remote endpoint must be https, and every request
re-resolves its host, refuses non-public addresses and dials only the vetted ones (no DNS
rebinding, no proxy from the environment); the API key is bound to that host by a
``CredentialBroker`` and never appears in ``repr`` or error messages. A loopback endpoint
(``localhost``, ``127.0.0.0/8``, ``::1``) may use http and is dialled directly.

Every call is metered through the mission's ``CostMeter`` (priced per input token; output is
free) and is short: answers are advisory, so a slow or failed call is skipped, not waited for.
"""

from __future__ import annotations

import asyncio
import functools
import ipaddress
import json
import math

import httpcore
import httpx
from pydantic import JsonValue

from lha.contracts.model import Usage
from lha.contracts.system_one import Question, SystemOneError, SystemOneResult
from lha.governor.metering import BudgetExceeded, CostMeter
from lha.model.retry import Sleep, with_retries
from lha.safety.egress import (
    CredentialBroker,
    EgressDenied,
    EgressPolicy,
    ParsedURL,
    Resolver,
    check_resolved_addresses,
    parse_url,
    system_resolver,
)
from lha.safety.pinned_http import PinnedNetworkBackend, PinnedTransport
from lha.systemone.wire import parse_response, request_body

TYPESAFE_ENDPOINT = "https://api.typesafe.ai/v1/systemone"
#: TypeSafe's published price for Jev (USD per million input tokens; output tokens are free).
TYPESAFE_PRICE_IN_PER_MTOK = 0.042
_KEY_PLACEHOLDER = "{{LHA_SYSTEM_ONE_API_KEY}}"
# Conservative characters per token for the pre-call estimate (as in ``lha.governor.metering``).
_CHARS_PER_TOKEN = 2.0


def is_local_endpoint(target: ParsedURL) -> bool:
    """True for a loopback endpoint (``localhost``, ``*.localhost``, ``127.0.0.0/8``, ``::1``)."""
    host = target.host.strip("[]")
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def default_price_in_per_mtok(endpoint: str) -> float | None:
    """The input price when none is configured: Jev's for TypeSafe, $0 for loopback, else unknown."""
    target = parse_url(endpoint)
    if target.host == parse_url(TYPESAFE_ENDPOINT).host:
        return TYPESAFE_PRICE_IN_PER_MTOK
    if is_local_endpoint(target):
        return 0.0
    return None


class SystemOneClient:
    """A metered client for one System One endpoint and model."""

    MAX_RETRIES = 1
    RETRY_BASE_DELAY_S = 0.25
    RETRY_MAX_DELAY_S = 2.0

    def __init__(
        self,
        *,
        model: str,
        endpoint: str = TYPESAFE_ENDPOINT,
        api_key: str = "",
        price_in_per_mtok: float | None = None,
        timeout_s: float = 5.0,
        meter: CostMeter | None = None,
        role: str = "system_one",
        resolver: Resolver | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        network_backend: httpcore.AsyncNetworkBackend | None = None,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        self._endpoint = endpoint
        self._target = parse_url(endpoint)  # EgressDenied for a malformed endpoint
        self._local = is_local_endpoint(self._target)
        if not self._local and self._target.scheme != "https":
            raise EgressDenied(f"a remote System One endpoint must use https: {endpoint!r}")
        self.name = f"systemone:{model}"
        self._model = model
        self._price = price_in_per_mtok
        self._meter = meter
        self._role = role
        self._api_key = api_key
        self._broker = CredentialBroker()
        if api_key:
            self._broker.register(_KEY_PLACEHOLDER, api_key, hosts=[self._target.host])
        self._policy = EgressPolicy(
            allow_hosts={self._target.host},
            allow_schemes={self._target.scheme},
            allow_ports={self._target.port},
        )
        self._resolver = resolver or system_resolver
        self._sleep = sleep
        self._backend: PinnedNetworkBackend | None = None
        if transport is None and not self._local:  # ``transport`` is a test seam
            self._backend = PinnedNetworkBackend(network_backend)
            transport = PinnedTransport(self._backend)
        self._client = httpx.AsyncClient(
            timeout=timeout_s, follow_redirects=False, trust_env=False, transport=transport
        )

    def __repr__(self) -> str:
        return f"SystemOneClient(model={self._model!r}, endpoint={self._endpoint!r})"

    @property
    def model(self) -> str:
        return self._model

    def _scrub(self, text: str) -> str:
        return text.replace(self._api_key, "***") if self._api_key else text

    def worst_case_usd(self, body: bytes) -> float | None:
        """Upper bound on a request's cost (``None`` when the endpoint has no price)."""
        if self._price is None:
            return None
        return math.ceil(len(body.decode("utf-8")) / _CHARS_PER_TOKEN) * self._price / 1e6

    async def evaluate(self, state: JsonValue, questions: dict[str, Question]) -> SystemOneResult:
        body = json.dumps(
            request_body(self._model, state, questions), separators=(",", ":")
        ).encode("utf-8")

        async def call() -> tuple[SystemOneResult, Usage]:
            data = await with_retries(
                functools.partial(self._post, body),
                max_retries=self.MAX_RETRIES,
                base_delay_s=self.RETRY_BASE_DELAY_S,
                max_delay_s=self.RETRY_MAX_DELAY_S,
                sleep=self._sleep,
            )
            result = parse_response(data, questions)
            usd = None if self._price is None else result.input_tokens * self._price / 1e6
            usage = Usage(
                input_tokens=result.input_tokens,
                output_tokens=result.output_tokens,
                model=result.model or self._model,
                provider=self.name,
                reported_cost_usd=usd,
            )
            return result, usage

        try:
            if self._meter is None:
                result, _usage = await call()
                return result
            return await self._meter.run_external(
                call, worst_case_usd=self.worst_case_usd(body), role=self._role
            )
        except SystemOneError:
            raise
        except BudgetExceeded as exc:
            raise SystemOneError(f"system one call refused: {exc}") from None
        except (httpx.HTTPError, EgressDenied, OSError, ValueError) as exc:
            raise SystemOneError(self._describe(exc)) from None

    def _describe(self, exc: Exception) -> str:
        if isinstance(exc, httpx.HTTPStatusError):
            why = f"{self._endpoint} answered HTTP {exc.response.status_code}"
        else:
            why = f"{self._endpoint} failed ({type(exc).__name__}: {exc})"
        return self._scrub(f"system one call failed: {why}")

    async def _post(self, body: bytes) -> object:
        parsed = self._policy.check(self._endpoint)
        if self._backend is not None:
            addresses = await check_resolved_addresses(parsed.host, parsed.port, self._resolver)
            self._backend.pin(parsed.host, addresses)  # the connection dials only these
        headers = {"content-type": "application/json"}
        if self._api_key:
            headers["authorization"] = f"Bearer {_KEY_PLACEHOLDER}"
            headers = self._broker.resolve_headers(headers, host=parsed.host)
        resp = await self._client.post(self._endpoint, content=body, headers=headers)
        resp.raise_for_status()
        return resp.json()

    async def aclose(self) -> None:
        await self._client.aclose()
