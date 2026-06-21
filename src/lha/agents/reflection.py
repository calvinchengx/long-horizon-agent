"""Within-mission reflection (Reflexion-style).

On a failed verification, the agent writes a short natural-language post-mortem ("what went wrong,
what to try next") that is stored in episodic memory and prepended to the next attempt — a cheap,
text-space self-improvement that measurably raises retry success without touching prompts/weights.
"""

from __future__ import annotations

from lha.contracts.model import ModelMessage, ModelProvider

_REFLECTION_CAP = 2000


async def reflect_on_failure(
    *, model: ModelProvider, item_description: str, failure_summary: str
) -> str:
    """Produce a short post-mortem for a failed attempt (no code, just lessons)."""
    result = await model.complete(
        [
            ModelMessage(
                role="system",
                content=(
                    "You are reflecting on a failed attempt (Reflexion-style). Write a SHORT "
                    "post-mortem: the likely root cause and a concrete different approach to try "
                    "next. No code."
                ),
            ),
            ModelMessage(
                role="user",
                content=f"Item: {item_description}\n\nFailure:\n{failure_summary[:4000]}",
            ),
        ]
    )
    return result.text[:_REFLECTION_CAP]
