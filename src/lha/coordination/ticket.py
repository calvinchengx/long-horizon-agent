"""Typed task contracts + ticket lifecycle.

A ``TaskContract`` is the structured delegation handed to a sub-agent — objective, output shape,
explicit boundaries, the files it owns, budgets, and acceptance criteria. Vague delegation is the
#1 orchestrator failure mode, so this is deliberately typed (and becomes a Temporal child-workflow
input, making it journaled + replayable).
"""

from __future__ import annotations

from enum import Enum

from pydantic import BaseModel, Field


class TicketStatus(str, Enum):
    CREATED = "created"
    IN_PROGRESS = "in_progress"
    AWAITING_VERIFY = "awaiting_verify"
    AWAITING_MERGE = "awaiting_merge"
    DONE = "done"
    FAILED = "failed"


#: The ticket lifecycle. Anything not listed is illegal. ``DONE`` and ``FAILED`` are terminal (a
#: failed piece of work is retried as a NEW ticket, so history is never rewritten). Verification
#: or merge can bounce a ticket back to ``IN_PROGRESS`` for another attempt.
ALLOWED_TRANSITIONS: dict[TicketStatus, frozenset[TicketStatus]] = {
    TicketStatus.CREATED: frozenset({TicketStatus.IN_PROGRESS, TicketStatus.FAILED}),
    TicketStatus.IN_PROGRESS: frozenset({TicketStatus.AWAITING_VERIFY, TicketStatus.FAILED}),
    TicketStatus.AWAITING_VERIFY: frozenset(
        {TicketStatus.AWAITING_MERGE, TicketStatus.IN_PROGRESS, TicketStatus.FAILED}
    ),
    TicketStatus.AWAITING_MERGE: frozenset(
        {TicketStatus.DONE, TicketStatus.IN_PROGRESS, TicketStatus.FAILED}
    ),
    TicketStatus.DONE: frozenset(),
    TicketStatus.FAILED: frozenset(),
}


class IllegalTransitionError(ValueError):
    """Raised when a ticket is asked to move along an edge not in ``ALLOWED_TRANSITIONS``."""


class TaskContract(BaseModel):
    """The typed work order for a sub-agent."""

    objective: str
    role: str = "researcher"  # researcher | implementer | reviewer | tester | integrator
    output_schema: str = ""  # description of the expected output shape
    boundaries: list[str] = Field(default_factory=list)  # explicit do-nots
    write_set: list[str] = Field(default_factory=list)  # files this writer owns (implementers)
    token_budget: int = 0  # 0 = inherit default
    tool_budget: int = 0
    acceptance: list[str] = Field(default_factory=list)  # check names that must pass


class Ticket(BaseModel):
    """A unit of delegated work with a durable lifecycle."""

    id: str
    contract: TaskContract
    item_id: str | None = None  # the checklist item this advances, if any
    status: TicketStatus = TicketStatus.CREATED
    branch: str | None = None
    result_summary: str = ""
    attempts: int = 0

    def can_transition(self, to: TicketStatus) -> bool:
        return to in ALLOWED_TRANSITIONS[self.status]

    def transition(self, to: TicketStatus) -> Ticket:
        """Return a copy advanced to ``to`` (transitions are durable events upstream).

        Raises ``IllegalTransitionError`` for any edge not in ``ALLOWED_TRANSITIONS`` (e.g.
        ``CREATED -> DONE`` skipping verification, or reopening a terminal ``DONE`` ticket).
        Entering ``IN_PROGRESS`` counts as a new attempt.
        """
        if not self.can_transition(to):
            raise IllegalTransitionError(
                f"ticket {self.id!r}: illegal transition {self.status.value} -> {to.value}"
            )
        update: dict[str, object] = {"status": to}
        if to is TicketStatus.IN_PROGRESS:
            update["attempts"] = self.attempts + 1
        return self.model_copy(update=update)
