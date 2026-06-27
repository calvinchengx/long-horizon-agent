"""Retry classification + bounded exponential backoff for model HTTP calls.

Only transient failures are retried (or failed over): HTTP 408/409/429/5xx, timeouts and
connection/transport errors. Client errors such as 400 (bad request), 401 (bad key) and 403
(forbidden) are NOT retried: retrying or failing over cannot fix them and would only hide a
misconfiguration. A server-supplied ``Retry-After`` is honoured (capped).
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from datetime import UTC, datetime
from email.utils import parsedate_to_datetime

import httpx

RETRYABLE_STATUS = frozenset({408, 409, 429})

Sleep = Callable[[float], Awaitable[None]]


def is_retryable(exc: BaseException) -> bool:
    """True for transient errors worth retrying / failing over on."""
    if getattr(exc, "retryable", False) is True:  # non-HTTP backends mark their own (claude -p)
        return True
    if isinstance(exc, httpx.HTTPStatusError):
        status = exc.response.status_code
        return status in RETRYABLE_STATUS or status >= 500
    # TransportError covers TimeoutException, ConnectError, ReadError, RemoteProtocolError, ...
    return isinstance(exc, httpx.TransportError | TimeoutError | ConnectionError)


def retry_after_seconds(exc: BaseException) -> float | None:
    """The server's ``Retry-After`` hint (seconds or HTTP-date), if any."""
    if not isinstance(exc, httpx.HTTPStatusError):
        return None
    raw = exc.response.headers.get("retry-after")
    if raw is None:
        return None
    try:
        return max(0.0, float(raw))
    except ValueError:
        pass
    try:
        when = parsedate_to_datetime(raw)
    except (TypeError, ValueError):
        return None
    if when.tzinfo is None:
        when = when.replace(tzinfo=UTC)
    return max(0.0, (when - datetime.now(UTC)).total_seconds())


def backoff_delay(
    attempt: int, exc: BaseException | None, *, base_s: float = 1.0, max_s: float = 60.0
) -> float:
    """Delay before retry ``attempt`` (0-based): ``Retry-After`` if given, else ``base * 2^n``."""
    hinted = retry_after_seconds(exc) if exc is not None else None
    delay = hinted if hinted is not None else base_s * (2**attempt)
    return min(delay, max_s)


async def with_retries[T](
    call: Callable[[], Awaitable[T]],
    *,
    max_retries: int,
    base_delay_s: float = 1.0,
    max_delay_s: float = 60.0,
    sleep: Sleep = asyncio.sleep,
) -> T:
    """Run ``call``; retry transient failures up to ``max_retries`` times with backoff."""
    attempt = 0
    while True:
        try:
            return await call()
        except Exception as exc:
            if attempt >= max_retries or not is_retryable(exc):
                raise
            await sleep(backoff_delay(attempt, exc, base_s=base_delay_s, max_s=max_delay_s))
            attempt += 1
