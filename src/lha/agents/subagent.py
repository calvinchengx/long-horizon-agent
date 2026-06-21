"""A generic sub-agent: runs a role's bounded ReAct loop and returns a condensed brief.

Read-only roles (researcher/reviewer/auditor) use this directly — they investigate via tools and
return a ~1-2K-token brief, never writing the integration line. It reuses the same action parser
as the Lead's loop, with the role's system prompt and a scoped (egress-policy) dispatcher.

The role's tool policy is enforced HERE as well as in the dispatcher: a role without
``allow_mutating`` (or ``allow_egress``) is never shown those tools, and any call to one is refused
without being dispatched — defense in depth against a permissive dispatcher.
"""

from __future__ import annotations

from dataclasses import dataclass

from lha.agent.loop import parse_action
from lha.agent.prompt import ACTION_INSTRUCTIONS, render_tools
from lha.agents.roles import RoleSpec
from lha.contracts.model import ModelMessage, ModelProvider, ToolCall
from lha.contracts.tools import ToolContext, ToolDispatcher, ToolResult, ToolSpec

_OBSERVATION_CAP = 4000
_BRIEF_CAP = 8000


@dataclass
class SubAgentResult:
    role: str
    brief: str
    tool_calls: int
    turns: int
    # The raw text of the final model turn (structured verdicts are parsed from it).
    final_text: str = ""
    # Set when the sub-agent failed (e.g. a model/API error in a fan-out); ``brief`` is then empty.
    error: str | None = None


class SubAgent:
    """Runs one role as a bounded, read-or-scoped loop returning a condensed artifact."""

    def __init__(
        self,
        *,
        role: RoleSpec,
        model: ModelProvider,
        dispatcher: ToolDispatcher,
        max_turns: int | None = None,
    ) -> None:
        self._role = role
        self._model = model
        self._dispatcher = dispatcher
        self._max_turns = max_turns or role.max_turns

    def _role_permits(self, spec: ToolSpec) -> bool:
        if spec.mutating and not self._role.allow_mutating:
            return False
        return not (spec.egress and not self._role.allow_egress)

    def visible_specs(self) -> list[ToolSpec]:
        """The tools this role may see and call (dispatcher permits ∩ role policy)."""
        return [s for s in self._dispatcher.specs() if self._role_permits(s)]

    async def _dispatch(self, call: ToolCall, ctx: ToolContext, allowed: set[str]) -> ToolResult:
        if call.name not in allowed:
            return ToolResult.failure(
                f"tool {call.name!r} is not available to the {self._role.name} role"
            )
        return await self._dispatcher.dispatch(call, ctx)

    async def run(
        self, *, objective: str, ctx: ToolContext, extra_context: str = ""
    ) -> SubAgentResult:
        specs = self.visible_specs()
        allowed = {s.name for s in specs}
        system = (
            f"{self._role.system_prompt}\n\n"
            f"Available tools:\n{render_tools(specs)}\n\n"
            f"{ACTION_INSTRUCTIONS}"
        )
        messages = [
            ModelMessage(role="system", content=system),
            ModelMessage(role="user", content=f"{objective}\n\n{extra_context}".strip()),
        ]

        tool_calls = 0
        turns = 0
        brief = ""
        final_text = ""
        for turns in range(1, self._max_turns + 1):
            result = await self._model.complete(messages)
            final_text = result.text

            if result.tool_calls:
                # Native tool use: keep the calls on the assistant turn and answer each by id.
                messages.append(
                    ModelMessage(
                        role="assistant", content=result.text, tool_calls=result.tool_calls
                    )
                )
                for native in result.tool_calls:
                    tool_result = await self._dispatch(native, ctx, allowed)
                    tool_calls += 1
                    observation = (tool_result.content or tool_result.error or "")[
                        :_OBSERVATION_CAP
                    ]
                    messages.append(
                        ModelMessage(role="tool", content=observation, tool_call_id=native.id)
                    )
                continue

            action = parse_action(result.text, [])
            if action.done or action.tool is None:
                brief = action.summary or result.text
                break
            call = ToolCall(
                id=f"{self._role.name}-{turns}", name=action.tool, arguments=action.arguments
            )
            tool_result = await self._dispatch(call, ctx, allowed)
            tool_calls += 1
            observation = (tool_result.content or tool_result.error or "")[:_OBSERVATION_CAP]
            messages.append(ModelMessage(role="assistant", content=result.text))
            messages.append(
                ModelMessage(role="user", content=f"OBSERVATION ({action.tool}): {observation}")
            )

        if not brief:
            # Turn budget exhausted (or the parser never saw a done signal): keep what we have.
            brief = final_text

        return SubAgentResult(
            role=self._role.name,
            brief=brief[:_BRIEF_CAP],
            tool_calls=tool_calls,
            turns=turns,
            final_text=final_text,
        )
