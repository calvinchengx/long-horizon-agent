"""Pre-emptive budget governor + loop detector."""

from __future__ import annotations

from collections import Counter

from pydantic import BaseModel

from lha.governor.cost import CostLedger


class GovernorDecision(BaseModel):
    """Whether the next step may proceed, and why."""

    allow: bool
    reason: str
    projected_usd: float
    spent_usd: float
    ceiling_usd: float


class BudgetGovernor:
    """Refuses the next step *before* it runs if it would breach the spend/iteration ceiling.

    Two checks: ``authorize_next`` (per cycle, projected from the most expensive cycle so far) and
    ``authorize_call`` (per model call — the hard stop: spent + in-flight reservations + this
    call's worst-case cost must stay within the ceiling). Unknown spend (an unpriced model) is
    treated conservatively: denied unless ``allow_unknown_cost`` was explicitly set.
    """

    def __init__(
        self, *, ceiling_usd: float, max_cycles: int, allow_unknown_cost: bool = False
    ) -> None:
        self._ceiling = ceiling_usd
        self._max_cycles = max_cycles
        self._allow_unknown_cost = allow_unknown_cost

    @property
    def ceiling_usd(self) -> float:
        return self._ceiling

    def authorize_next(
        self, ledger: CostLedger, *, cycles_done: int, projected_usd: float | None = None
    ) -> GovernorDecision:
        spent = ledger.total_usd
        # Estimate the next step's cost from history if not supplied: the most expensive cycle
        # so far (a mean under-projects a spiky run).
        estimate = (
            projected_usd
            if projected_usd is not None
            else max(ledger.mean_usd_per_cycle(), ledger.max_usd_per_cycle())
        )
        projected_total = spent + estimate

        if cycles_done >= self._max_cycles:
            return self._deny("max cycles reached", projected_total, spent)
        if ledger.unknown_cost_entries and not self._allow_unknown_cost:
            return self._deny(self._unknown_reason(ledger), projected_total, spent)
        if projected_total > self._ceiling:
            return self._deny("projected spend exceeds ceiling", projected_total, spent)
        return self._allow(projected_total, spent)

    def authorize_call(
        self, ledger: CostLedger, *, worst_case_usd: float | None, reserved_usd: float = 0.0
    ) -> GovernorDecision:
        """Hard per-call stop. ``worst_case_usd=None`` means the call cannot be priced."""
        spent = ledger.total_usd
        committed = spent + reserved_usd
        if ledger.unknown_cost_entries and not self._allow_unknown_cost:
            return self._deny(self._unknown_reason(ledger), committed, spent)
        if worst_case_usd is None:
            if self._allow_unknown_cost:
                return self._allow(committed, spent)
            return self._deny(
                "cannot price this model call (no configured price); refusing to spend an "
                "unknown amount",
                committed,
                spent,
            )
        projected = committed + worst_case_usd
        if projected > self._ceiling:
            return self._deny("worst-case cost of this call exceeds the budget", projected, spent)
        return self._allow(projected, spent)

    @staticmethod
    def _unknown_reason(ledger: CostLedger) -> str:
        return (
            f"spend is unverifiable: {ledger.unknown_cost_entries} call(s) with unknown price "
            "(configure prices or explicitly allow unknown cost)"
        )

    def _allow(self, projected: float, spent: float) -> GovernorDecision:
        return GovernorDecision(
            allow=True,
            reason="within budget",
            projected_usd=projected,
            spent_usd=spent,
            ceiling_usd=self._ceiling,
        )

    def _deny(self, reason: str, projected: float, spent: float) -> GovernorDecision:
        return GovernorDecision(
            allow=False,
            reason=reason,
            projected_usd=projected,
            spent_usd=spent,
            ceiling_usd=self._ceiling,
        )


class LoopDetector:
    """Flags a signature that FAILS ``threshold`` times IN A ROW (e.g. the same item not advancing).

    Counts are consecutive per signature: observing a signature with ``failed=False`` (progress)
    resets its count, matching ``Settings.stall_limit`` ("consecutive failed attempts").
    """

    def __init__(self, *, threshold: int = 3) -> None:
        self._threshold = threshold
        self._counts: Counter[str] = Counter()

    def observe(self, signature: str, *, failed: bool = True) -> bool:
        """Record an attempt for ``signature``; return True once it has failed ``threshold`` times
        consecutively. A successful attempt (``failed=False``) resets the streak and returns False.
        """
        if not failed:
            self._counts.pop(signature, None)
            return False
        self._counts[signature] += 1
        return self._counts[signature] >= self._threshold

    def reset(self, signature: str | None = None) -> None:
        if signature is None:
            self._counts.clear()
        else:
            self._counts.pop(signature, None)
