"""Export the language-neutral conformance cases in ``spec/`` from the Python implementation.

The Python implementation is the reference: this script records what it does for a corpus of
inputs, and both implementations' test suites then assert the recorded behaviour. Run it only when
a behaviour change is intended, and review the diff:

    cd python && uv run python scripts/export_spec.py
"""

from __future__ import annotations

import hashlib
import importlib
import json
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
SPEC = ROOT / "spec"
sys.path.insert(0, str(ROOT / "python"))  # so ``tests.unit.*`` corpora import

from lha.contracts.model import Usage  # noqa: E402
from lha.contracts.state import Checklist, ChecklistItem  # noqa: E402
from lha.contracts.verify import checks_from_commands, derive_check_name  # noqa: E402
from lha.coordination.decision_log import _canonical, _chain_hash  # noqa: E402
from lha.coordination.ownership import is_shared  # noqa: E402
from lha.model.pricing import CLAUDE_PRICES, lookup_claude_price  # noqa: E402
from lha.obs.redact import is_secret_key, redact_text  # noqa: E402
from lha.safety.commands import classify_command  # noqa: E402
from lha.safety.egress import (  # noqa: E402
    EgressDenied,
    EgressPolicy,
    is_public_address,
    normalize_host,
    parse_url,
)
from lha.verify.harness_integrity import _is_harness_file  # noqa: E402

GENESIS = "0" * 64


def _write(rel: str, data: object) -> None:
    path = SPEC / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {path.relative_to(ROOT)}")


def _param_values(module: str, func: str) -> list[Any]:
    """The first parametrize argument's values of ``module.func`` (pytest marks)."""
    fn = getattr(importlib.import_module(module), func)
    values: list[Any] = []
    for mark in getattr(fn, "pytestmark", []):
        if mark.name == "parametrize":
            for value in mark.args[1]:
                values.append(value[0] if isinstance(value, tuple) else value)
    return values


# --- safety/classify_command ------------------------------------------------------------------


def _command_corpus() -> list[list[str]]:
    corpus: list[list[str]] = []
    corpus += _param_values("tests.unit.test_safety_commands", "test_gated_commands")
    corpus += _param_values("tests.unit.test_safety_commands", "test_benign_commands")
    paths = importlib.import_module("tests.unit.test_safety_command_paths")
    corpus += [argv for argv, _ in paths.CASES]
    bypasses = importlib.import_module("tests.unit.test_safety_bypasses")
    corpus += bypasses.GATED + bypasses.ALLOWED
    for func in (
        "test_launcher_option_values_do_not_hide_the_command",
        "test_http_upload_spellings_are_gated",
        "test_sed_in_place_on_protected_paths_is_gated",
    ):
        corpus += _param_values("tests.unit.test_review_fixes_safety", func)
    corpus += [["sh", "-c", s] for s in ("ls\ngit push origin main", "echo `git push`", "ls\npwd")]
    unique: list[list[str]] = []
    for argv in corpus:
        if list(argv) not in unique:
            unique.append(list(argv))
    return unique


def export_classifier() -> None:
    cases = [{"argv": argv, "reason": classify_command(argv)} for argv in _command_corpus()]
    _write("safety/classify_command.json", {"cases": cases})


# --- safety/egress ----------------------------------------------------------------------------

_HOSTS = [
    "Example.COM",
    "example.com.",
    "[::1]",
    "bücher.example",
    "straße.de",
    "xn--bcher-kva.example",
    "  spaced.example  ",
    "127.0.0.1",
    "",
]
_URLS = [
    "https://docs.example.com/x",
    "http://docs.example.com:80/x",
    "https://DOCS.EXAMPLE.COM./x",
    "https://docs.example.com:8443/x",
    "ftp://docs.example.com/x",
    "https://user:pw@docs.example.com/x",
    "https://straße.de/x",
    "https://bücher.example/x",
    "https:///nohost",
    "https://docs.example.com:99999/x",
    "http://[::1]:8080/x",
    "not a url",
]
_ADDRESSES = [
    "127.0.0.1",
    "10.1.2.3",
    "172.16.0.1",
    "192.168.1.1",
    "169.254.169.254",
    "::1",
    "::ffff:127.0.0.1",
    "0.0.0.0",
    "100.64.0.1",
    "fc00::1",
    "fe80::1",
    "fec0::1",
    "93.184.216.34",
    "8.8.8.8",
    "2606:4700:4700::1111",
    "not-an-ip",
]


