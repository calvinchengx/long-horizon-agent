"""Flaky-test quarantine.

A flaky check shouldn't gate progress (or it makes 'green' unreliable). Quarantined checks still
run — for visibility / flake-rate tracking — but as NON-gating, so they don't block a verified item.

Quarantine requires EVIDENCE: a check can only be marked flaky after it has been observed both
passing and failing on the SAME revision (identical code), at least ``min_flips`` times per
outcome. Without that rule, ``mark_flaky("pytest")`` would silently turn the whole suite
advisory. (And even a fully quarantined gate can't mark work done: zero gating checks is
``unverified``.)
"""

from __future__ import annotations

from collections import defaultdict

from lha.contracts.verify import Check, CheckResult


class FlakeEvidenceError(ValueError):
    """Raised when quarantining a check that has no recorded flake history."""


class FlakyQuarantine:
    def __init__(self, *, min_flips: int = 1, min_runs: int = 3) -> None:
        # Per revision, a check must have passed >= min_flips and failed >= min_flips times,
        # across >= min_runs total runs, to count as flaky.
        self._min_flips = max(1, min_flips)
        self._min_runs = max(2, min_runs)
        self._history: dict[str, dict[str, list[int]]] = defaultdict(dict)
        self._flaky: set[str] = set()

    def record(self, name: str, *, revision: str, passed: bool) -> None:
        """Record one observed outcome of check ``name`` on code ``revision`` (e.g. a git sha)."""
        counts = self._history[name].setdefault(revision, [0, 0])
        counts[0 if passed else 1] += 1

    def record_results(self, results: list[CheckResult], *, revision: str) -> None:
        for result in results:
            self.record(result.name, revision=revision, passed=result.passed)

    def has_flake_evidence(self, name: str) -> bool:
        for passes, fails in self._history.get(name, {}).values():
            if (
                passes >= self._min_flips
                and fails >= self._min_flips
                and passes + fails >= self._min_runs
            ):
                return True
        return False

    def mark_flaky(self, name: str) -> None:
        """Quarantine ``name``; raises ``FlakeEvidenceError`` without recorded flake history."""
        if not self.has_flake_evidence(name):
            raise FlakeEvidenceError(
                f"refusing to quarantine {name!r}: no recorded pass+fail on the same revision "
                f"(need >= {self._min_flips} of each over >= {self._min_runs} runs)"
            )
        self._flaky.add(name)

    def is_flaky(self, name: str) -> bool:
        return name in self._flaky

    def partition(self, checks: list[Check]) -> tuple[list[Check], list[Check]]:
        """Split into (gating, quarantined); quarantined checks are forced non-gating."""
        gating: list[Check] = []
        quarantined: list[Check] = []
        for check in checks:
            if check.name in self._flaky:
                quarantined.append(check.model_copy(update={"gating": False}))
            else:
                gating.append(check)
        return gating, quarantined
