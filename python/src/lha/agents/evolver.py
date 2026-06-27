"""Offline prompt evolver (GEPA-style reflective optimization).

Runs BETWEEN missions in a separate workflow. It reads failure traces and proposes an improved
system prompt for a role — as a candidate only. Promotion is gated downstream by the deterministic
verifier + an independent Agent-as-Judge on a held-out eval set + replay of recorded missions; the
evolver NEVER hot-patches a running agent. This keeps self-improvement safe and reversible.
"""

from __future__ import annotations

from dataclasses import dataclass

from lha.contracts.model import ModelMessage, ModelProvider


@dataclass
class PromptCandidate:
    role: str
    prompt: str
    rationale: str


class PromptEvolver:
    """Proposes an improved prompt for a role from its failure traces (offline)."""

    def __init__(self, model: ModelProvider) -> None:
        self._model = model

    async def propose(
        self, *, role: str, current_prompt: str, failure_traces: list[str]
    ) -> PromptCandidate:
        traces = "\n".join(f"- {t}" for t in failure_traces[-50:]) or "(no traces)"
        result = await self._model.complete(
            [
                ModelMessage(
                    role="system",
                    content=(
                        "You optimize an agent's SYSTEM PROMPT by reflecting on its failures "
                        "(GEPA-style). Output ONLY the improved prompt text — no preamble."
                    ),
                ),
                ModelMessage(
                    role="user",
                    content=(
                        f"Role: {role}\n\nCurrent prompt:\n{current_prompt}\n\n"
                        f"Failure traces:\n{traces}\n\nReturn an improved prompt."
                    ),
                ),
            ]
        )
        candidate = result.text.strip() or current_prompt
        return PromptCandidate(
            role=role,
            prompt=candidate,
            rationale="reflective optimization over failure traces (candidate; eval-gated)",
        )