def export_egress() -> None:
    parse_cases = []
    for url in _URLS:
        try:
            parsed = parse_url(url)
            parse_cases.append({"url": url, "ok": True, **parsed.model_dump()})
        except EgressDenied:
            parse_cases.append({"url": url, "ok": False})
    policy = EgressPolicy(allow_hosts={"docs.example.com", "strasse.de", "bücher.example"})
    permit_cases = [
        {"allow_hosts": sorted(policy.allow_hosts), "url": url, "permitted": policy.permits(url)}
        for url in _URLS
    ]
    _write(
        "safety/egress.json",
        {
            "normalize_host": [{"host": h, "normalized": normalize_host(h)} for h in _HOSTS],
            "parse_url": parse_cases,
            "is_public_address": [
                {"address": a, "public": is_public_address(a)} for a in _ADDRESSES
            ],
            "permits": permit_cases,
        },
    )


# --- obs/redact -------------------------------------------------------------------------------

_TEXTS = [
    "key sk-ant-abcdefghijklmnop1234 here",
    "token ghp_abcdefghijklmnopqrstuvwxyz0123",
    "github_pat_11ABCDEFGHIJKLMNOPQRST_abcdefghij",
    "slack xoxb-1234567890-abcdefghij",
    "aws AKIAABCDEFGHIJKLMNOP done",
    "google AIzaSyA1234567890abcdefghijklmnopqrstu",
    "Authorization: Bearer abc.def.ghi",
    "authorization=Basic dXNlcjpwYXNz",
    "curl -H 'Authorization: Token t0ps3cret'",
    "bearer xyz123==",
    "postgresql://lha:secretpw@db:5432/lha",
    "redis://:hunter2secret@localhost:6379/0",
    "postgresql://u:p@ss@host/db",
    "https://example.com/path?q=1",
    "plain text with no secrets",
    "x_sk-abcdefghijklmnopqrst",
]
_KEYS = [
    "api_key",
    "ANTHROPIC_API_KEY",
    "password",
    "db_password",
    "secret",
    "client_secret",
    "token",
    "access_token",
    "accessToken",
    "refreshToken",
    "sessionToken",
    "auth",
    "authorization",
    "dsn",
    "postgres_dsn",
    "tokens_used",
    "input_tokens",
    "author",
    "name",
    "private_key",
]


def export_redact() -> None:
    _write(
        "obs/redact.json",
        {
            "redact_text": [{"text": t, "redacted": redact_text(t)} for t in _TEXTS],
            "is_secret_key": [{"key": k, "secret": is_secret_key(k)} for k in _KEYS],
        },
    )


# --- contracts/verify check names ------------------------------------------------------------

_COMMANDS = [
    ["uv", "run", "pytest", "-q"],
    ["uv", "run", "--frozen", "ruff", "check", "."],
    ["python3", "-m", "ty", "check"],
    ["/usr/bin/python3.12", "-c", "print(1)"],
    ["npx", "tsc"],
    ["npm", "test"],
    ["make", "check"],
    ["./scripts/verify.sh"],
    ["-v"],
    ["python", "-m", "pytest", "tests/unit"],
    ["cargo", "test"],
    ["go", "test", "./..."],
    ["uv", "run", "weird name!!", "x"],
]


