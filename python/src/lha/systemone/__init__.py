"""System One decision models (TypeSafe's Jev, or a self-hosted Kev): typed, calibrated answers.

See ``lha.contracts.system_one`` for the contract and docs/25-system-one.md for how LHA uses them.
"""

from lha.systemone.build import build_stall_triage, build_system_one, close_system_one
from lha.systemone.client import SystemOneClient
from lha.systemone.stub import StubSystemOne
from lha.systemone.triage import StallTriage, TriageVerdict

__all__ = [
    "StallTriage",
    "StubSystemOne",
    "SystemOneClient",
    "TriageVerdict",
    "build_stall_triage",
    "build_system_one",
    "close_system_one",
]
