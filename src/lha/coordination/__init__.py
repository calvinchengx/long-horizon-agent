"""Coordination: how agents hand off work and avoid stepping on each other.

Agents coordinate through typed artifacts, not chatter (free-form inter-agent chat is the top
multi-agent failure mode). Work flows down as a ``TaskContract`` (a ``Ticket``); writes are kept
conflict-free by a ``FileOwnershipMap`` enforced at the git layer (declare-then-enforce).
"""

from lha.coordination.blackboard import Blackboard, BoardEntry
from lha.coordination.decision_log import DecisionLog
from lha.coordination.ownership import (
    LEAD,
    FileOwnershipMap,
    InvalidPathError,
    LeaseRequest,
    OwnershipViolation,
)
from lha.coordination.ticket import (
    ALLOWED_TRANSITIONS,
    IllegalTransitionError,
    TaskContract,
    Ticket,
    TicketStatus,
)

__all__ = [
    "ALLOWED_TRANSITIONS",
    "LEAD",
    "Blackboard",
    "BoardEntry",
    "DecisionLog",
    "FileOwnershipMap",
    "IllegalTransitionError",
    "InvalidPathError",
    "LeaseRequest",
    "OwnershipViolation",
    "TaskContract",
    "Ticket",
    "TicketStatus",
]
