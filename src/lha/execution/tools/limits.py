"""Shared limits for tool output (keeps results compact so context stays high-signal)."""

from __future__ import annotations

# Hard cap on a single tool result's text; larger payloads should spill to the object store.
MAX_TOOL_OUTPUT = 25_000
