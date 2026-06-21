"""Concrete tools + a default toolset factory."""

from lha.contracts.tools import Tool
from lha.execution.tools.fs import GrepTool, ListFilesTool, ReadFileTool, WriteFileTool
from lha.execution.tools.shell import ShellTool
from lha.execution.tools.web import FetchUrlTool, WebSearchTool


def default_local_tools() -> list[Tool]:
    """The non-egress tool set (file IO + shell) every mission gets by default."""
    return [ReadFileTool(), WriteFileTool(), ListFilesTool(), GrepTool(), ShellTool()]


__all__ = [
    "FetchUrlTool",
    "GrepTool",
    "ListFilesTool",
    "ReadFileTool",
    "ShellTool",
    "WebSearchTool",
    "WriteFileTool",
    "default_local_tools",
]
