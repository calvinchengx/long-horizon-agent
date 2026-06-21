"""Convert ``ToolSpec``s to provider-native tool-calling schemas.

The agent loop is provider-agnostic (JSON-action protocol), but when a backend supports native
tool-calling we can hand it real schemas for higher reliability. These helpers produce the OpenAI
function-calling and Anthropic tool shapes from our neutral ``ToolSpec``.
"""

from __future__ import annotations

from lha.contracts.tools import ToolSpec

_EMPTY_SCHEMA: dict[str, object] = {"type": "object", "properties": {}}


def to_openai_tools(specs: list[ToolSpec]) -> list[dict[str, object]]:
    """OpenAI / OpenAI-compatible `tools` array (function-calling)."""
    return [
        {
            "type": "function",
            "function": {
                "name": spec.name,
                "description": spec.description,
                "parameters": spec.parameters or _EMPTY_SCHEMA,
            },
        }
        for spec in specs
    ]


def to_claude_tools(specs: list[ToolSpec]) -> list[dict[str, object]]:
    """Anthropic Messages API `tools` array."""
    return [
        {
            "name": spec.name,
            "description": spec.description,
            "input_schema": spec.parameters or _EMPTY_SCHEMA,
        }
        for spec in specs
    ]
