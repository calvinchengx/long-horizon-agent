"""A deterministic stub model — an HONEST test double, never presented as a real run.

It implements the real ``ModelProvider`` interface so the durable spine, anchor, and tests can
run instantly, offline, and at $0 — without a real LLM. Its output is a deterministic function
of the input (so durability/replay tests are reproducible), and its ``name`` is explicitly
``stub:*`` so it can never be mistaken for genuine model output.

Optionally, a script of canned ``TurnResult`` objects can be supplied to drive a specific
scenario in a test (e.g. "fail verification once, then fix it").
"""

from __future__ import annotations

import hashlib

from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage


class StubModel(ModelProvider):
    """Deterministic, offline ``ModelProvider`` for tests and CI."""

    def __init__(self, model_name: str = "stub-1", script: list[TurnResult] | None = None) -> None:
        # Prefix makes it unmistakable in logs/traces that this is NOT a real model.
        self.name = f"stub:{model_name}"
        self._script = list(script) if script else None
        self._turn = 0

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        # Scripted mode: return the next pre-baked turn (cycling on the last entry).
        if self._script is not None:
            result = self._script[min(self._turn, len(self._script) - 1)]
            self._turn += 1
            return result.model_copy(deep=True)

        # Default mode: a deterministic echo derived from the conversation, with real-looking
        # (but synthetic) token counts so cost plumbing can be exercised.
        prompt = "\n".join(f"{m.role}: {m.content}" for m in messages)
        digest = hashlib.sha256(prompt.encode("utf-8")).hexdigest()[:8]
        in_tokens = max(1, len(prompt) // 4)
        text = f"[stub:{digest}] acknowledged {len(messages)} message(s)."
        self._turn += 1
        return TurnResult(
            text=text,
            thinking=f"deterministic stub reasoning for digest {digest}",
            usage=Usage(input_tokens=in_tokens, output_tokens=len(text) // 4, model=self.name),
            stop_reason="end_turn",
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        # A stub is free, by definition.
        return 0.0
