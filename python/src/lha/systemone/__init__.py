"""System One decision models (TypeSafe's Jev, or a self-hosted Kev): typed, calibrated answers.

See ``lha.contracts.system_one`` for the contract and docs/25-system-one.md for how LHA uses them.
"""

from lha.systemone.build import (
    build_command_second_opinion,
    build_review_screen,
    build_stall_triage,
    build_system_one,
    close_system_one,
)
from lha.systemone.client import SystemOneClient
from lha.systemone.screen import CommandSecondOpinion, CommandVerdict, ReviewScreen, ScreenVerdict
from lha.systemone.stub import StubSystemOne
from lha.systemone.triage import StallTriage, TriageVerdict

__all__ = [
    "CommandSecondOpinion",
    "CommandVerdict",
    "ReviewScreen",
    "ScreenVerdict",
    "StallTriage",
    "StubSystemOne",
    "SystemOneClient",
    "TriageVerdict",
    "build_command_second_opinion",
    "build_review_screen",
    "build_stall_triage",
    "build_system_one",
    "close_system_one",
]
