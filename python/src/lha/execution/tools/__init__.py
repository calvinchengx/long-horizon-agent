"""Concrete tools + a default toolset factory."""

from lha.contracts.tools import Tool
from lha.execution.tools.decisions import (
    RECORD_DECISION,
    DecisionBuffer,
    DecisionSink,
    DecisionToolDispatcher,
    RecordDecisionTool,
    with_decision_tool,
)
from lha.execution.tools.fs import GrepTool, ListFilesTool, ReadFileTool, WriteFileTool
from lha.execution.tools.leases import (
    REQUEST_LEASE,
    LeaseToolDispatcher,
    with_lease_tool,
)
from lha.execution.tools.shell import ShellTool
from lha.execution.tools.web import FetchUrlTool, WebSearchTool


def default_local_tools() -> list[Tool]:
    """The non-egress tool set (file IO + shell) every mission gets by default.

    ``record_decision`` is not in this list: it is bound to a mission's decision sink and added
    by ``with_decision_tool`` (``lha.agent.assembly.build_lead_loop`` does this with the anchor).
    """
    return [ReadFileTool(), WriteFileTool(), ListFilesTool(), GrepTool(), ShellTool()]


__all__ = [
    "RECORD_DECISION",
    "REQUEST_LEASE",
    "DecisionBuffer",
    "DecisionSink",
    "DecisionToolDispatcher",
    "FetchUrlTool",
    "GrepTool",
    "LeaseToolDispatcher",
    "ListFilesTool",
    "ReadFileTool",
    "RecordDecisionTool",
    "ShellTool",
    "WebSearchTool",
    "WriteFileTool",
    "default_local_tools",
    "with_decision_tool",
    "with_lease_tool",
]
