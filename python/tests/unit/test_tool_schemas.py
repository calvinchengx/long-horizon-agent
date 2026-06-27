"""Tests for native tool-schema conversion."""

from __future__ import annotations

from lha.contracts.tools import ToolSpec
from lha.model.tool_schemas import to_claude_tools, to_openai_tools

_SPEC = ToolSpec(
    name="read_file",
    description="Read a file.",
    parameters={"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]},
)


def test_to_openai_tools_shape() -> None:
    tools = to_openai_tools([_SPEC])
    assert tools[0]["type"] == "function"
    fn = tools[0]["function"]
    assert isinstance(fn, dict)
    assert fn["name"] == "read_file"
    assert fn["parameters"]["required"] == ["path"]


def test_to_claude_tools_shape() -> None:
    tools = to_claude_tools([_SPEC])
    assert tools[0]["name"] == "read_file"
    assert tools[0]["input_schema"]["properties"]["path"]["type"] == "string"


def test_empty_params_default() -> None:
    spec = ToolSpec(name="noop", description="d")
    assert to_openai_tools([spec])[0]["function"]["parameters"] == {
        "type": "object",
        "properties": {},
    }
