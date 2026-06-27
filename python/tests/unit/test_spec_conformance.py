"""The Python implementation against the language-neutral cases in ``spec/`` (Go runs the same)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from lha.contracts.model import Usage
from lha.contracts.state import Checklist
from lha.contracts.verify import checks_from_commands, derive_check_name
from lha.coordination.decision_log import _canonical, _chain_hash
from lha.coordination.ownership import is_shared
from lha.model.pricing import lookup_claude_price
from lha.obs.redact import is_secret_key, redact_text
from lha.safety.commands import classify_command
from lha.safety.egress import (
    EgressDenied,
    EgressPolicy,
    is_public_address,
    normalize_host,
    parse_url,
)
from lha.verify.harness_integrity import _is_harness_file

SPEC = Path(__file__).resolve().parents[3] / "spec"


def _load(rel: str) -> Any:
    return json.loads((SPEC / rel).read_text(encoding="utf-8"))


def test_classify_command() -> None:
    for case in _load("safety/classify_command.json")["cases"]:
        assert classify_command(case["argv"]) == case["reason"], case["argv"]


def test_egress() -> None:
    spec = _load("safety/egress.json")
    for case in spec["normalize_host"]:
        assert normalize_host(case["host"]) == case["normalized"], case
    for case in spec["parse_url"]:
        if case["ok"]:
            parsed = parse_url(case["url"])
            assert (parsed.scheme, parsed.host, parsed.port) == (
                case["scheme"],
                case["host"],
                case["port"],
            )
        else:
            with pytest.raises(EgressDenied):
                parse_url(case["url"])
    for case in spec["is_public_address"]:
        assert is_public_address(case["address"]) == case["public"], case
    for case in spec["permits"]:
        policy = EgressPolicy(allow_hosts=set(case["allow_hosts"]))
        assert policy.permits(case["url"]) == case["permitted"], case


def test_redact() -> None:
    spec = _load("obs/redact.json")
    for case in spec["redact_text"]:
        assert redact_text(case["text"]) == case["redacted"], case
    for case in spec["is_secret_key"]:
        assert is_secret_key(case["key"]) == case["secret"], case


def test_check_names() -> None:
    spec = _load("contracts/check_names.json")
    for case in spec["derive_check_name"]:
        assert derive_check_name(case["command"]) == case["name"], case
    for case in spec["checks_from_commands"]:
        assert [c.name for c in checks_from_commands(case["commands"])] == case["names"]


def test_checklist() -> None:
    spec = _load("state/checklist.json")
    for case in spec["scenarios"]:
        checklist = Checklist.model_validate(case["checklist"])
        nxt = checklist.next_actionable()
        assert (nxt.id if nxt else None) == case["next_actionable"], case["name"]
        assert checklist.is_complete == case["is_complete"], case["name"]
        assert checklist.is_deadlocked == case["is_deadlocked"], case["name"]
        assert checklist.deadlock_reason() == case["deadlock_reason"], case["name"]
        assert checklist.dependency_errors() == case["dependency_errors"], case["name"]
        assert checklist.items_done == case["items_done"], case["name"]
    checklist = Checklist.model_validate(spec["transitions"]["initial"])
    for step in spec["transitions"]["steps"]:
        args = step["args"]
        if step["op"] == "start":
            checklist.start(args["item_id"])
        elif step["op"] == "record_failure":
            checklist.record_failure(
                args["item_id"], args["reason"], max_consecutive_failures=args["max"]
            )
        elif step["op"] == "unblock":
            checklist.unblock(args["item_id"])
        else:
            checklist.record_success(args["item_id"], args["verified_by"])
        assert json.loads(checklist.model_dump_json()) == step["after"], step["op"]


def test_decision_chain() -> None:
    spec = _load("coordination/decision_chain.json")
    prev = spec["genesis"]
    for link in spec["chain"]:
        assert _canonical(link["record"]) == link["canonical"]
        assert link["prev"] == prev
        assert _chain_hash(prev, link["record"]) == link["hash"]
        prev = link["hash"]


def test_shared_paths_and_harness_files() -> None:
    for case in _load("coordination/shared_paths.json")["cases"]:
        assert is_shared(case["path"]) == case["shared"], case
    for case in _load("verify/harness_files.json")["cases"]:
        assert _is_harness_file(case["path"]) == case["harness"], case


def test_pricing() -> None:
    for case in _load("model/pricing.json")["claude"]:
        price = lookup_claude_price(case["model"])
        assert (price is not None) == case["priced"], case["model"]
        for cost in case["costs"]:
            assert price is not None
            assert price.cost(Usage(**cost["usage"])) == pytest.approx(cost["usd"], rel=1e-12)
