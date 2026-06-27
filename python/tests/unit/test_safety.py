"""Tests for the safety plane: Rule-of-Two + egress policy + credential broker."""

from __future__ import annotations

import pytest

from lha.safety.egress import CredentialBroker, EgressPolicy
from lha.safety.rule_of_two import (
    Capability,
    RuleOfTwoViolation,
    check_rule_of_two,
    permits,
)


def test_rule_of_two_allows_two() -> None:
    caps = {Capability.UNTRUSTED_CONTENT, Capability.EXTERNAL_COMMS}
    assert permits(caps)
    check_rule_of_two(caps)  # does not raise


def test_rule_of_two_blocks_trifecta() -> None:
    caps = {Capability.UNTRUSTED_CONTENT, Capability.PRIVATE_DATA, Capability.EXTERNAL_COMMS}
    assert not permits(caps)
    with pytest.raises(RuleOfTwoViolation):
        check_rule_of_two(caps)


def test_egress_policy_is_default_deny() -> None:
    policy = EgressPolicy()
    assert not policy.permits("https://example.com/x")
    policy = EgressPolicy(allow_hosts={"api.tavily.com"})
    assert policy.permits("https://api.tavily.com/search")
    assert not policy.permits("https://evil.test/exfiltrate")


def test_credential_broker_swaps_placeholders() -> None:
    broker = CredentialBroker()
    broker.register("{{TAVILY_KEY}}", "real-secret-123", hosts={"api.tavily.com"})
    headers = broker.resolve_headers(
        {"Authorization": "Bearer {{TAVILY_KEY}}"}, host="API.Tavily.com."
    )
    assert headers["Authorization"] == "Bearer real-secret-123"
    # An untouched value is unchanged.
    assert broker.resolve("no placeholder here", host="api.tavily.com") == "no placeholder here"


def test_credential_broker_only_injects_for_bound_host() -> None:
    broker = CredentialBroker()
    broker.register("{{TAVILY_KEY}}", "real-secret-123", hosts={"api.tavily.com"})
    leaked = broker.resolve_headers({"Authorization": "Bearer {{TAVILY_KEY}}"}, host="evil.test")
    assert leaked["Authorization"] == "Bearer {{TAVILY_KEY}}"
    with pytest.raises(ValueError):
        broker.register("{{X}}", "secret", hosts=[])
