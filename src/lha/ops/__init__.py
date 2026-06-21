"""Operations: dependency-degradation / safe-park decisions and mission lifecycle."""

from lha.ops.degradation import (
    DependencyStatus,
    Health,
    SafeParkDecision,
    decide_safe_park,
)
from lha.ops.lifecycle import MissionOutcome, should_declare_impossible

__all__ = [
    "DependencyStatus",
    "Health",
    "MissionOutcome",
    "SafeParkDecision",
    "decide_safe_park",
    "should_declare_impossible",
]
