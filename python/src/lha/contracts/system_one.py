"""The System One contract: typed questions in, calibrated typed answers out (no generated text).

A System One model (TypeSafe's hosted Jev, or a self-hosted server that speaks the same
``POST /v1/systemone`` API, such as Kev) takes a ``state`` and a map of named questions and returns
one typed answer per question:

- ``Noul``: a yes/no question; the answer is the probability of yes.
- ``Choice``: pick one of up to 255 described options; the answer is the most likely option, a
  probability for every option, and a confidence (how concentrated the distribution is).
- ``Score``: rate against 2-10 ordered levels; the answer is the probability-weighted level, a
  probability per level, and a confidence.

LHA uses these answers only to make itself MORE cautious or cheaper (split or block an item
early, reorder recalled memory), never to allow an action or mark work done: those stay with the
deterministic verifier and command classifier. A failed call changes nothing.
"""

from __future__ import annotations

from typing import Literal, Protocol, runtime_checkable

from pydantic import BaseModel, Field, JsonValue


class Noul(BaseModel):
    """A yes/no question: the answer is the probability that the answer is yes."""

    type: Literal["noul"] = "noul"
    instructions: JsonValue
    # Optional descriptions of what a yes ("true") and a no ("false") mean.
    criteria: dict[str, JsonValue] | None = None


class Choice(BaseModel):
    """Pick one option: ``criteria`` maps each option name to its description (or ``None``)."""

    type: Literal["choice"] = "choice"
    instructions: JsonValue
    criteria: dict[str, JsonValue]


class Score(BaseModel):
    """Rate against ordered levels: ``criteria`` lists the level descriptions, lowest first."""

    type: Literal["score"] = "score"
    instructions: JsonValue
    criteria: list[JsonValue]


Question = Noul | Choice | Score


class NoulAnswer(BaseModel):
    type: Literal["noul"] = "noul"
    noul: float


class ChoiceAnswer(BaseModel):
    type: Literal["choice"] = "choice"
    choice: str
    probabilities: dict[str, float]
    confidence: float


class ScoreAnswer(BaseModel):
    type: Literal["score"] = "score"
    score: float
    probabilities: dict[str, float]  # level index ("0", "1", ...) -> probability
    confidence: float


Answer = NoulAnswer | ChoiceAnswer | ScoreAnswer


class SystemOneResult(BaseModel):
    """One evaluation: the versioned model that answered, one answer per question, token usage."""

    model: str = ""
    answers: dict[str, Answer] = Field(default_factory=dict)
    input_tokens: int = 0
    output_tokens: int = 0


class SystemOneError(RuntimeError):
    """A System One call failed (network, HTTP status, budget or a malformed answer)."""


@runtime_checkable
class SystemOneModel(Protocol):
    """A System One backend. Implementations live in ``lha.systemone``."""

    name: str

    async def evaluate(self, state: JsonValue, questions: dict[str, Question]) -> SystemOneResult:
        """Answer every question against ``state``; raise ``SystemOneError`` on any failure."""
        ...
