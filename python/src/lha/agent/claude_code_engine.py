"""The ``claude_code`` lead engine: one lead cycle is one ``claude -p`` session.

With ``LHA_LEAD_ENGINE=claude_code`` the built-in turn loop is replaced, for the lead only, by
Claude Code's own agentic loop. Everything around it is unchanged: LHA picks the item, recites
the mission anchor, then (after the session) runs the deterministic checks and the item's
witnesses, commits a verified result or rolls a failed attempt back, and replans blocked items.
A fresh session each cycle is deliberate: the anchor in git is the memory, not the chat.

Tools (``LHA_CLAUDE_CODE_TOOLS``):

- ``lha`` (default): Claude Code's built-in tools are switched off (``--tools ""``) and LHA's
  tools are served to it over MCP (``lha.agent.mcp_bridge``). Every command runs in LHA's
  sandbox, irreversible commands go to the human gate, and egress follows the allow-list.
- ``native``: Claude Code uses its own Read/Edit/Bash on the host workdir. Nothing is isolated,
  so it needs ``sandbox=local`` with ``allow_unsafe_local``. A deny list keeps git history,
  publishing and the web out of reach, but a prefix deny list is not a safety boundary.

In both modes the session also gets a ``verify`` tool: it runs the mission's checks and the
item's witnesses exactly as the harness will afterwards, so Claude Code can iterate to green
before it stops. LHA still verifies again after the session; ``verify`` never marks anything done.

Budget: the session is authorized up front with ``LHA_CLAUDE_CODE_MAX_BUDGET_USD`` as its worst
case, or what is left of the budget when that is less (also passed as ``--max-budget-usd``), and
the ``total_cost_usd`` Claude Code reports is recorded in the mission's ledger. A session killed at
its timeout reports no total: it is charged the tokens it streamed so far, priced
(``SessionProgress.spent_usd``), or its whole cap when a model it used has no price.

Progress: ``on_progress`` sees each turn and tool call as the session makes it (``stream-json``);
the lead loop records them as ``session_progress`` events.
"""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field

from lha.agent.mcp_bridge import BridgeTool, McpBridge, bridge_tool
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, Usage
from lha.contracts.tools import ToolResult, ToolSpec
from lha.contracts.verify import VerificationResult
from lha.governor.metering import MeteredModel, run_external
from lha.model.claude_code import (
    DEFAULT_MODEL,
    ClaudeCodeError,
    ClaudeCodeResult,
    ClaudeCodeTimeout,
    SessionProgress,
    base_args,
    call_budget_usd,
    run_claude,
)

# Claude Code's built-in tools a native session may use.
NATIVE_TOOLS = ("Read", "Edit", "MultiEdit", "Write", "Glob", "Grep", "LS", "Bash", "TodoWrite")
# Denied in a native session: LHA owns git history, and publishing and the web need a human or
# the egress policy. Prefix rules only: ``sh -c 'git push'`` is not caught (see module docstring).
NATIVE_DENY = (
    "Bash(git commit:*)",
    "Bash(git push:*)",
    "Bash(git reset:*)",
    "Bash(git checkout:*)",
    "Bash(git switch:*)",
    "Bash(git rebase:*)",
    "Bash(git merge:*)",
    "Bash(git tag:*)",
    "Bash(git stash:*)",
    "Bash(gh:*)",
    "Bash(npm publish:*)",
    "Bash(pnpm publish:*)",
    "Bash(uv publish:*)",
    "Bash(twine upload:*)",
    "Bash(docker push:*)",
    "Bash(curl:*)",
    "Bash(wget:*)",
    "WebFetch",
    "WebSearch",
)
# Tools a native session still gets from LHA over MCP.
_NATIVE_BRIDGED = ("record_decision",)
_VERIFY_DESCRIPTION = (
    "Run the mission's deterministic checks and this item's witnesses, exactly as the harness "
    "will after this session. Returns PASSED or the failure report. Call it when you think the "
    "item is done; it does not mark anything done."
)

Dispatch = Callable[[ToolCall], Awaitable[ToolResult]]
Verify = Callable[[], Awaitable[VerificationResult]]


@dataclass
class EngineRun:
    """What one session did, for the cycle's checkpoint and trace."""

    summary: str
    turns: int = 0
    tool_calls: int = 0
    tools_used: list[str] = field(default_factory=list)
    # The session could change files without going through LHA's dispatcher (native tools).
    edits_untracked: bool = False
    # Set when the session stopped early (time or spend cap); its work is still verified.
    stopped: str | None = None
    session_id: str | None = None


def verification_text(result: VerificationResult) -> str:
    if result.all_green:
        return "PASSED: every check and witness is green."
    if result.verdict == "unverified":
        return "UNVERIFIED: this mission has no checks to run."
    return f"FAILED:\n{result.failure_report()}"