def export_check_names() -> None:
    _write(
        "contracts/check_names.json",
        {
            "derive_check_name": [{"command": c, "name": derive_check_name(c)} for c in _COMMANDS],
            "checks_from_commands": [
                {
                    "commands": group,
                    "names": [c.name for c in checks_from_commands(group)],
                }
                for group in (
                    [
                        ["uv", "run", "pytest", "tests/unit"],
                        ["uv", "run", "pytest", "tests/int"],
                        [],
                    ],
                    [["harness_integrity"], ["pytest"], ["pytest"], ["pytest"]],
                    [
                        ["ruff", "check"],
                        ["uv", "run", "ty", "check"],
                        ["uv", "run", "pytest", "-q"],
                    ],
                )
            ],
        },
    )


# --- state/checklist --------------------------------------------------------------------------


def _item(item_id: str, status: str = "todo", deps: list[str] | None = None) -> ChecklistItem:
    return ChecklistItem(
        id=item_id, description=f"task {item_id}", status=status, depends_on=deps or []
    )


_SCENARIOS: dict[str, list[ChecklistItem]] = {
    "empty": [],
    "fresh": [_item("01"), _item("02"), _item("03")],
    "in_progress_first": [_item("01"), _item("02", "in_progress")],
    "chain_waits": [_item("01", "done"), _item("02", deps=["01"]), _item("03", deps=["02"])],
    "all_done": [_item("01", "done"), _item("02", "done")],
    "blocked_only": [_item("01", "done"), _item("02", "blocked")],
    "blocked_dep": [_item("01", "blocked"), _item("02", deps=["01"]), _item("03")],
    "unknown_dep": [_item("01", deps=["99"])],
    "self_dep": [_item("01", deps=["01"])],
    "cycle": [_item("01", deps=["03"]), _item("02", deps=["01"]), _item("03", deps=["02"])],
    "duplicate": [_item("01"), _item("01")],
}


def export_checklist() -> None:
    scenarios = []
    for name, items in _SCENARIOS.items():
        checklist = Checklist(items=items)
        nxt = checklist.next_actionable()
        scenarios.append(
            {
                "name": name,
                "checklist": json.loads(checklist.model_dump_json()),
                "next_actionable": nxt.id if nxt else None,
                "is_complete": checklist.is_complete,
                "is_deadlocked": checklist.is_deadlocked,
                "deadlock_reason": checklist.deadlock_reason(),
                "dependency_errors": checklist.dependency_errors(),
                "items_done": checklist.items_done,
            }
        )
    # A transition script: start -> fail -> fail -> fail (blocks at 3) -> unblock -> succeed.
    checklist = Checklist(items=[_item("01"), _item("02", deps=["01"])])
    initial = json.loads(checklist.model_dump_json())
    steps: list[dict[str, Any]] = []
    ops: list[tuple[str, dict[str, Any]]] = [
        ("start", {"item_id": "01"}),
        ("record_failure", {"item_id": "01", "reason": "tests failed", "max": 3}),
        ("record_failure", {"item_id": "01", "reason": "tests failed again", "max": 3}),
        ("record_failure", {"item_id": "01", "reason": "still failing", "max": 3}),
        ("unblock", {"item_id": "01"}),
        ("record_success", {"item_id": "01", "verified_by": ["pytest", "ruff"]}),
        ("start", {"item_id": "02"}),
        ("record_success", {"item_id": "02", "verified_by": ["pytest"]}),
    ]
    for op, args in ops:
        if op == "start":
            checklist.start(args["item_id"])
        elif op == "record_failure":
            checklist.record_failure(
                args["item_id"], args["reason"], max_consecutive_failures=args["max"]
            )
        elif op == "unblock":
            checklist.unblock(args["item_id"])
        else:
            checklist.record_success(args["item_id"], args["verified_by"])
        steps.append({"op": op, "args": args, "after": json.loads(checklist.model_dump_json())})
    _write(
        "state/checklist.json",
        {"scenarios": scenarios, "transitions": {"initial": initial, "steps": steps}},
    )


