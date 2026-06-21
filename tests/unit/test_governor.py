"""Tests for the pre-emptive budget governor, cost ledger, and loop detector."""

from __future__ import annotations

from lha.contracts.model import Usage
from lha.governor.cost import CostLedger
from lha.governor.governor import BudgetGovernor, LoopDetector


def test_cost_ledger_totals() -> None:
    ledger = CostLedger()
    ledger.record(
        cycle_id="c1", usage=Usage(input_tokens=100, output_tokens=50, model="m"), usd=0.5
    )
    ledger.record(
        cycle_id="c2", usage=Usage(input_tokens=200, output_tokens=20, model="m"), usd=1.5
    )
    assert ledger.total_usd == 2.0
    assert ledger.total_input_tokens == 300
    assert ledger.total_output_tokens == 70
    assert ledger.mean_usd_per_cycle() == 1.0


def test_governor_allows_within_budget() -> None:
    ledger = CostLedger()
    ledger.record(cycle_id="c1", usage=Usage(model="m"), usd=1.0)
    gov = BudgetGovernor(ceiling_usd=10.0, max_cycles=100)
    decision = gov.authorize_next(ledger, cycles_done=1, projected_usd=2.0)
    assert decision.allow
    assert decision.projected_usd == 3.0


def test_governor_denies_when_projected_exceeds_ceiling() -> None:
    ledger = CostLedger()
    ledger.record(cycle_id="c1", usage=Usage(model="m"), usd=9.5)
    gov = BudgetGovernor(ceiling_usd=10.0, max_cycles=100)
    decision = gov.authorize_next(ledger, cycles_done=1, projected_usd=1.0)
    assert not decision.allow
    assert "ceiling" in decision.reason


def test_governor_denies_at_max_cycles() -> None:
    gov = BudgetGovernor(ceiling_usd=1_000_000.0, max_cycles=3)
    decision = gov.authorize_next(CostLedger(), cycles_done=3, projected_usd=0.0)
    assert not decision.allow
    assert "max cycles" in decision.reason


def test_loop_detector_trips_after_threshold() -> None:
    detector = LoopDetector(threshold=3)
    assert not detector.observe("01:edit")
    assert not detector.observe("01:edit")
    assert detector.observe("01:edit")  # third occurrence → looping
    detector.reset("01:edit")
    assert not detector.observe("01:edit")