class ClaudeCodeEngine:
    """Runs a lead cycle as one ``claude -p`` session (see the module docstring)."""

    def __init__(
        self,
        *,
        binary: str = "claude",
        model: str = DEFAULT_MODEL,
        tools: str = "lha",
        max_budget_usd: float = 5.0,
        timeout_s: float = 3600.0,
    ) -> None:
        if tools not in ("lha", "native"):
            raise ValueError(f"unknown Claude Code tool mode: {tools!r}")
        self._binary = binary
        self._model = model
        self._tools = tools
        self._max_budget_usd = max_budget_usd
        self._timeout_s = timeout_s
        self.name = f"claude_code_engine:{model}"

    @property
    def native(self) -> bool:
        return self._tools == "native"

    def _bridge_tools(
        self, specs: list[ToolSpec], dispatch: Dispatch, verify: Verify, cycle_id: str
    ) -> tuple[list[BridgeTool], list[str]]:
        used: list[str] = []

        def handler(spec: ToolSpec) -> Callable[[dict[str, object]], Awaitable[tuple[str, bool]]]:
            async def _call(arguments: dict[str, object]) -> tuple[str, bool]:
                used.append(spec.name)
                call = ToolCall(
                    id=f"{cycle_id}-cc-{len(used)}", name=spec.name, arguments=arguments
                )
                result = await dispatch(call)
                return (result.content if result.ok else result.error or result.content), (
                    not result.ok
                )

            return _call

        async def _verify(_: dict[str, object]) -> tuple[str, bool]:
            result = await verify()
            return verification_text(result), not result.all_green

        bridged = [s for s in specs if not self.native or s.name in _NATIVE_BRIDGED]
        tools = [bridge_tool(s.name, s.description, s.parameters, handler(s)) for s in bridged]
        tools.append(bridge_tool("verify", _VERIFY_DESCRIPTION, {}, _verify))
        return tools, used

    def _args(
        self, bridge: McpBridge, system: str, max_budget_usd: float | None = None
    ) -> list[str]:
        budget = self._max_budget_usd if max_budget_usd is None else max_budget_usd
        args = [
            *base_args(model=self._model, max_budget_usd=budget),
            "--strict-mcp-config",
            "--mcp-config",
            json.dumps(bridge.mcp_config()),
            "--append-system-prompt",
            system,
        ]
        if self.native:
            args += ["--allowedTools", *NATIVE_TOOLS, *bridge.allowed_tools()]
            args += ["--disallowedTools", *NATIVE_DENY]
        else:
            args += ["--tools", "", "--allowedTools", *bridge.allowed_tools()]
        return args

    async def run(
        self,
        *,
        messages: list[ModelMessage],
        cwd: str,
        cycle_id: str,
        specs: list[ToolSpec],
        dispatch: Dispatch,
        verify: Verify,
        meter: ModelProvider | None = None,
        on_progress: Callable[[SessionProgress], None] | None = None,
    ) -> EngineRun:
        """One session on ``messages`` (system + task) in ``cwd``; metered through ``meter``."""
        system = "\n\n".join(m.content for m in messages if m.role == "system")
        prompt = "\n\n".join(m.content for m in messages if m.role != "system")
        tools, used = self._bridge_tools(specs, dispatch, verify, cycle_id)
        # The session's cap is what is left of the budget when that is less than the configured cap.
        budget = self._max_budget_usd
        if isinstance(meter, MeteredModel):
            budget = call_budget_usd(budget, meter.remaining_usd)

        async def _session() -> tuple[EngineRun, Usage]:
            async with McpBridge(tools) as bridge:
                try:
                    result: ClaudeCodeResult = await run_claude(
                        self._args(bridge, system, budget),
                        prompt=prompt,
                        binary=self._binary,
                        cwd=cwd,
                        timeout_s=self._timeout_s,
                        provider=self.name,
                        fallback_model=self._model,
                        on_progress=on_progress,
                    )
                except ClaudeCodeTimeout as exc:
                    # The work so far is still in the workdir: verify it rather than drop it, and
                    # charge what the session spent (its cap when a model has no price).
                    spent = exc.progress.usage(provider=self.name, fallback_model=self._model)
                    return self._stopped(str(exc), used), spent
                except ClaudeCodeError as exc:
                    if exc.subtype is None or not exc.subtype.startswith("error_max"):
                        raise
                    return self._stopped(str(exc), used), exc.usage or Usage(provider=self.name)
            run = EngineRun(
                summary=result.text,
                turns=result.num_turns,
                tool_calls=len(used),
                tools_used=list(used),
                edits_untracked=self.native,
                session_id=result.session_id,
            )
            return run, result.usage

        return await run_external(meter, _session, worst_case_usd=budget)

    def _stopped(self, reason: str, used: list[str]) -> EngineRun:
        return EngineRun(
            summary="",
            tool_calls=len(used),
            tools_used=list(used),
            edits_untracked=self.native,
            stopped=reason,
        )