# --- coordination/decision chain --------------------------------------------------------------


def export_decision_chain() -> None:
    records: list[dict[str, Any]] = [
        {"decision": "Use SQLite for the cache", "rationale": "single node", "affected": ["db.py"]},
        {"decision": 'Ünïcödé ✓ <tag> & "quotes"', "rationale": "line\nbreak\u2028sep"},
        {"decision": "numbers", "rationale": "types", "n": 3, "f": 1.5, "z": 0.0, "b": True},
        {"nested": {"b": [1, 2, {"a": None}], "a": "x"}, "decision": "d", "rationale": "r"},
    ]
    prev = GENESIS
    chain = []
    for record in records:
        digest = _chain_hash(prev, record)
        chain.append(
            {"record": record, "canonical": _canonical(record), "prev": prev, "hash": digest}
        )
        prev = digest
    assert (
        hashlib.sha256(f"{GENESIS}\n{chain[0]['canonical']}".encode()).hexdigest()
        == chain[0]["hash"]
    )
    _write("coordination/decision_chain.json", {"genesis": GENESIS, "chain": chain})


# --- coordination/ownership shared files ------------------------------------------------------

_SHARED_PATHS = [
    "pyproject.toml",
    "uv.lock",
    "package.json",
    "go.mod",
    "services/api/pyproject.toml",
    "./uv.lock",
    "app/db/migrations/0001_init.py",
    "src/pkg/a.py",
    "README.md",
    ".github/workflows/ci.yml",
    "Dockerfile",
    "src/__init__.py",
]


def export_ownership() -> None:
    _write(
        "coordination/shared_paths.json",
        {"cases": [{"path": p, "shared": is_shared(p)} for p in _SHARED_PATHS]},
    )


# --- verify/harness files ---------------------------------------------------------------------

_HARNESS_PATHS = [
    "tests/test_a.py",
    "test/helpers.py",
    "src/pkg/tests/test_a.py",
    "test_app.py",
    "pkg/test_models.py",
    "pkg/models_test.py",
    "conftest.py",
    "src/conftest.py",
    "pyproject.toml",
    "setup.cfg",
    ".coveragerc",
    "tox.ini",
    "sub/pytest.ini",
    "noxfile.py",
    "src/app.py",
    "src/contest.py",
    "docs/tests.md",
    "latest_results.py",
]


def export_harness_files() -> None:
    _write(
        "verify/harness_files.json",
        {"cases": [{"path": p, "harness": _is_harness_file(p)} for p in _HARNESS_PATHS]},
    )


# --- model/pricing ----------------------------------------------------------------------------


def export_pricing() -> None:
    usages = [
        Usage(input_tokens=1_000_000),
        Usage(output_tokens=1_000_000),
        Usage(input_tokens=1234, output_tokens=567, cache_read_input_tokens=10_000),
        Usage(cache_creation_input_tokens=100_000, cache_creation_1h_input_tokens=40_000),
        Usage(cache_creation_input_tokens=10, cache_creation_1h_input_tokens=50),
    ]
    models = [*CLAUDE_PRICES, "claude-haiku-4-5-20251001", "claude-imaginary-9", "gpt-4o"]
    cases = []
    for model in models:
        price = lookup_claude_price(model)
        cases.append(
            {
                "model": model,
                "priced": price is not None,
                "costs": [{"usage": u.model_dump(), "usd": price.cost(u)} for u in usages]
                if price
                else [],
            }
        )
    _write("model/pricing.json", {"claude": cases})


def main() -> None:
    export_classifier()
    export_egress()
    export_redact()
    export_check_names()
    export_checklist()
    export_decision_chain()
    export_ownership()
    export_harness_files()
    export_pricing()


if __name__ == "__main__":
    main()
