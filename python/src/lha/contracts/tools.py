"""The tool contract.

The model never touches the world directly: it emits ``ToolCall`` requests, and a
``ToolDispatcher`` decides whether to run them (allow-list, mutating/egress policy, arg
validation, protected paths, human gates for irreversible commands) before handing off to a concrete ``Tool`` that executes inside the sandbox. Tools
return a compact ``ToolResult`` (large payloads spill to the object store via ``artifact_ref``).
Standardizing here is what lets the tool surface grow (MCP, web/deep-research, ...) without the
agent loop changing.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol, runtime_checkable

from pydantic import BaseModel, Field

from lha.contracts.model import ToolCall
from lha.contracts.sandbox import SandboxSession


class ToolSpec(BaseModel):
    """Describes a tool to the model (JSON-Schema parameters) and to the dispatcher (policy)."""

    name: str
    description: str
    parameters: dict[str, object] = Field(default_factory=dict)  # JSON Schema for the arguments
    mutating: bool = False  # performs writes / side effects
    egress: bool = False  # reaches the network (gated by default-deny)
    untrusted_input: bool = False  # returns content from outside the trust boundary (web pages...)
    # Argument names holding workspace paths: a mutating tool may not target .lha/ or .git/.
    path_args: list[str] = Field(default_factory=list)
    # Argument name holding an argv list, screened for irreversible commands (routed to a gate).
    command_arg: str | None = None


class ToolResult(BaseModel):
    """A compact, model-facing tool result."""

    ok: bool
    content: str = ""
    artifact_ref: str | None = None  # pointer to a large payload in the object store
    error: str | None = None

    @classmethod
    def success(cls, content: str = "", *, artifact_ref: str | None = None) -> ToolResult:
        return cls(ok=True, content=content, artifact_ref=artifact_ref)

    @classmethod
    def failure(cls, error: str) -> ToolResult:
        return cls(ok=False, error=error)


@dataclass
class ToolContext:
    """Live, non-serializable context handed to a tool at execution time."""

    mission_id: str
    session: SandboxSession


@runtime_checkable
class Tool(Protocol):
    """A single capability. Implementations live in ``src/lha/execution/tools/``."""

    spec: ToolSpec

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        """Execute the tool with validated ``arguments`` and return a compact result."""
        ...


@runtime_checkable
class ToolDispatcher(Protocol):
    """Gates and routes tool calls. Implementation: ``src/lha/execution/dispatcher.py``."""

    def specs(self) -> list[ToolSpec]:
        """The specs of tools currently permitted (what the model is told it can call)."""
        ...

    async def dispatch(self, call: ToolCall, ctx: ToolContext) -> ToolResult:
        """Validate + execute a tool call (never raises; failures come back as ``ToolResult``)."""
        ...
