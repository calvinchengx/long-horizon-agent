"""Shell tool: run a command (argv list, no shell) inside the sandbox.

``argv`` is declared as the spec's ``command_arg`` so the dispatcher screens it with
``lha.safety.commands.classify_command`` and routes irreversible commands to a human gate.
"""

from __future__ import annotations

from lha.contracts.tools import ToolContext, ToolResult, ToolSpec
from lha.execution.proc import MAX_TIMEOUT_S
from lha.execution.tools.limits import MAX_TOOL_OUTPUT


class ShellTool:
    spec = ToolSpec(
        name="run_command",
        description="Run a command (argv list, NO shell) in the workspace; returns output + exit code.",
        parameters={
            "type": "object",
            "properties": {
                "argv": {"type": "array", "items": {"type": "string"}},
                "timeout_s": {"type": "integer", "minimum": 1},
            },
            "required": ["argv"],
        },
        mutating=True,
        command_arg="argv",
    )

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        raw_argv = arguments.get("argv")
        if not isinstance(raw_argv, list) or not raw_argv:
            return ToolResult.failure("'argv' must be a non-empty list of strings")
        if not all(isinstance(token, str) for token in raw_argv):
            return ToolResult.failure("'argv' entries must all be strings")
        argv = [str(token) for token in raw_argv]

        raw_timeout = arguments.get("timeout_s", 600)
        if isinstance(raw_timeout, bool) or not isinstance(raw_timeout, int) or raw_timeout < 1:
            return ToolResult.failure("'timeout_s' must be a positive integer")
        timeout_s = min(raw_timeout, MAX_TIMEOUT_S)

        result = await ctx.session.exec(argv, timeout_s=timeout_s)
        body = (
            f"exit_code={result.exit_code}\n"
            f"--- stdout ---\n{result.stdout}\n"
            f"--- stderr ---\n{result.stderr}"
        )
        body = body if len(body) <= MAX_TOOL_OUTPUT else body[:MAX_TOOL_OUTPUT] + "\n…[truncated]"
        return ToolResult(
            ok=result.ok,
            content=body,
            error=None if result.ok else f"exit {result.exit_code}",
        )
