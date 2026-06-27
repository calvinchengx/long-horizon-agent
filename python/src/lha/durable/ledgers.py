"""Task + Progress ledgers (the orchestrator's working state, Magentic-One style).

The Task Ledger holds the facts, the plan, and the live tickets. The Progress Ledger counts rounds
and detects stalls — when too many rounds pass with no progress, the orchestrator forces a re-plan
rather than letting the team spin. Both are plain dataclasses carried as workflow state across
Continue-As-New.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from lha.coordination.ticket import Ticket, TicketStatus

_TERMINAL = {TicketStatus.DONE, TicketStatus.FAILED}


@dataclass
class TaskLedger:
    """Facts, the current plan, and all tickets the orchestrator is tracking."""

    facts: list[str] = field(default_factory=list)
    plan: list[str] = field(default_factory=list)
    tickets: dict[str, Ticket] = field(default_factory=dict)

    def upsert(self, ticket: Ticket) -> None:
        self.tickets[ticket.id] = ticket

    def open_tickets(self) -> list[Ticket]:
        return [t for t in self.tickets.values() if t.status not in _TERMINAL]


@dataclass
class ProgressLedger:
    """Round counter + stall detection."""

    stall_limit: int = 5
    rounds: int = 0
    last_progress_round: int = 0

    def record(self, *, made_progress: bool) -> None:
        self.rounds += 1
        if made_progress:
            self.last_progress_round = self.rounds

    @property
    def rounds_since_progress(self) -> int:
        return self.rounds - self.last_progress_round

    @property
    def stalled(self) -> bool:
        return self.rounds_since_progress >= self.stall_limit
