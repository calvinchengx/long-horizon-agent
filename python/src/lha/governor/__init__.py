"""The governor: pre-emptive cost control + loop/oscillation detection.

A buggy or hijacked agent over a multi-week run can burn an unbounded bill or spin forever. The
governor refuses the NEXT step *before* it runs if projected spend/iterations would breach the
ceiling (alerting-after-the-fact is too late), and detects repeated (state, action) signatures so
an oscillating agent is stopped and re-planned rather than thrashing. ``CostMeter``/``MeteredModel``
wrap every model provider so each call is budget-checked before it runs and recorded after.
"""

from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor, GovernorDecision, LoopDetector
from lha.governor.metering import BudgetExceeded, CostMeter, MeteredModel

__all__ = [
    "BudgetExceeded",
    "BudgetGovernor",
    "CostLedger",
    "CostMeter",
    "GovernorDecision",
    "LoopDetector",
    "MeteredModel",
]
