"""Lead engine via the Claude Agent SDK (optional).

The default Lead uses our provider-agnostic loop. When the ``claude`` extra + Claude auth are
available, this runs the Lead's work through the Claude Agent SDK's own agentic loop — with its
built-in tools and server-side compaction / context-editing / memory tool — which is the most
capable Lead.

The SDK streams typed messages: ``AssistantMessage`` (``content`` = list of blocks; ``TextBlock``
has ``.text``), ``UserMessage``, ``SystemMessage`` and a final ``ResultMessage`` (``result`` text,
``session_id``, ``total_cost_usd``, ``is_error``). Messages have no top-level ``.text``, so this
reads the content blocks and prefers the ``ResultMessage.result``. It is coded defensively
(duck-typed, also accepting dict blocks) against SDK version differences.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass


@dataclass
class SdkRunResult:
    text: str
    session_id: str | None
    cost_usd: float | None = None
    is_error: bool = False


def _block_text(block: object) -> str | None:
    if isinstance(block, dict):
        if block.get("type", "text") != "text":
            return None
        text = block.get("text")
    else:
        text = getattr(block, "text", None)
    return text if isinstance(text, str) else None


def _content_texts(content: object) -> list[str]:
    if isinstance(content, str):
        return [content]
    if not isinstance(content, Iterable):
        return []
    return [t for t in (_block_text(b) for b in content) if t]


def collect_sdk_messages(messages: Iterable[object]) -> SdkRunResult:
    """Fold a stream of SDK messages into the final text + session id (+ cost)."""
    assistant_texts: list[str] = []
    result_text: str | None = None
    session_id: str | None = None
    cost: float | None = None
    is_error = False
    for message in messages:
        sid = getattr(message, "session_id", None)
        if not isinstance(sid, str):
            data = getattr(message, "data", None)
            sid = data.get("session_id") if isinstance(data, dict) else None
        if isinstance(sid, str) and sid:
            session_id = sid

        result = getattr(message, "result", None)
        if isinstance(result, str):
            result_text = result
            total = getattr(message, "total_cost_usd", None)
            cost = float(total) if isinstance(total, int | float) else cost
            is_error = bool(getattr(message, "is_error", False))
            continue

        # Only assistant turns carry the model's text (user turns echo tool results).
        if type(message).__name__ == "UserMessage":
            continue
        assistant_texts.extend(_content_texts(getattr(message, "content", None)))

    text = result_text if result_text else "\n".join(assistant_texts)
    return SdkRunResult(text=text, session_id=session_id, cost_usd=cost, is_error=is_error)


class ClaudeSdkLead:
    """Runs a prompt through the Claude Agent SDK loop, collecting the final text + session id."""

    def __init__(
        self, *, model: str = "claude-sonnet-4-6", cwd: str | None = None, max_turns: int = 30
    ) -> None:
        self._model = model
        self._cwd = cwd
        self._max_turns = max_turns

    async def run(self, prompt: str) -> SdkRunResult:
        from claude_agent_sdk import ClaudeAgentOptions, query

        options = ClaudeAgentOptions(model=self._model, cwd=self._cwd, max_turns=self._max_turns)
        messages: list[object] = [m async for m in query(prompt=prompt, options=options)]
        return collect_sdk_messages(messages)
