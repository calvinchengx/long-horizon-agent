"""Tests for ops (degradation/safe-park, lifecycle) and verifier-trust coverage parsing."""

from __future__ import annotations

from lha.ops.degradation import DependencyStatus, Health, decide_safe_park
from lha.ops.lifecycle import should_declare_impossible
from lha.verify.trust_bootstrap import parse_coverage_pct


def test_safe_park_when_critical_down() -> None:
    decision = decide_safe_park(
        [
            DependencyStatus(name="git", health=Health.DOWN),
            DependencyStatus(name="pgvector", health=Health.DEGRADED),
        ]
    )
    assert decision.park
    assert "git" in decision.reason
    assert decision.degraded == ["pgvector"]


def test_no_park_when_only_optional_degraded() -> None:
    decision = decide_safe_park(
        [
            DependencyStatus(name="model", health=Health.OK),
            DependencyStatus(name="langfuse", health=Health.DOWN),
        ]
    )
    assert not decision.park
    assert decision.degraded == ["langfuse"]


def test_declare_impossible_threshold() -> None:
    assert not should_declare_impossible(consecutive_failures=2, threshold=3)
    assert should_declare_impossible(consecutive_failures=3, threshold=3)


def test_parse_coverage_pct() -> None:
    text = "Name      Stmts   Miss  Cover\nfoo.py       10      2    80%\nTOTAL        10      2    80%\n"
    assert parse_coverage_pct(text) == 0.8
    assert parse_coverage_pct("no coverage line") == 0.0
