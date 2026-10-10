"""The ``opencode`` lead engine: one lead cycle is one ``opencode run`` session.

With ``LHA_LEAD_ENGINE=opencode`` the built-in turn loop is replaced, for the lead only, by
OpenCode's own agentic loop. Everything around it is unchanged: LHA picks the item, recites the
mission anchor, then (after the session) runs the deterministic checks and the item's witnesses,
commits a verified result or rolls a failed attempt back, and replans blocked items. A fresh
session each cycle is deliberate: the anchor in git is the memory, not the chat.

Tools (``LHA_OPENCODE_TOOLS``):

- ``lha`` (default): OpenCode's own tools are denied by an injected agent, and LHA's tools are
  served to it over MCP (``lha.agent.mcp_bridge``) as ``lha_<tool>``. Every command runs in LHA's
  sandbox, irreversible commands go to the human gate, and egress follows the allow-list.
- ``native``: OpenCode uses its own read/edit/shell on the host workdir. Nothing is isolated, so
  it needs ``sandbox=local`` with ``allow_unsafe_local``. A deny list keeps git history, publishing
  and the web out of reach, but a prefix deny list is not a safety boundary.

In both modes the session also gets a ``verify`` tool: it runs the mission's checks and the item's
witnesses exactly as the harness will afterwards, so OpenCode can iterate to green before it stops.
LHA still verifies again after the session; ``verify`` never marks anything done.

The agent, the MCP server and the system prompt are injected per session through a temporary
``OPENCODE_CONFIG`` file, and the child's environment is stripped of every ``OPENCODE_*`` marker so
it never attaches to the OpenCode that may be running LHA. Local sessions run ``--standalone`` (a
private server) by default.

Budget: the session is authorized up front with ``LHA_OPENCODE_MAX_BUDGET_USD`` as its worst case,
or what is left of the budget when that is less. OpenCode has no spend-cap flag, so the engine
kills a session that reaches its cap while streaming; the ``cost`` OpenCode reports per step is
recorded in the mission's ledger, and a killed session is charged what it streamed
(``SessionProgress.spent_usd``), or its whole cap when its spend could not be seen.

Progress: ``on_progress`` sees each turn and tool call as the session makes it (``--format json``);
the lead loop records them as ``session_progress`` events.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from lha.agent.claude_code_engine import (
    Dispatch,
    EngineRun,
    Verify,
    verification_text,
)
from lha.agent.mcp_bridge import BridgeTool, McpBridge, bridge_tool
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall, Usage
from lha.contracts.tools import ToolSpec
from lha.governor.metering import MeteredModel, run_external
from lha.model.claude_code import call_budget_usd
from lha.model.opencode import (
    DEFAULT_AGENT,
    DEFAULT_MODEL,
    OpenCodeError,
    OpenCodeTimeout,
    SessionProgress,
    base_args,
    run_opencode,
)

#: Denied in a native session: LHA owns git history, and publishing and the web need a human or
#: the egress policy. Prefix rules only: ``sh -c 'git push'`` is not caught (see module docstring).
NATIVE_DENY: tuple[dict[str, str], ...] = (
    *(
        {"action": "shell", "resource": pattern, "effect": "deny"}
        for pattern in (
            "git commit *",
            "git push *",
            "git reset *",
            "git checkout *",
            "git switch *",
            "git rebase *",
            "git merge *",
            "git tag *",
            "git stash *",
            "gh *",
            "npm publish *",
            "pnpm publish *",
            "uv publish *",
            "twine upload *",
            "docker push *",
            "curl *",
            "wget *",
        )
    ),
    {"action": "webfetch", "resource": "*", "effect": "deny"},
    {"action": "websearch", "resource": "*", "effect": "deny"},
    {"action": "subagent", "resource": "*", "effect": "deny"},
    {"action": "question", "resource": "*", "effect": "deny"},
)
#: The only bridged tools a native session gets from LHA.
_NATIVE_BRIDGED = ("record_decision",)
_VERIFY_DESCRIPTION = (
    "Run the mission's deterministic checks and this item's witnesses, exactly as the harness "
    "will after this session. Returns PASSED or the failure report. Call it when you think the "
    "item is done; it does not mark anything done."
)


class OpenCodeEngine:
    """Runs a lead cycle as one ``opencode run`` session (see the module docstring)."""

    #: The trace event the loop records when the session ends (``loop._engine_session``).
    session_event = "opencode_session"

    def __init__(
        self,
        *,
        binary: str = "opencode",
        model: str = DEFAULT_MODEL,
        agent: str = DEFAULT_AGENT,
        tools: str = "lha",
        standalone: bool = True,
        max_budget_usd: float = 5.0,
        timeout_s: float = 3600.0,
    ) -> None:
        if tools not in ("lha", "native"):
            raise ValueError(f"unknown OpenCode tool mode: {tools!r}")
        self._binary = binary
        self._model = model
        self._agent = agent
        self._tools = tools
        self._standalone = standalone
        self._max_budget_usd = max_budget_usd
        self._timeout_s = timeout_s
        self.name = f"opencode_engine:{model or 'default'}"

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
                    id=f"{cycle_id}-oc-{len(used)}", name=spec.name, arguments=arguments
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

    def _permissions(self) -> list[dict[str, str]]:
        if self.native:
            return [dict(rule) for rule in NATIVE_DENY]
        # Only LHA's bridged tools: deny every other action, then allow the lha_* tools.
        return [
            {"action": "*", "resource": "*", "effect": "deny"},
            {"action": "lha_*", "resource": "*", "effect": "allow"},
        ]

    def _config(self, system: str, bridge: McpBridge) -> dict[str, object]:
        """The per-session OpenCode config: an agent, and LHA's tools over MCP."""
        return {
            "mcp": {"servers": {bridge.name: bridge.opencode_server()}},
            "agents": {
                self._agent: {
                    "description": "LHA lead engine session",
                    "mode": "primary",
                    "system": system,
                    "permissions": self._permissions(),
                }
            },
        }

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
        from lha.model.opencode import _config_file, _injected_env

        system = "\n\n".join(m.content for m in messages if m.role == "system")
        prompt = "\n\n".join(m.content for m in messages if m.role != "system")
        tools, used = self._bridge_tools(specs, dispatch, verify, cycle_id)
        # The session's cap is what is left of the budget when that is less than the configured cap.
        budget = self._max_budget_usd
        if isinstance(meter, MeteredModel):
            budget = call_budget_usd(budget, meter.remaining_usd)

        async def _session() -> tuple[EngineRun, Usage]:
            async with McpBridge(tools) as bridge:
                with _config_file(self._config(system, bridge)) as cfg_path:
                    try:
                        result = await run_opencode(
                            base_args(
                                model=self._model,
                                agent=self._agent,
                                standalone=self._standalone,
                            ),
                            prompt=prompt,
                            binary=self._binary,
                            cwd=cwd,
                            timeout_s=self._timeout_s,
                            provider=self.name,
                            fallback_model=self._model,
                            max_cost_usd=budget,
                            env=_injected_env(cfg_path),
                            on_progress=on_progress,
                        )
                    except OpenCodeTimeout as exc:
                        # The work so far is still in the workdir: verify it rather than drop it.
                        spent = exc.progress.usage(provider=self.name, fallback_model=self._model)
                        return self._stopped(str(exc), used), spent
                    except OpenCodeError as exc:
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
