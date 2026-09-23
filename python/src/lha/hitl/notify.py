"""Gate notifications: an optional webhook that receives every gate event as a JSON POST.

Off unless ``LHA_GATE_WEBHOOK_URL`` is set. Delivery is best-effort and never raises: a slow or
failing receiver can delay a gate event by at most ``timeout`` seconds and can never approve,
deny or stall anything. Durable missions call this from the ``notify_gate`` activity (never from
workflow code); local runs call it from the terminal approver's worker thread.
"""

from __future__ import annotations

from typing import Any

import httpx

WEBHOOK_OFF = "off"
WEBHOOK_SENT = "sent"


def _outcome(status_code: int) -> str:
    return WEBHOOK_SENT if 200 <= status_code < 300 else f"failed: HTTP {status_code}"


async def post_webhook(
    url: str | None,
    payload: dict[str, Any],
    *,
    timeout: float,
    transport: httpx.AsyncBaseTransport | None = None,
) -> str:
    """POST ``payload``; return ``"off"`` (no URL), ``"sent"`` or ``"failed: <why>"``."""
    if not url:
        return WEBHOOK_OFF
    try:
        async with httpx.AsyncClient(timeout=timeout, transport=transport) as client:
            response = await client.post(url, json=payload)
        return _outcome(response.status_code)
    except Exception as exc:  # never let a notification break a gate
        return f"failed: {type(exc).__name__}"


def post_webhook_sync(
    url: str | None,
    payload: dict[str, Any],
    *,
    timeout: float,
    transport: httpx.BaseTransport | None = None,
) -> str:
    """Blocking twin of :func:`post_webhook` (for code already running in a worker thread)."""
    if not url:
        return WEBHOOK_OFF
    try:
        with httpx.Client(timeout=timeout, transport=transport) as client:
            response = client.post(url, json=payload)
        return _outcome(response.status_code)
    except Exception as exc:
        return f"failed: {type(exc).__name__}"
