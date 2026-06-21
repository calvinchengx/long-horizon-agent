"""Secret redaction for observability payloads.

Trace events and span attributes are shipped to logs and (optionally) external collectors, so
anything that looks like a credential is masked before it leaves the process: values under
secret-looking keys, ``SecretStr`` values, and secret-looking strings (provider API keys, bearer
tokens, passwords embedded in URLs/DSNs) wherever they appear.
"""

from __future__ import annotations

import re
from collections.abc import Mapping

from pydantic import SecretStr

REDACTED = "***"

# Keys whose values are always masked. ``token`` only as a whole word/suffix, so usage counters
# like ``input_tokens`` / ``max_tokens`` stay visible.
_SECRET_KEY = re.compile(
    r"api[_-]?key|apikey|secret|passw(or)?d|authorization|credential|private[_-]?key|cookie"
    r"|dsn|(^|[_-])(token|auth)$",
    re.IGNORECASE,
)

_SECRET_VALUE_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = (
    # Provider keys: Anthropic/OpenAI (sk-...), GitHub, Slack, AWS access key ids, Google.
    (re.compile(r"\b(sk-[A-Za-z0-9_\-]{16,})"), REDACTED),
    (re.compile(r"\b(gh[pousr]_[A-Za-z0-9]{20,})"), REDACTED),
    (re.compile(r"\b(xox[abposr]-[A-Za-z0-9-]{10,})"), REDACTED),
    (re.compile(r"\b(AKIA[0-9A-Z]{16})\b"), REDACTED),
    (re.compile(r"\b(AIza[0-9A-Za-z_\-]{30,})"), REDACTED),
    # "Bearer <token>" in headers or messages.
    (re.compile(r"(?i)\b(bearer)\s+[A-Za-z0-9._~+/\-]+=*"), r"\1 " + REDACTED),
    # scheme://user:password@host -> scheme://user:***@host
    (re.compile(r"(\b[a-z][a-z0-9+.\-]*://[^/\s:@]+:)[^@\s/]+@"), r"\1" + REDACTED + "@"),
)


def is_secret_key(key: str) -> bool:
    return bool(_SECRET_KEY.search(key))


def redact_text(text: str) -> str:
    """Mask secret-looking substrings inside free text."""
    for pattern, replacement in _SECRET_VALUE_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


def redact_value(value: object) -> object:
    """Recursively redact ``value`` (mappings, sequences, strings, ``SecretStr``)."""
    if isinstance(value, SecretStr):
        return REDACTED
    if isinstance(value, str):
        return redact_text(value)
    if isinstance(value, Mapping):
        return redact_mapping(value)
    if isinstance(value, list | tuple):
        return [redact_value(item) for item in value]
    return value


def redact_mapping(data: Mapping[str, object]) -> dict[str, object]:
    """Copy of ``data`` with secret-keyed values masked and all other values redacted deeply."""
    out: dict[str, object] = {}
    for key, value in data.items():
        if is_secret_key(str(key)) and value not in (None, ""):
            out[key] = REDACTED
        else:
            out[key] = redact_value(value)
    return out
