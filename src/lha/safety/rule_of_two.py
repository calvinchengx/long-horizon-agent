"""Meta's 'Rule of Two' for agent safety.

A single agent session must hold AT MOST TWO of the three dangerous capabilities at once:
- ingesting UNTRUSTED content (web pages, issue text, ...),
- access to PRIVATE data (secrets, customer data),
- the ability to perform EXTERNAL communications (egress / side effects).

Holding all three is the 'lethal trifecta' that makes prompt injection catastrophic. This is a
structural guard the orchestrator checks before granting a session its capability set.
"""

from __future__ import annotations

from enum import Enum


class Capability(str, Enum):
    UNTRUSTED_CONTENT = "untrusted_content"
    PRIVATE_DATA = "private_data"
    EXTERNAL_COMMS = "external_comms"


class RuleOfTwoViolation(ValueError):
    """Raised when a session would hold all three dangerous capabilities."""


def permits(capabilities: set[Capability]) -> bool:
    """True if the capability set is safe (holds at most two of the three)."""
    return len(capabilities) < 3


def check_rule_of_two(capabilities: set[Capability]) -> None:
    """Raise ``RuleOfTwoViolation`` if a session would hold the full lethal trifecta."""
    if not permits(capabilities):
        raise RuleOfTwoViolation(
            "session would hold untrusted content + private data + external comms "
            "(the lethal trifecta); split capabilities across sessions"
        )
