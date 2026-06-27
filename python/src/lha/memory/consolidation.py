"""Sleep-time memory consolidation: episodes → durable semantic facts.

Runs in the background (e.g. during a durable-sleep window). Uses the model to distill episodes
into reusable facts, in bounded batches, with a deterministic fallback so it never fails: if the
model's reply is unusable the episodes are kept verbatim (and the failure is reported), never
dropped. Forgetting is conservative: ``soft_invalidate`` flips ``valid=False`` (never an in-place LLM rewrite of a fact,
which would risk turning a hallucination into 'remembered truth').
"""

from __future__ import annotations

import json
from collections.abc import Callable
from dataclasses import dataclass, field

from lha.contracts.memory import MemoryRecord
from lha.contracts.model import ModelMessage, ModelProvider
from lha.ids import new_id

_INSTRUCTIONS = (
    "Distill the episodes below into a short list of durable, reusable FACTS about the "
    "repository/domain (each a single sentence). Reply with ONLY a JSON array of strings."
)


#: Episodes sent to the model per consolidation call (bounds prompt size; all batches run).
DEFAULT_BATCH_SIZE = 200


@dataclass
class ConsolidationResult:
    """Outcome of a consolidation pass.

    ``facts`` holds every distilled fact plus, for any batch whose model reply could not be
    parsed, that batch's episodes verbatim (tagged ``metadata["consolidation"] == "verbatim"``) —
    nothing is dropped silently. ``failed_batches`` > 0 signals that the model output was
    unusable for those batches, so the caller can log/alert/retry.
    """

    facts: list[MemoryRecord] = field(default_factory=list)
    batches: int = 0
    failed_batches: int = 0

    @property
    def ok(self) -> bool:
        return self.failed_batches == 0


def _parse_facts(text: str) -> list[str] | None:
    """Parse a JSON array of facts; ``None`` means the reply was unparseable."""
    start = text.find("[")
    end = text.rfind("]")
    if start != -1 and end != -1 and end > start:
        try:
            parsed = json.loads(text[start : end + 1])
        except ValueError:
            return None
        if isinstance(parsed, list):
            return [str(x).strip() for x in parsed if str(x).strip()]
    return None


async def consolidate(
    *,
    model: ModelProvider,
    episodes: list[str],
    mission_id: str | None = None,
    batch_size: int = DEFAULT_BATCH_SIZE,
) -> ConsolidationResult:
    """Distill ALL ``episodes`` (in bounded batches) into semantic ``MemoryRecord`` facts.

    Each batch of at most ``batch_size`` episodes is one model call. If a batch's reply can't be
    parsed, every distinct episode of that batch is kept verbatim as a fact and the failure is
    counted in the result — the pass never silently loses episodes.
    """
    if batch_size < 1:
        raise ValueError("batch_size must be >= 1")
    result = ConsolidationResult()
    base_meta = {"mission_id": mission_id} if mission_id else {}
    for offset in range(0, len(episodes), batch_size):
        batch = episodes[offset : offset + batch_size]
        result.batches += 1
        joined = "\n".join(f"- {episode}" for episode in batch)
        reply = await model.complete(
            [
                ModelMessage(role="system", content="You are the Librarian; consolidate memory."),
                ModelMessage(role="user", content=f"{_INSTRUCTIONS}\n\nEpisodes:\n{joined}"),
            ]
        )
        facts = _parse_facts(reply.text)
        metadata = dict(base_meta)
        if facts is None:
            # Unusable model output: keep the whole batch verbatim rather than dropping any of it.
            result.failed_batches += 1
            facts = list(dict.fromkeys(batch))
            metadata["consolidation"] = "verbatim"
        result.facts.extend(
            MemoryRecord(id=new_id("fact"), kind="semantic", text=fact, metadata=dict(metadata))
            for fact in facts
        )
    return result


def soft_invalidate(
    records: list[MemoryRecord], *, predicate: Callable[[MemoryRecord], bool]
) -> int:
    """Soft-forget: mark matching records invalid (never deletes; never rewrites). Returns count."""
    count = 0
    for record in records:
        if record.valid and predicate(record):
            record.valid = False
            count += 1
    return count
