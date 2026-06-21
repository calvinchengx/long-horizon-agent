"""In-session context compaction.

When a multi-turn cycle's message list grows, summarize the older turns into one compact,
state-carrying note (task / decisions / discoveries incl. failed approaches / next steps),
keeping the system message, the original task message and the most recent turns verbatim. This
holds long sessions inside the context window without losing load-bearing facts.

The summarizer sees the NEWEST part of the older transcript (each message individually clipped),
and the kept tail never starts with an orphaned tool observation whose action was summarized.
"""

from __future__ import annotations

from lha.contracts.model import ModelMessage, ModelProvider

_SUMMARY_INSTRUCTIONS = (
    "Summarize the conversation so far into a compact, state-carrying note. Preserve: the task, "
    "key decisions (and why), discoveries INCLUDING failed approaches, and next steps. Be terse."
)
_TRANSCRIPT_CAP = 12000
_PER_MESSAGE_CAP = 2000


def _is_observation(message: ModelMessage) -> bool:
    return message.role == "tool" or (
        message.role == "user" and message.content.startswith("OBSERVATION")
    )


def _clip(text: str, cap: int) -> str:
    if len(text) <= cap:
        return text
    half = cap // 2
    return f"{text[:half]}\n...[clipped]...\n{text[-half:]}"


async def compact_messages(
    *,
    model: ModelProvider,
    messages: list[ModelMessage],
    keep_last: int = 4,
    keep_task: bool = True,
) -> list[ModelMessage]:
    """Return ``system + task + summary-of-older + last ~keep_last turns`` (or ``messages``)."""
    system = [m for m in messages if m.role == "system"][:1]
    body = [m for m in messages if m.role != "system"]
    task = body[:1] if keep_task and body and body[0].role == "user" else []
    rest = body[len(task) :]
    if len(rest) <= keep_last:
        return messages

    split = len(rest) - keep_last
    # Keep an action and its observation together: don't start the kept tail on an observation.
    while split > 0 and _is_observation(rest[split]):
        split -= 1
    to_summarize, recent = rest[:split], rest[split:]
    if not to_summarize:
        return messages

    transcript = "\n".join(f"{m.role}: {_clip(m.content, _PER_MESSAGE_CAP)}" for m in to_summarize)
    if len(transcript) > _TRANSCRIPT_CAP:
        transcript = "...[older turns trimmed]...\n" + transcript[-_TRANSCRIPT_CAP:]
    result = await model.complete(
        [
            ModelMessage(role="system", content=_SUMMARY_INSTRUCTIONS),
            ModelMessage(role="user", content=transcript),
        ]
    )
    summary = ModelMessage(
        role="user", content=f"[compacted summary of earlier turns]\n{result.text[:4000]}"
    )
    return [*system, *task, summary, *recent]
