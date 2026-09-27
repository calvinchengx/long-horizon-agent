"""Export the language-neutral conformance cases in ``spec/`` from the Python implementation.

The Python implementation is the reference: this script records what it does for a corpus of
inputs, and both implementations' test suites then assert the recorded behaviour. Run it only when
a behaviour change is intended, and review the diff:

    cd python && uv run python scripts/export_spec.py
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib
import json
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
SPEC = ROOT / "spec"
sys.path.insert(0, str(ROOT / "python"))  # so ``tests.unit.*`` corpora import

from lha.agent.loop import parse_action  # noqa: E402
from lha.agent.prompt import (  # noqa: E402
    CODE_MAP_HARD_CAP,
    CODE_MAP_HEADER,
    build_messages,
    corrective_message,
    render_code_map,
    render_memory_block,
    render_tools,
)
from lha.agents.planner import Planner, assign_ownership, parse_plan  # noqa: E402
from lha.agents.reflection import reflect_on_failure  # noqa: E402
from lha.agents.replanner import Replanner  # noqa: E402
from lha.agents.reviewer import Reviewer, ReviewResult, parse_review  # noqa: E402
from lha.agents.roles import ROLES, claude_model_for  # noqa: E402
from lha.agents.subagent import SubAgent  # noqa: E402
from lha.agents.waves import implementer_objective, new_implementer_run  # noqa: E402
from lha.contracts.model import ModelMessage, TurnResult, Usage  # noqa: E402
from lha.contracts.state import (  # noqa: E402
    Checklist,
    ChecklistItem,
    DecisionRecord,
    EventRecord,
    SituationSnapshot,
)
from lha.contracts.tools import ToolSpec  # noqa: E402
from lha.contracts.verify import checks_from_commands, derive_check_name  # noqa: E402
from lha.coordination.decision_log import (  # noqa: E402
    _canonical,
    _chain_hash,
    encode_link,
    verify_chain,
)
from lha.coordination.enforcement import OwnershipGuard  # noqa: E402
from lha.coordination.leases import decide_lease  # noqa: E402
from lha.coordination.ownership import FileOwnershipMap, LeaseRequest, is_shared  # noqa: E402
from lha.coordination.ticket import TaskContract, Ticket, TicketStatus  # noqa: E402
from lha.execution.tools import default_local_tools  # noqa: E402
from lha.execution.tools.decisions import DecisionBuffer, RecordDecisionTool  # noqa: E402
from lha.execution.tools.leases import LEASE_SPEC  # noqa: E402
from lha.execution.tools.web import FetchUrlTool, WebSearchTool  # noqa: E402
from lha.model import parse_fallback_entry  # noqa: E402
from lha.model.pricing import CLAUDE_PRICES, lookup_claude_price  # noqa: E402
from lha.model.stub import StubModel  # noqa: E402
from lha.obs.redact import is_secret_key, redact_text  # noqa: E402
from lha.safety.commands import classify_command  # noqa: E402
from lha.safety.egress import (  # noqa: E402
    EgressDenied,
    EgressPolicy,
    is_public_address,
    normalize_host,
    parse_url,
)
from lha.state.vendor import _target_path  # noqa: E402
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
    "voyage key pa-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-xy done",
    "short pa-abcdefghijklmnopqrstuvwxyz01234 and spa-abcdefghijklmnopqrstuvwxyz0123456789",
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
    # Replanning: a blocked item with a witness and a dependent is split into two children.
    split = Checklist(
        items=[
            _item("01", "done"),
            ChecklistItem(
                id="02",
                description="coarse item",
                status="blocked",
                depends_on=["01"],
                witnesses=["go:TestCoarse"],
            ),
            _item("03", deps=["02"]),
        ]
    )
    split_initial = json.loads(split.model_dump_json())
    split.split(
        "02",
        [
            ChecklistItem(id="x", description="first half"),
            ChecklistItem(id="y", description="second half", witnesses=["cmd:true"]),
        ],
    )
    splits = {
        "initial": split_initial,
        "item_id": "02",
        "drafts": [
            {"description": "first half", "witnesses": []},
            {"description": "second half", "witnesses": ["cmd:true"]},
        ],
        "after": json.loads(split.model_dump_json()),
        "next_actionable": (split.next_actionable() or ChecklistItem(id="", description="")).id,
        "items_total": split.items_total,
        "is_complete": split.is_complete,
    }
    _write(
        "state/checklist.json",
        {
            "scenarios": scenarios,
            "transitions": {"initial": initial, "steps": steps},
            "split": splits,
        },
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
    _write(
        "coordination/decision_chain.json",
        {"genesis": GENESIS, "chain": chain, "log": _decision_log_cases()},
    )


def _decision_log_cases() -> dict[str, Any]:
    """The ``.lha/decisions.ndjson`` line format: a legacy prefix, then chained envelopes.

    ``lines`` is a log exactly as ``GitMissionAnchor`` leaves it after a pre-chain anchor gets its
    first chained records; ``links`` gives, per line, its kind and the running hash after it;
    ``verify`` gives whole logs (each line ``\\n``-terminated) and the expected verdict.
    """
    legacy = [
        DecisionRecord(decision="Use SQLite", rationale="single node", cycle_id="c1"),
        DecisionRecord(decision="Ünï ✓ <&>", rationale="a\u2028b", affected=["x.py"]),
    ]
    chained = [
        DecisionRecord(
            decision="Store dates as UTC",
            rationale="one format",
            alternatives_rejected="epoch ints",
            affected=["models.py", "api.py"],
            cycle_id="c3",
        ),
        DecisionRecord(decision="Pin httpx", rationale="API churn", cycle_id="c4"),
    ]
    lines: list[str] = []
    links: list[dict[str, Any]] = []
    running = GENESIS
    for record in legacy:
        line = record.model_dump_json()
        running = _chain_hash(running, json.loads(line))
        lines.append(line)
        links.append({"kind": "legacy", "running_hash": running})
    for record in chained:
        line, running = encode_link(running, record)
        lines.append(line)
        links.append({"kind": "chained", "running_hash": running})

    def case(name: str, body: list[str]) -> dict[str, Any]:
        check = verify_chain(("".join(f"{ln}\n" for ln in body)).encode())
        return {
            "name": name,
            "lines": body,
            "ok": check.ok,
            "checked": check.checked,
            "legacy": check.legacy,
            "problem": check.problem,
        }

    tampered = json.loads(lines[2])
    tampered["record"]["decision"] = "Store dates as local time"
    return {
        "lines": lines,
        "links": links,
        "last_hash": running,
        "verify": [
            case("legacy prefix + chain", lines),
            case("legacy only (unprotected)", lines[:2]),
            case("chain only", [encode_link(GENESIS, chained[0])[0]]),
            case("edited legacy line", [lines[1], lines[1], *lines[2:]]),
            case("edited chained record", [*lines[:2], json.dumps(tampered), lines[3]]),
            case("deleted chained record", [*lines[:2], lines[3]]),
            case("legacy line after the chain", [*lines, lines[0]]),
        ],
    }


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


def export_flaky_retry() -> None:
    flaky = importlib.import_module("tests.unit.test_flaky_retry_verifier")
    _write(
        "verify/flaky_retry.json",
        {
            "revision": flaky.SPEC_REVISION,
            "cases": [
                {**case, "expected": flaky.run_flaky_scenario(case)}
                for case in flaky.FLAKY_SCENARIOS
            ],
        },
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


# --- model/fallback_models --------------------------------------------------------------------
# LHA_FALLBACK_MODELS entries (``backend:model[@in/out]``) -> the parsed spec, or python's exact
# ValueError message.


def export_fallback_models() -> None:
    entries = [
        "ollama:qwen3:8b",
        " OpenAI_Compat:llama-3.3-70b@0.59/0.79 ",
        "claude:claude-haiku-4-5",
        "CLAUDE:claude-opus-4-8@5/25",
        "openai_compat:a@b@ 1 / 2 ",
        "openai_compat:m@1_0/2e1",
        "openai_compat:m@.5/5.",
        "openai_compat:m@+1/-0",
        "stub: s ",
        "claude_code:sonnet",
        "claude",
        "gpt:4o",
        ":m",
        "claude:",
        "claude: @1/2",
        "openai_compat:m@abc",
        "openai_compat:m@1",
        "openai_compat:m@1/",
        "openai_compat:m@-1/2",
        "openai_compat:m@1/-2",
        "openai_compat:m@0x1/2",
        "openai_compat:m@1__0/2",
        "openai_compat:m@_1/2",
        "openai_compat:@1/2",
        "  ollama : qwen3 ",
        "ollama:'quoted'é",
    ]
    cases: list[dict[str, Any]] = []
    for entry in entries:
        try:
            spec = parse_fallback_entry(entry)
        except ValueError as exc:
            cases.append({"entry": entry, "error": str(exc)})
            continue
        price = spec.price
        cases.append(
            {
                "entry": entry,
                "backend": spec.backend,
                "model": spec.model,
                "price": None
                if price is None
                else {
                    "input_per_mtok": price.input_per_mtok,
                    "output_per_mtok": price.output_per_mtok,
                },
            }
        )
    _write("model/fallback_models.json", {"cases": cases})


# --- state/vendor_paths -----------------------------------------------------------------------
# Where ``lha vendor`` stores a fetched URL (relative to the vendor directory).


def export_vendor_paths() -> None:
    inputs = [
        ("https://docs.example.com/", "text/html"),
        ("https://docs.example.com/api/v1/guide", "text/html"),
        ("https://docs.example.com/api/v1/guide", "application/json"),
        ("https://docs.example.com/spec.json", "application/json"),
        ("https://docs.example.com/a/./b/../c", "text/plain"),
        ("https://docs.example.com/page?x=1&y=2", "text/html"),
        ("https://docs.example.com/page.html?x=1", "text/html"),
        ("https://docs.example.com/we%20ird/na%2Fme/..hidden./x", "text/html"),
        ("https://docs.example.com/über/straße", "text/html"),
        ("https://Docs.Example.COM:8443/x", ""),
        ("https://docs.example.com//double//slash/", "application/xhtml+xml"),
        ("http://127.0.0.1/raw", "text/plain"),
        ("https://xn--bcher-kva.example/buch", "text/html"),
        ("https://docs.example.com/~user/-_.ok", "text/html"),
        ("https://docs.example.com/?", "text/html"),
        ("https://docs.example.com/p?q=%C3%A9", "text/html"),
    ]
    cases = [
        {"url": url, "content_type": ctype, "path": str(_target_path(url, ctype))}
        for url, ctype in inputs
    ]
    _write("state/vendor_paths.json", {"cases": cases})


# --- agent/prompts ----------------------------------------------------------------------------
# The lead's prompts, the JSON reply protocol and the Planner/Replanner prompts: Go must produce
# byte-identical strings for the same inputs. Inputs are recorded as model dumps (tool specs keep
# their ``parameters`` key order, which ``render_tools`` shows as a Python dict repr).

_LONG_UNICODE = "".join(f"ligne {n} é🙂 " for n in range(1500))  # > 8000 code points


def _spec(name: str, description: str, properties: dict[str, object]) -> ToolSpec:
    return ToolSpec(
        name=name, description=description, parameters={"type": "object", "properties": properties}
    )


_SPECS = [
    _spec(
        "write_file",
        "Write a file.",
        {"path": {"type": "string"}, "content": {"type": "string", "description": 'it\'s "new"'}},
    ),
    _spec(
        "run",
        "Run argv.",
        {
            "argv": {"type": "array", "items": {"type": "string"}, "minItems": 1},
            "timeout_s": {"type": "number", "default": 60.0, "maximum": 1e16},
            "check": {"type": "boolean", "default": True, "nullable": None},
        },
    ),
    ToolSpec(name="noop", description="No arguments."),
]


def _decisions() -> list[DecisionRecord]:
    return [
        DecisionRecord(decision="use SQLite", rationale="zero ops", cycle_id="c1"),
        DecisionRecord(
            decision="REST over gRPC",
            rationale="clients",
            alternatives_rejected="gRPC",
            affected=["api/", "docs/api.md"],
        ),
        DecisionRecord(decision="x" * 300, rationale="ü" * 400, cycle_id="c9"),
    ]


def _prompt_cases() -> list[dict[str, Any]]:
    base_item = ChecklistItem(id="01", description="Add the model")
    failing = ChecklistItem(
        id="02.1",
        description="Wire the API",
        witnesses=["go:TestAPI@./internal/...", "cmd:make check"],
        attempts=2,
        last_failure="FAILED ✗ " * 500,
    )
    commits = [f"{n:07x} lha: attempt {n}" for n in range(12)]
    cases: list[dict[str, Any]] = [
        {"name": "mission_only", "mission_text": "Mission: M\n\nBuild it.", "item": base_item},
        {
            "name": "anchor_only",
            "anchor_text": "  Mission: from the progress file  \n",
            "item": base_item,
        },
        {
            "name": "mission_with_long_context",
            "mission_text": "Mission: M",
            "anchor_text": _LONG_UNICODE,
            "item": base_item,
            "commits": commits,
        },
        {
            "name": "context_equal_to_mission",
            "mission_text": "Mission: M",
            "anchor_text": " Mission: M ",
            "item": base_item,
        },
        {"name": "witnesses_and_failure", "mission_text": "Mission: M", "item": failing},
        {
            "name": "memory_and_decisions",
            "mission_text": "Mission: M",
            "item": base_item,
            "memory_text": "\n  " + "mémoire " * 3000 + "  \n",
            "decisions": _decisions(),
        },
        {
            "name": "memory_and_code_map",
            "mission_text": "Mission: M",
            "item": base_item,
            "memory_text": "Memory:\n- a fact",
            "code_map_text": render_code_map("<ctx task='x'>" + "carte " * 4000 + "</ctx>"),
        },
        {"name": "engine", "mission_text": "Mission: M", "item": failing, "engine": True},
        {"name": "no_tools", "mission_text": "Mission: M", "item": base_item, "specs": []},
    ]
    out = []
    for case in cases:
        specs = case.get("specs", _SPECS)
        assert isinstance(specs, list)
        snapshot = SituationSnapshot(
            head_sha="abc",
            recent_commits=case.get("commits", []),
            last_decisions=case.get("decisions", []),
        )
        item = case["item"]
        assert isinstance(item, ChecklistItem)
        messages = build_messages(
            anchor_text=str(case.get("anchor_text", "")),
            mission_text=str(case.get("mission_text", "")),
            snapshot=snapshot,
            item=item,
            specs=specs,
            memory_text=str(case.get("memory_text", "")),
            engine=bool(case.get("engine", False)),
            code_map_text=str(case.get("code_map_text", "")),
        )
        out.append(
            {
                "name": case["name"],
                "anchor_text": case.get("anchor_text", ""),
                "mission_text": case.get("mission_text", ""),
                "memory_text": case.get("memory_text", ""),
                "code_map_text": case.get("code_map_text", ""),
                "engine": case.get("engine", False),
                "snapshot": snapshot.model_dump(mode="json"),
                "item": item.model_dump(mode="json"),
                "specs": [s.model_dump(mode="json") for s in specs],
                "messages": [{"role": m.role, "content": m.content} for m in messages],
            }
        )
    return out


def _code_map_cases() -> dict[str, Any]:
    """The ripwire command for an item and how its output becomes the prompt section."""
    from lha.agent.code_map import (
        TRACE_CHARS,
        TRACE_SCRIPT,
        code_map_argv,
        code_map_query,
        found_code,
        trace_argv,
        trace_env,
    )

    items = [
        ChecklistItem(id="01", description="add farewell()"),
        ChecklistItem(id="02", description='quote " & -dash; $(rm -rf /) `x` déjà 🐟'),
        ChecklistItem(
            id="03",
            description="mask gitlab tokens",
            witnesses=["pytest:tests/test_redact.py::test_gitlab", "cmd:make check"],
            last_failure="déjà 🐟 " * 900 + "\ntests/test_redact.py:6: AssertionError",
        ),
    ]
    outputs = ["", "   \n\t ", " <ctx/> ", "<ctx>" + "déjà " * 5000 + "</ctx>"]
    return {
        "header": CODE_MAP_HEADER,
        "hard_cap": CODE_MAP_HARD_CAP,
        "argv": [
            {"item": i.model_dump(mode="json"), "budget": b, "argv": code_map_argv(i, b)}
            for i in items
            for b in (200, 2000)
        ],
        "render": [{"output": o, "rendered": render_code_map(o)} for o in outputs],
        "query": [{"item": i.model_dump(mode="json"), "query": code_map_query(i)} for i in items],
        "trace_chars": TRACE_CHARS,
        "trace_script": TRACE_SCRIPT,
        "trace_argv": trace_argv(),
        "trace_env": [
            {"item": i.model_dump(mode="json"), "budget": 1200, "env": trace_env(i, 1200)}
            for i in items
        ],
        "found_code": [
            {"output": o, "found": found_code(o)}
            for o in ["", "<ctx/>", '<ctx><d p="src/a.py:1"/></ctx>', '<ctx p="x"/>', 'p="nospace"']
        ],
    }


def _memory_cases() -> list[dict[str, Any]]:
    sections = [
        ("Past attempts", ["attempt one failed because of X " * 3, "attempt two é" * 20]),
        ("Skills", []),
        ("Repository", [f"src/file_{n}.py: def f{n}(): ..." for n in range(30)]),
    ]
    cases = []
    for budget, weights in [
        (4000, None),
        (600, None),
        (1200, (3.0, 1.0, 1.0)),
        (100, None),
        (50_000, (0.0, 0.0, 1.0)),
    ]:
        cases.append(
            {
                "sections": [{"title": t, "lines": lines} for t, lines in sections],
                "budget_chars": budget,
                "weights": list(weights) if weights is not None else None,
                "rendered": render_memory_block(sections, budget_chars=budget, weights=weights),
            }
        )
    return cases


_REPLIES = [
    ('{"tool": "read_file", "arguments": {"path": "x"}}', None),
    ('Sure: {"tool": "run", "arguments": {"argv": ["ls", "-la"], "n": 2}} ok', None),
    ('{"done": true, "summary": "ok"}', None),
    ('{"done": true, "summary": [1, "a", null, 2.50, {"k": false}]}', None),
    ('{"done": true}', None),
    ('{"done": "yes"}', None),
    ('{"tool": "", "arguments": {}}', None),
    ('{"tool": "x", "arguments": [1]}', None),
    ("I am finished.", None),
    ("{not json}", None),
    ('{"done": true}', "max_tokens"),
    ('{"done": true}', "LENGTH"),
    ('{"done": true}', "end_turn"),
    ("x" * 300, None),
]


def _action_cases() -> list[dict[str, Any]]:
    cases = []
    for text, stop in _REPLIES:
        action = parse_action(text, [], stop_reason=stop)
        cases.append(
            {
                "text": text,
                "stop_reason": stop,
                "done": action.done,
                "tool": action.tool,
                "arguments": action.arguments,
                "summary": action.summary,
                "error": action.error,
            }
        )
    return cases


_PLANS = [
    '[{"description": "a"}, {"description": "b", "depends_on": ["1"]}, '
    '{"description": "c", "depends_on": [1, "step 2", "#2"]}]',
    '[{"description": "a", "depends_on": ["2"]}, {"description": "b", "depends_on": ["2"]}, '
    '{"description": "c", "depends_on": ["99", "setup"]}]',
    'Plan:\n["plain step", {"step": "via step", "depends_on": [1.0, true, null, "", 7, 1.5]}, 42]',
    '[{"description": "a", "files": ["src/a.py", "src/A2.py"]}, '
    '{"description": "b", "files": ["src/b.py", "pyproject.toml"]}, '
    '{"description": "c", "files": ["SRC/a.py"]}, {"description": "d", "files": ["../x.py"]}, '
    '{"description": "e", "files": ".lha/checklist.json"}, {"description": "f", '
    '"allow_harness_edits": true, "depends_on": "1"}]',
    "no json here",
    '{"description": "not a list"}',
]


def _plan_cases() -> list[dict[str, Any]]:
    cases = []
    for text in _PLANS:
        items, files = parse_plan(text)
        ownership = assign_ownership(items, files)
        cases.append(
            {
                "text": text,
                "items": [i.model_dump(mode="json") for i in items],
                "files": files,
                "owners": ownership.owners,
            }
        )
    return cases


def export_agent_prompts() -> None:
    blocked = ChecklistItem(
        id="03",
        description="OneLake data plane",
        witnesses=["trusted:e2e"],
        consecutive_failures=3,
        last_failure="boom " * 1000,
    )
    _write(
        "agent/prompts.json",
        {
            "build_messages": _prompt_cases(),
            "lead_tools": {
                "specs": [
                    t.spec.model_dump(mode="json")
                    for t in [*default_local_tools(), RecordDecisionTool(DecisionBuffer())]
                ],
                "rendered": render_tools(
                    [t.spec for t in [*default_local_tools(), RecordDecisionTool(DecisionBuffer())]]
                ),
            },
            "render_memory_block": _memory_cases(),
            "code_map": _code_map_cases(),
            "corrective": corrective_message("no JSON object found in reply").content,
            "parse_action": _action_cases(),
            "parse_plan": _plan_cases(),
            "planner_messages": [
                {"role": m.role, "content": m.content}
                for m in _planner_messages("T", "Build the thing.", "")
            ],
            "planner_messages_acceptance": [
                {"role": m.role, "content": m.content}
                for m in _planner_messages("T", "Build the thing.", "all tests pass")
            ],
            "replanner": {
                "mission_text": "Mission: M",
                "item": blocked.model_dump(mode="json"),
                "messages": [
                    {"role": m.role, "content": m.content}
                    for m in _replanner_messages("Mission: M", blocked)
                ],
            },
        },
    )


def _planner_messages(title: str, description: str, acceptance: str) -> list[ModelMessage]:
    """The messages ``Planner.plan_mission`` sends (captured through a recording model)."""
    return asyncio.run(
        _capture(
            lambda m: Planner(m).plan_mission(
                title=title, description=description, acceptance=acceptance
            )
        )
    )


def _replanner_messages(mission_text: str, item: ChecklistItem) -> list[ModelMessage]:
    return asyncio.run(_capture(lambda m: Replanner(m).split(mission_text=mission_text, item=item)))


async def _capture(call: Any) -> list[ModelMessage]:
    seen: list[ModelMessage] = []

    class _Recording(StubModel):
        async def complete(
            self,
            messages: list[ModelMessage],
            *,
            tools: list[dict[str, object]] | None = None,
            max_tokens: int | None = None,
        ) -> TurnResult:
            seen.extend(messages)
            return TurnResult(text="[]")

    await call(_Recording())
    return seen


# --- agent/org --------------------------------------------------------------------------------
# The multi-agent organization's prompts and parsers: the role chart, the sub-agent prompt (tool
# visibility by role), the Reviewer's prompt and verdict parsing, reflection, the implementer's
# objective, the ownership guard's refusals, lease decisions and the ticket lifecycle.


class _SpecsOnly:
    """A dispatcher that only lists specs (the sub-agents under test never call a tool)."""

    def __init__(self, specs: list[ToolSpec]) -> None:
        self._specs = specs

    def specs(self) -> list[ToolSpec]:
        return list(self._specs)

    async def dispatch(self, call: Any, ctx: Any) -> Any:  # pragma: no cover - never called
        raise AssertionError("no tool call expected")


def _org_specs() -> list[ToolSpec]:
    return [
        *(t.spec for t in default_local_tools()),
        RecordDecisionTool(DecisionBuffer()).spec,
        LEASE_SPEC,
        FetchUrlTool.spec,
        WebSearchTool.spec,
    ]


def _messages(seen: list[ModelMessage]) -> list[dict[str, str]]:
    return [{"role": m.role, "content": m.content} for m in seen]


_SUBAGENT_RUNS = [
    ("researcher", "Find context relevant to: add a CLI", ""),
    ("reviewer", "Review it", "  Acceptance criteria: x\n\n"),
    ("implementer", "Checklist item [01]: do it", "Mission: M\n\nResearch briefs:\nB"),
]


def _subagent_messages(role: str, objective: str, extra: str) -> list[dict[str, str]]:
    dispatcher = _SpecsOnly(_org_specs())
    return _messages(
        asyncio.run(
            _capture(
                lambda m: SubAgent(role=ROLES[role], model=m, dispatcher=dispatcher).run(
                    objective=objective,
                    ctx=None,  # type: ignore[arg-type]
                    extra_context=extra,
                )
            )
        )
    )


def _subagent_cases() -> list[dict[str, Any]]:
    cases = []
    for role, objective, extra in _SUBAGENT_RUNS:
        agent = SubAgent(role=ROLES[role], model=StubModel(), dispatcher=_SpecsOnly(_org_specs()))
        cases.append(
            {
                "role": role,
                "objective": objective,
                "extra_context": extra,
                "visible": [s.name for s in agent.visible_specs()],
                "messages": _subagent_messages(role, objective, extra),
            }
        )
    return cases


_REVIEW_REPLIES = [
    '{"done": true, "verdict": "approve", "blocking_issues": [], "advisory": ["docs"]}',
    '{"done": true, "verdict": "APPROVED ", "blocking_issues": ["no tests"]}',
    '{"done": true, "verdict": "request_changes", "blocking_issues": [" a ", "", 3, null]}',
    '{"done": true, "verdict": "maybe", "blocking_issues": []}',
    '{"done": true, "verdict": "maybe", "blocking_issues": ["x"]}',
    '{"done": true, "verdict": "maybe"}',
    '{"done": true, "blocking_issues": "not a list"}',
    '{"done": true, "summary": "no verdict"}',
    "Looks fine.\n  block: missing error handling\nBLOCK:  and no tests \n",
    "No blocking issues found.",
    "",
    '{"verdict": 7, "advisory": [1.5, true, {"k": "v"}]}',
]


def _review_cases() -> list[dict[str, Any]]:
    cases = []
    for text in _REVIEW_REPLIES:
        verdict, blocking, issues, advisory = parse_review(text)
        notes = ReviewResult(
            brief=text,
            blocking=blocking,
            tool_calls=0,
            verdict=verdict,
            blocking_issues=issues,
            advisory=advisory,
        ).notes()
        cases.append(
            {
                "text": text,
                "verdict": verdict,
                "blocking": blocking,
                "blocking_issues": issues,
                "advisory": advisory,
                "notes": notes,
            }
        )
    return cases


def _reviewer_messages(criteria: str, diff: str) -> list[dict[str, str]]:
    dispatcher = _SpecsOnly(_org_specs())
    return _messages(
        asyncio.run(
            _capture(
                lambda m: Reviewer(m, dispatcher).review(
                    diff=diff,
                    criteria=criteria,
                    ctx=None,  # type: ignore[arg-type]
                )
            )
        )
    )


_LONG_DIFF = "diff --git a/x b/x\n+é" * 1200


def _implementer_cases() -> list[dict[str, Any]]:
    owners = FileOwnershipMap()
    owners.assign("src/a.py", "implementer-01")
    owners.assign("src/B.py", "implementer-01")
    plain = ChecklistItem(id="01", description="Add the model")
    failed = ChecklistItem(
        id="01", description="Add the model", attempts=2, last_failure="boom é " * 700
    )
    runs: list[tuple[str, ChecklistItem, list[str], dict[str, Any]]] = [
        ("minimal", plain, ["pytest", "ruff"], {"mission_text": ""}),
        (
            "full",
            failed,
            [],
            {
                "mission_text": "Mission: M\n\nBuild it.",
                "reflection": "\n  Reflection on 01: try again  \n",
                "decisions_text": "Design decisions already recorded: x",
                "briefs": ["B1", "B2"],
                "board": "Team board (earlier rounds):\n[a] b",
            },
        ),
        (
            "no_lease_tool",
            plain,
            ["pytest"],
            {"mission_text": "M", "lease_tool": False, "reflection": "  "},
        ),
    ]
    cases = []
    for name, item, acceptance, kwargs in runs:
        run = new_implementer_run(item, "c3", owners, tool_budget=12, acceptance=acceptance)
        objective, extra = implementer_objective(run, **kwargs)
        cases.append(
            {
                "name": name,
                "item": item.model_dump(mode="json"),
                "owners": owners.owners,
                "cycle_id": "c3",
                "acceptance": acceptance,
                "inputs": kwargs,
                "ticket_id": run.ticket.id,
                "write_set": run.ticket.contract.write_set,
                "objective": objective,
                "extra": extra,
            }
        )
    return cases


def _guard_cases() -> list[dict[str, Any]]:
    owners = FileOwnershipMap()
    owners.assign("mine.py", "implementer-01")
    owners.assign("theirs.py", "implementer-02")
    cases = []
    for writers, lease_tool, path in [
        (["implementer-01"], False, "mine.py"),
        (["implementer-01"], False, "./Theirs.py"),
        (["implementer-01"], True, "theirs.py"),
        (["implementer-01"], False, "pyproject.toml"),
        (["implementer-01"], True, "src/__init__.py"),
        (["implementer-01"], False, "new.py"),
        (["implementer-01"], True, "../x.py"),
        (["implementer-01"], False, "/etc/passwd"),
        (["lead", "implementer-02"], False, "theirs.py"),
        (["lead", "implementer-02"], False, "new.py"),
        (["lead", "implementer-02"], False, "mine.py"),
        (["lead"], False, "pyproject.toml"),
    ]:
        guard = OwnershipGuard(_SpecsOnly([]), owners, writers=writers, lease_tool=lease_tool)
        cases.append(
            {
                "owners": owners.owners,
                "writers": writers,
                "lease_tool": lease_tool,
                "path": path,
                "refusal": guard.refusal(path),
            }
        )
    return cases


def _lease_cases() -> list[dict[str, Any]]:
    owners = FileOwnershipMap()
    owners.assign("a.py", "implementer-01")
    owners.assign("b.py", "implementer-02")
    owners.assign("c.py", "lead")
    cases = []
    for writer, path, reason, finished in [
        ("implementer-01", "src/new.py", " why ", []),
        ("implementer-01", "./a.py", "mine", []),
        ("implementer-01", "b.py", "need it", []),
        ("implementer-01", "B.py", "now", ["implementer-02"]),
        ("implementer-01", "c.py", "lead's", []),
        ("implementer-01", "pyproject.toml", "deps", []),
        ("implementer-01", ".lha/ownership.json", "x", []),
        ("implementer-01", ".GIT/config", "x", []),
        ("implementer-01", "../x.py", "x", []),
        ("lead", "d.py", "x", []),
        ("implementer-03", "e.py", "r" * 600, []),
    ]:
        decision = decide_lease(
            owners, LeaseRequest(writer=writer, path=path, reason=reason), finished=finished
        )
        cases.append(
            {
                "owners": owners.owners,
                "writer": writer,
                "path": path,
                "reason": reason,
                "finished": finished,
                "decision": decision.model_dump(),
                "message": decision.message(),
            }
        )
    return cases


def _ticket_cases() -> list[dict[str, Any]]:
    contract = TaskContract(objective="o")
    return [
        {
            "from": start.value,
            "to": to.value,
            "legal": Ticket(id="t", contract=contract, status=start).can_transition(to),
        }
        for start in TicketStatus
        for to in TicketStatus
    ]


def export_agent_org() -> None:
    failure = "FAILED ✗ " * 800
    reflection_seen = asyncio.run(
        _capture(
            lambda m: reflect_on_failure(
                model=m, item_description="Wire the API", failure_summary=failure
            )
        )
    )
    _write(
        "agent/org.json",
        {
            "roles": {
                name: {
                    "tier": role.tier.value,
                    "claude_model": claude_model_for(role.tier),
                    "system_prompt": role.system_prompt,
                    "allow_mutating": role.allow_mutating,
                    "allow_egress": role.allow_egress,
                    "max_turns": role.max_turns,
                }
                for name, role in ROLES.items()
            },
            "specs": [s.model_dump(mode="json") for s in _org_specs()],
            "rendered_specs": render_tools(_org_specs()),
            "subagent": _subagent_cases(),
            "reviewer_messages": [
                {
                    "criteria": criteria,
                    "diff": diff,
                    "messages": _reviewer_messages(criteria, diff),
                }
                for criteria, diff in [("Add the model", "(empty diff)"), ("Ünïcode", _LONG_DIFF)]
            ],
            "parse_review": _review_cases(),
            "reflection": {
                "item_description": "Wire the API",
                "failure_summary": failure,
                "messages": _messages(reflection_seen),
            },
            "implementer_objective": _implementer_cases(),
            "ownership_guard": _guard_cases(),
            "leases": _lease_cases(),
            "tickets": _ticket_cases(),
        },
    )


# --- execution/paths --------------------------------------------------------------------------

_REL_PATHS = [
    "",
    ".",
    "./",
    "a",
    "a/./b/../c.txt",
    "a//b/",
    "src\\pkg\\x.py",
    "../x",
    "..",
    "a/../../x",
    "a/b/../../..",
    "a/..",
    "/etc/passwd",
    "\\\\srv\\share",
    "C:\\x",
    "C:x",
    "1:x",
    "é:x",
    "a:b",
    "a\x00b",
    ".lha/checklist.json",
    "./.git/hooks/pre-commit",
    ".GIT/config",
    ".Lha",
    "a/../.git/config",
    "src/.git_notes",
    ".github/workflows/ci.yml",
    ".gitignore",
    "sub/.git/x",
    "'quoted'",
    "mixed\"'",
    "tab\there",
    "ünïcödé/файл.txt",
]


def export_paths() -> None:
    from lha.execution.paths import (
        PathEscapeError,
        contained_posix,
        is_protected,
        normalize_relpath,
    )

    normalize: list[dict[str, Any]] = []
    protected: list[dict[str, Any]] = []
    contained: list[dict[str, Any]] = []
    for path in _REL_PATHS:
        try:
            normalize.append(
                {"path": path, "normalized": str(normalize_relpath(path)), "error": None}
            )
        except PathEscapeError as exc:
            normalize.append({"path": path, "normalized": None, "error": str(exc)})
        try:
            protected.append({"path": path, "protected": is_protected(path), "error": None})
        except PathEscapeError as exc:
            protected.append({"path": path, "protected": None, "error": str(exc)})
        for workdir in ("/workspace", "/home/user/workspace/"):
            try:
                result = contained_posix(workdir, path)
                contained.append(
                    {"workdir": workdir, "path": path, "result": result, "error": None}
                )
            except PathEscapeError as exc:
                contained.append(
                    {"workdir": workdir, "path": path, "result": None, "error": str(exc)}
                )
    _write(
        "execution/paths.json",
        {"normalize_relpath": normalize, "is_protected": protected, "contained_posix": contained},
    )


# --- execution/arguments ----------------------------------------------------------------------


def _argument_corpus() -> list[tuple[dict[str, Any], Any, str]]:
    from lha.execution.tools import (
        GrepTool,
        ReadFileTool,
        RecordDecisionTool,
        ShellTool,
        WriteFileTool,
    )
    from lha.execution.tools.decisions import DecisionBuffer
    from lha.execution.tools.web import FetchUrlTool, WebSearchTool

    shell = ShellTool.spec.parameters
    write = WriteFileTool.spec.parameters
    read = ReadFileTool.spec.parameters
    grep = GrepTool.spec.parameters
    fetch = FetchUrlTool.spec.parameters
    search = WebSearchTool.spec.parameters
    decision = RecordDecisionTool(DecisionBuffer()).spec.parameters
    enum = {"type": "string", "enum": ["a", "b", "ü"]}
    num_enum = {"enum": [1, 2.5, None, "x"]}
    bounded = {"type": "number", "minimum": 0.5, "maximum": 10}
    nested = {
        "type": "object",
        "properties": {
            "cfg": {
                "type": "object",
                "properties": {"name": {"type": "string"}, "tags": {"type": "array"}},
                "required": ["name", "tags"],
                "additionalProperties": False,
            },
            "rows": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {"id": {"type": "integer", "minimum": 0}},
                    "required": ["id"],
                },
            },
            "extra": {"type": "null"},
        },
        "additionalProperties": {"type": "boolean"},
    }
    odd = {"type": ["string", "null"], "properties": "not-a-dict", "enum": "not-a-list"}
    return [
        (shell, {"argv": ["ls", "-la"]}, ""),
        (shell, {"argv": "rm -rf /"}, ""),
        (shell, {"argv": ["ls", 3]}, ""),
        (shell, {"argv": ["ls", None]}, ""),
        (shell, {"argv": ["ls", 1.5]}, ""),
        (shell, {"argv": ["ls"], "timeout_s": True}, ""),
        (shell, {"argv": ["ls"], "timeout_s": 0}, ""),
        (shell, {"argv": ["ls"], "timeout_s": -3}, ""),
        (shell, {"argv": ["ls"], "timeout_s": 1.5}, ""),
        (shell, {"argv": ["ls"], "timeout_s": 5.0}, ""),
        (shell, {"argv": ["ls"], "timeout_s": 30}, ""),
        (shell, {"argv": ["ls"], "unknown": {"x": 1}}, ""),
        (write, {"path": "a", "content": ["x"]}, ""),
        (write, {"path": "a", "content": {"k": "v"}}, ""),
        (write, {"path": 123, "content": "x"}, ""),
        (read, {"path": "a"}, ""),
        (read, {"path": False}, ""),
        (grep, {"pattern": "x", "regex": "yes"}, ""),
        (grep, {"pattern": "x", "regex": 1}, ""),
        (grep, {"pattern": "x", "regex": True, "subdir": "src"}, ""),
        (fetch, {"url": "https://a.test", "headers": {"X-Count": 3}}, ""),
        (fetch, {"url": "https://a.test", "headers": ["x"]}, ""),
        (fetch, {"url": "https://a.test", "headers": {"Accept": "text/html"}}, ""),
        (search, {"query": "q", "max_results": "5"}, ""),
        (search, {"query": "q", "max_results": 5}, ""),
        (decision, {"decision": "d", "rationale": "r"}, ""),
        (decision, {"decision": "d", "rationale": "r", "extra": 1}, ""),
        (decision, {"decision": "d", "rationale": "r", "affected": ["a", 2]}, ""),
        (decision, {"decision": "d", "rationale": 3}, ""),
        (enum, "a", "mode"),
        (enum, "ü", "mode"),
        (enum, "c", "mode"),
        (enum, 1, "mode"),
        (num_enum, 1.0, "v"),
        (num_enum, True, "v"),
        (num_enum, 2.5, "v"),
        (num_enum, None, "v"),
        (num_enum, 3, "v"),
        (num_enum, [1], "v"),
        (bounded, 0.5, "n"),
        (bounded, 0.25, "n"),
        (bounded, 11, "n"),
        (bounded, 10.0, "n"),
        (bounded, True, "n"),
        (bounded, 1e300, "n"),
        (nested, {"cfg": {"name": "x", "tags": []}}, ""),
        (nested, {"cfg": {"name": "x"}}, ""),
        (nested, {"cfg": {"name": "x", "tags": [], "more": 1}}, ""),
        (nested, {"cfg": {"name": 1, "tags": []}}, ""),
        (nested, {"rows": [{"id": 1}, {"id": -1}]}, ""),
        (nested, {"rows": [{"id": 1}, {}]}, ""),
        (nested, {"rows": [{"id": 1}, "x"]}, ""),
        (nested, {"extra": None}, ""),
        (nested, {"extra": 0}, ""),
        (nested, {"flag": True}, ""),
        (nested, {"flag": "yes"}, ""),
        (nested, {"cfg": "x"}, "outer"),
        (odd, 5, "o"),
        (odd, {"k": 1}, "o"),
        ({}, {"anything": [1, {"a": None}]}, ""),
        ({"type": "object"}, [], ""),
        ({"type": "array", "items": {"type": "string"}}, ["a", "b", 3], "list"),
        ({"type": "integer"}, 10**20, "big"),
        ({"type": "string"}, 'quote\'d "x"', "s"),
    ]


def export_arguments() -> None:
    from lha.execution.dispatcher import _missing_required, validate_arguments

    cases = []
    for schema, value, where in _argument_corpus():
        error = validate_arguments(schema, value, where)
        if isinstance(value, dict):
            # Go maps have no insertion order: every case must give the same error sorted.
            resorted = {k: value[k] for k in sorted(value)}
            assert validate_arguments(schema, resorted, where) == error, (schema, value)
        cases.append({"schema": schema, "value": value, "where": where, "error": error})
    missing = []
    for required, arguments in [
        (["path"], {}),
        (["path", "content"], {"content": "x"}),
        (["b", "a"], {}),
        ([], {"x": 1}),
        ("not-a-list", {}),
        ([1, "a"], {"a": 1}),
    ]:
        found = _missing_required({"required": required}, arguments)
        missing.append({"required": required, "arguments": arguments, "missing": sorted(found)})
    _write("execution/arguments.json", {"validate": cases, "missing_required": missing})


# --- execution/sandbox_egress -----------------------------------------------------------------

_SANDBOX_EGRESS_LISTS: list[tuple[list[str], list[str], list[str]]] = [
    ([], [], []),
    (["proxy.golang.org", "sum.golang.org", "storage.googleapis.com"], [], []),
    (["PyPI.org.", "files.pythonhosted.org", "pypi.org"], [], []),
    (["github.com"], [], []),
    ([".golang.org"], [], []),
    (["pypi.org:8443"], [], []),
    (["10.0.0.1"], [], []),
    ([], ["mirror.internal.example", ".docs.example"], []),
    ([], ["api.github.com"], []),
    ([], [".com"], []),
    ([], ["bucket.s3.eu-west-1.amazonaws.com"], []),
    ([], [".pypi.org"], []),
    ([], ["http://x.org"], []),
    ([], [], ["github.com", ".amazonaws.com", "upload.pypi.org"]),
    ([], [], ["x.org:99999"]),
    (["pypi.org"], ["pypi.org", "mirror.example"], ["mirror.example", "github.com"]),
]

_RULE_OF_TWO_ENV: list[dict[str, str]] = [
    {"sandbox_egress": "pypi.org"},
    {"sandbox_egress": "pypi.org", "private_data": "true"},
    {"sandbox_egress_allow_write_hosts": "github.com, gitlab.com", "private_data": "true"},
    {"sandbox_egress": "pypi.org", "web_allow_hosts": "b.test,a.test", "private_data": "true"},
    {"web_allow_hosts": "docs.test", "private_data": "true"},
    {"sandbox": "local", "sandbox_egress": "pypi.org"},
    {"sandbox": "local", "allow_unsafe_local": "true", "web_allow_hosts": "docs.test"},
    {"sandbox": "e2b", "sandbox_egress": "pypi.org", "private_data": "true"},
    {"private_data": "true"},
]

_PROXY_LOG = [
    "2026-09-26 10:00:00,001 INFO lha-egress-proxy listening on 0.0.0.0:3128 allow=pypi.org",
    "2026-09-26 10:00:01,000 INFO allow CONNECT pypi.org:443 -> 151.101.0.223:443",
    "2026-09-26 10:00:01,500 INFO allow CONNECT pypi.org:443 -> 151.101.64.223:443",
    "2026-09-26 10:00:02,000 WARNING deny CONNECT github.com:443: host not in egress allow-list: "
    "github.com",
    "2026-09-26 10:00:03,000 WARNING deny GET http://example.org:8080/x?token=abc: host not in "
    "egress allow-list: example.org",
    "2026-09-26 10:00:03,500 INFO allow GET http://files.example/a -> 93.184.216.34:80",
    "2026-09-26 10:00:04,000 WARNING fail CONNECT [2001:db8::1]:8443: upstream unreachable: x",
    "2026-09-26 10:00:05,000 WARNING deny CONNECT 10.0.0.1:443: IP-literal hosts are not "
    "allowed: 10.0.0.1",
    "2026-09-26 10:00:06,000 ERROR error: boom",
    "not a log line",
]


def export_sandbox_egress() -> None:
    from lha.config import Settings
    from lha.execution.egress_events import parse_proxy_log
    from lha.execution.egress_hosts import (
        PACKAGE_FETCH_HOSTS,
        WRITE_HOSTS,
        SandboxEgressError,
        sandbox_allow_list,
    )
    from lha.execution.tools.toolset import check_run_rule_of_two
    from lha.safety.rule_of_two import RuleOfTwoViolation

    lists: list[dict[str, Any]] = []
    for egress, extra, write in _SANDBOX_EGRESS_LISTS:
        case: dict[str, Any] = {"egress": egress, "extra": extra, "write": write}
        try:
            case |= {"hosts": sandbox_allow_list(egress, extra, write), "error": None}
        except SandboxEgressError as exc:
            case |= {"hosts": None, "error": str(exc)}
        lists.append(case)
    rule: list[dict[str, Any]] = []
    for env in _RULE_OF_TWO_ENV:
        settings = Settings(_env_file=None, **env)  # type: ignore[call-arg]
        try:
            check_run_rule_of_two(settings)
            error = None
        except RuleOfTwoViolation as exc:
            error = str(exc)
        rule.append(
            {"env": env, "egress_enabled": settings.sandbox_egress_enabled(), "error": error}
        )
    _write(
        "execution/sandbox_egress.json",
        {
            "package_fetch_hosts": list(PACKAGE_FETCH_HOSTS),
            "write_hosts": list(WRITE_HOSTS),
            "allow_list": lists,
            "rule_of_two": rule,
            "proxy_log": {
                "lines": _PROXY_LOG,
                "events": [e.payload for e in parse_proxy_log(_PROXY_LOG)],
            },
        },
    )


_MEMORY_TEXTS = [
    "",
    "hello",
    "Fix the config parser: KeyError 'port' (twice) port PORT",
    "déjà vu — ÉCOLE of 42 fish_and_chips; tabs\tand\nnewlines",
    "a b c d e f g h i j k l m n o p q r s t u v w x y z 0 1 2 3 4 5 6 7 8 9",
    "listen port configurable " * 12,
]

_BM25_DOCS = [
    ("d1", "the config parser reads key=value lines"),
    ("d2", "listen on the configured port; default port 8080"),
    ("d3", "config config config port"),
    ("d4", "unrelated text about fish"),
    ("d5", ""),
    ("d1", "the config parser reads key value lines and the port"),  # re-add replaces d1
    ("d6", "Port PORT port: the listen port"),
]

_BM25_QUERIES = [
    "config parser",
    "listen port",
    "port",
    "fish and chips",
    "",
    "nothing matches here",
    "the the the",
]


def export_memory() -> None:
    """spec/memory: hash-embedder vectors, cosine, BM25 scores, RRF fusion order, episode and
    term rendering, and full memory blocks recalled by ``MissionMemory`` for a fixture."""
    from tests.unit.memory_spec_fixture import memory_cases, run_case

    from lha.contracts.memory import MemoryRecord
    from lha.memory.embeddings import HashEmbedder
    from lha.memory.hybrid import BM25Index, reciprocal_rank_fusion
    from lha.memory.semantic_memory import cosine
    from lha.memory.service import _render_episode, _terms

    hash_cases = []
    for dim in (256, 1024, 7):
        embedder = HashEmbedder(dim=dim)
        for text in _MEMORY_TEXTS:
            (vector,) = asyncio.run(embedder.embed([text]))
            hash_cases.append({"dim": dim, "text": text, "vector": vector})
    cosine_cases = []
    for a, b in [
        ([1.0, 0.0], [0.0, 1.0]),
        ([1.0, 2.0, 3.0], [3.0, 2.0, 1.0]),
        ([0.1] * 10, [0.2, 0.3] * 5),
        ([0.0, 0.0], [1.0, 1.0]),
        ([1e-300, 1e300, -1e300], [1.0, 1.0, 1.0]),
        ([0.1, 0.2, 0.3, 1e16, -1e16], [0.3, 0.2, 0.1, 1.0, 1.0]),
    ]:
        cosine_cases.append({"a": a, "b": b, "cosine": cosine(a, b)})
    _write("memory/hash_embedder.json", {"hash": hash_cases, "cosine": cosine_cases})

    bm25 = BM25Index()
    for doc_id, text in _BM25_DOCS:
        bm25.add([MemoryRecord(id=doc_id, kind="semantic", text=text)])
    bm25_cases = [
        {"query": q, "k": k, "hits": [[d, s] for d, s in bm25.query(q, k=k)]}
        for q in _BM25_QUERIES
        for k in (10, 2)
    ]
    rankings_cases = [
        [["a", "b", "c"], ["c", "b", "a"]],
        [["a", "b"], ["c", "d"]],
        [["x"], [], ["x", "y"], ["y", "x", "z"]],
        [],
        [["only"]],
    ]
    fusion = [
        {"rankings": r, "fused": [[d, s] for d, s in reciprocal_rank_fusion(r)]}
        for r in rankings_cases
    ]
    _write(
        "memory/retrieval.json",
        {
            "bm25_docs": [list(d) for d in _BM25_DOCS],
            "bm25": bm25_cases,
            "fusion": fusion,
        },
    )

    episodes = []
    for payload, cycle in [
        ({}, "c1"),
        (
            {
                "item_id": "01",
                "verdict": "failed",
                "attempts": 2,
                "status": "in_progress",
                "tools": ["read_file", "write_file", "read_file"],
                "files": [f"f{i}.py" for i in range(10)],
                "summary": "tried  a\tregex " * 20,
                "failure": "KeyError " * 60,
                "verified": False,
            },
            "c2",
        ),
        (
            {
                "item_id": "02",
                "verdict": "",
                "verified": True,
                "summary": "worked",
                "failure": "ignored",
                "attempts": 1,
                "status": "done",
            },
            "c3",
        ),
        ({"item_id": None, "verdict": None, "verified": True, "summary": ""}, "c4"),
    ]:
        episodes.append(
            {"payload": payload, "cycle_id": cycle, "rendered": _render_episode(payload, cycle)}
        )
    terms = [
        {"text": t, "terms": _terms(t)}
        for t in [
            "Fix the config parser for missing port keys",
            "make the LISTEN port configurable, the port!",
            "add a test for each item in files",
            "snake_case_name and CamelCase and x9_y",
        ]
    ]
    recall = []
    for case in memory_cases():
        blocks = asyncio.run(run_case(case))
        recall.append({**case, "blocks": blocks})
    _write("memory/recall.json", {"episodes": episodes, "terms": terms, "cases": recall})


def _voyage_responses() -> list[tuple[int, object]]:
    vec = [0.5, -1, 2.25e-3]
    return [
        (2, {"data": [{"index": 1, "embedding": [3, 4]}, {"index": 0, "embedding": vec}]}),
        (1, {"object": "list", "data": [{"object": "embedding", "embedding": vec, "index": 0}]}),
        (0, {"data": []}),
        (1, {"data": {"index": 0}}),
        (1, []),
        (1, {"data": ["x"]}),
        (1, {"data": [{"index": "0", "embedding": vec}]}),
        (1, {"data": [{"index": 1.0, "embedding": vec}]}),
        (1, {"data": [{"index": None, "embedding": vec}]}),
        (1, {"data": [{"index": True, "embedding": vec}]}),
        (2, {"data": [{"index": 2, "embedding": vec}]}),
        (1, {"data": [{"index": -1, "embedding": vec}]}),
        (1, {"data": [{"index": 0, "embedding": []}]}),
        (1, {"data": [{"index": 0}]}),
        (1, {"data": [{"index": 0, "embedding": [1, "2"]}]}),
        (1, {"data": [{"index": 0, "embedding": [1, False]}]}),
        (2, {"data": [{"index": 0, "embedding": vec}, {"index": 0, "embedding": vec}]}),
        (2, {"data": [{"index": 0, "embedding": vec}]}),
        (1, {"data": [{"index": 0, "embedding": vec}, {"index": 1, "embedding": vec}]}),
    ]


def export_voyage() -> None:
    """spec/memory/voyage.json: the Voyage embedder's batching, the exact request bodies it
    sends for the same inputs, how it reads (or rejects) a response, and its probe's HTTP-status
    messages."""
    from lha.memory.embeddings import (
        VOYAGE_BATCH_CHARS,
        VOYAGE_BATCH_TEXTS,
        VOYAGE_DEFAULT_MODEL,
        parse_voyage_response,
        voyage_batches,
        voyage_request_body,
        voyage_status_message,
    )

    batch_inputs: list[tuple[list[str], int, int]] = [
        ([], 3, 10),
        (["a"], 3, 10),
        (["a", "bb", "ccc", "dddd"], 3, 10),
        (["a", "bb", "ccc", "dddd"], 2, 100),
        (["0123456789abc", "x", "y"], 3, 10),  # one text over the budget travels alone
        (["x", "0123456789abc", "y"], 5, 10),
        (["déjà", "vu", "🐟🐟🐟", "日本語テキスト"], 10, 8),  # code points, not bytes
        (["", "", "", ""], 3, 0),
        (["abcde", "fghij", "k"], 10, 10),  # exactly at the budget stays together
    ]
    batches = [
        {
            "texts": texts,
            "max_texts": n,
            "max_chars": c,
            "batches": voyage_batches(texts, max_texts=n, max_chars=c),
        }
        for texts, n, c in batch_inputs
    ]
    request_inputs: list[tuple[str, str, list[str]]] = [
        (VOYAGE_DEFAULT_MODEL, "document", ["dimension probe"]),
        (VOYAGE_DEFAULT_MODEL, "query", ["make the listen port configurable"]),
        ("voyage-code-3", "document", ["def f():\n\treturn '<a & b>'", 'quote " and \\ slash']),
        ("voyage-3.5-lite", "query", ["déjà vu — ÉCOLE", "🐟 \u2028 \x00 end"]),
        (VOYAGE_DEFAULT_MODEL, "document", [f"chunk {i}" for i in range(130)]),
        (VOYAGE_DEFAULT_MODEL, "document", []),
    ]
    requests = [
        {
            "model": model,
            "input_type": input_type,
            "texts": texts,
            "bodies": [
                voyage_request_body(model, batch, input_type) for batch in voyage_batches(texts)
            ],
        }
        for model, input_type, texts in request_inputs
    ]
    responses = []
    for count, data in _voyage_responses():
        try:
            responses.append(
                {"count": count, "response": data, "vectors": parse_voyage_response(data, count)}
            )
        except ValueError as exc:
            responses.append({"count": count, "response": data, "error": str(exc)})
    statuses = [
        {"model": model, "status": status, "message": voyage_status_message(model, status)}
        for model in (VOYAGE_DEFAULT_MODEL, "it's-quoted")
        for status in (400, 401, 403, 408, 429, 500, 503)
    ]
    _write(
        "memory/voyage.json",
        {
            "default_model": VOYAGE_DEFAULT_MODEL,
            "batch_texts": VOYAGE_BATCH_TEXTS,
            "batch_chars": VOYAGE_BATCH_CHARS,
            "batches": batches,
            "requests": requests,
            "responses": responses,
            "statuses": statuses,
        },
    )


# --- state/wire_bytes: raw bytes of Go/Python-written JSON -------------------------------------

#: Event payloads whose key order is not sorted, with floats, nesting, escapes and non-ASCII: the
#: anchor writes each as ``EventRecord.model_dump_json()`` and both implementations must produce
#: exactly these bytes (compared as bytes, never as parsed JSON).
_WIRE_EVENTS: list[dict[str, Any]] = [
    {"kind": "cycle", "cycle_id": "c1", "payload": {}},
    {"kind": "started", "cycle_id": "", "payload": {}, "payload_ref": "blobs/x"},
    {
        "kind": "cycle",
        "cycle_id": "c2",
        "payload": {
            "item_id": "01",
            "verified": True,
            "verdict": "passed",
            "status": "done",
            "tool_calls": 3,
            "split_into": [],
            "rolled_back": False,
            "checks": [
                {
                    "name": "pytest",
                    "passed": True,
                    "gating": True,
                    "exit_code": 0,
                    "duration_s": 0.0,
                },
                {
                    "name": "ruff",
                    "passed": False,
                    "gating": False,
                    "exit_code": 1,
                    "duration_s": 1.5,
                },
            ],
        },
    },
    {
        "kind": "ticket",
        "cycle_id": "c3",
        "payload": {
            "ticket_id": "c3-02",
            "item_id": "02",
            "history": [{"status": "created", "note": ""}, {"status": "done", "note": "ok <&>"}],
            "leases": [{"path": "b.py", "granted": True, "why": "free"}],
            "branch": None,
        },
    },
    {
        "kind": "note",
        "cycle_id": "c4",
        "payload": {
            "zeta": 1,
            "alpha": 1.0,
            "big": 1e16,
            "small": 1e-05,
            "tiny": 1.5e-07,
            "neg": -2.5,
            "text": 'é ü \u2028 \u2029 \x85 "q" \\ \t\n',
            "nested": {"z": {"y": [None, True, {"b": 2, "a": 1}]}, "a": []},
        },
    },
]

_WIRE_OWNERSHIP: list[dict[str, Any]] = [
    {
        "items": ["02", "01", "03"],
        "files": {
            "02": ["src/zeta.py", "src/Alpha.py", "docs/b.md"],
            "01": ["README.md", "src/mid.py"],
            "03": ["src/beta.py", "src/alpha.py"],
        },
        "release": [],
        "reassign": [],
    },
    {
        "items": ["01", "02"],
        "files": {"01": ["z.py", "a.py", "m.py"], "02": ["y.py", "b.py"]},
        "release": ["implementer-01"],
        "reassign": [["b.py", "implementer-01"], ["c.py", "lead"]],
    },
]

_WIRE_GATES: list[dict[str, Any]] = [
    {
        "mission_id": "m1",
        "workdir": "/w",
        "gate_id": "g1",
        "kind": "tool_call",
        "event": "opened",
        "question": "Allow `git push` with token=sk-live-abcdefghijklmnop? <&> é",
        "options": ["approve", "reject"],
        "default_action": "reject",
        "deadline": "2026-09-27T10:00:00Z",
        "request": {
            "fingerprint": "0c6efe57c6d81fa336b6a6459b37de23",
            "tool": "run_command",
            "reason": "git push (outward-facing / rewrites history)",
            "arguments": "{'argv': ['git', 'push']}",
        },
    },
    {
        "mission_id": "m1",
        "workdir": "/w",
        "gate_id": "g2",
        "kind": "deadlock",
        "event": "reminder",
        "question": "Retry blocked items?",
        "options": ["retry", "abort"],
        "default_action": "abort",
        "step": 2,
    },
    {
        "mission_id": "m1",
        "workdir": "/w",
        "gate_id": "g2",
        "kind": "deadlock",
        "event": "resolved",
        "decision": "retry",
    },
]


def _wire_ownership(case: dict[str, Any]) -> str:
    items = [ChecklistItem(id=i, description=f"item {i}") for i in case["items"]]
    ownership = assign_ownership(items, case["files"])
    for writer in case["release"]:
        ownership.release(writer)
    for path, writer in case["reassign"]:
        ownership.reassign(path, writer)
    return ownership.model_dump_json(indent=2)


def _wire_gate(case: dict[str, Any]) -> dict[str, str]:
    import httpx._content

    from lha.durable.activities import gate_notice_payload
    from lha.durable.types import GateNotice, PendingApproval

    fields = dict(case)
    if fields.get("request") is not None:
        fields["request"] = PendingApproval(**fields["request"])
    notice = GateNotice(**fields)
    payload = gate_notice_payload(notice)
    _, stream = httpx._content.encode_json(payload)
    event = EventRecord(
        kind=f"gate_{notice.event}", cycle_id=f"gate:{notice.gate_id}", payload=payload
    )
    return {"body": b"".join(stream).decode("utf-8"), "event": event.model_dump_json()}


def _wire_anchor_file(lines: list[str]) -> str:
    import tempfile

    from lha.contracts.state import Checkpoint
    from lha.state.mission_anchor import GitMissionAnchor

    async def run(workdir: str) -> str:
        anchor = GitMissionAnchor(workdir)
        items = Checklist(items=[ChecklistItem(id="01", description="one")])
        await anchor.initialize(title="Wire", description="bytes", items=items)
        await anchor.commit_checkpoint(
            Checkpoint(
                cycle_id="c1",
                progress_summary="- c1",
                checklist=items,
                events=[EventRecord.model_validate_json(line) for line in lines],
            )
        )
        return (Path(workdir) / ".lha" / "events.ndjson").read_text(encoding="utf-8")

    with tempfile.TemporaryDirectory() as workdir:
        return asyncio.run(run(workdir))


def export_wire_bytes() -> None:
    from lha.coordination.leases import LeaseDecision, lease_event
    from lha.execution.egress_events import parse_proxy_log

    events = [EventRecord.model_validate(e).model_dump_json() for e in _WIRE_EVENTS]
    leases = [
        {
            "writer": "implementer-02",
            "path": "b.py",
            "reason": "need it",
            "granted": True,
            "previous_owner": None,
            "why": "unowned",
        },
        {
            "writer": "implementer-01",
            "path": "a.py",
            "reason": "x",
            "granted": False,
            "previous_owner": "implementer-03",
            "why": "owned by 'implementer-03'",
        },
    ]
    _write(
        "state/wire_bytes.json",
        {
            "events": events,
            "anchor_events_file": _wire_anchor_file(events),
            "lease_events": [
                {"decision": d, "line": lease_event(LeaseDecision(**d), "c9").model_dump_json()}
                for d in leases
            ],
            "egress_events": [e.model_dump_json() for e in parse_proxy_log(_PROXY_LOG)],
            "ownership": [{**c, "json": _wire_ownership(c)} for c in _WIRE_OWNERSHIP],
            "gates": [{"notice": c, **_wire_gate(c)} for c in _WIRE_GATES],
        },
    )


def export_system_one() -> None:
    """spec/systemone/wire.json: System One request bodies, strict answer parsing, confidence,
    the stall-triage question and action table, reranking, endpoints and default prices."""
    from lha.contracts.state import ChecklistItem as _Item
    from lha.contracts.system_one import ChoiceAnswer, SystemOneError
    from lha.memory.rerank import apply_relevance, system_one_rerank_request
    from lha.safety.egress import parse_url
    from lha.systemone import triage
    from lha.systemone.client import default_price_in_per_mtok, is_local_endpoint
    from lha.systemone.wire import (
        MAX_CHOICE_OPTIONS,
        MAX_SCORE_LEVELS,
        MIN_SCORE_LEVELS,
        SUM_TOLERANCE,
        choice_confidence,
        parse_response,
        question_body,
        question_from_wire,
        request_body,
        score_confidence,
    )

    noul = {"type": "noul", "instructions": "Is it urgent?"}
    noul_c = {
        "type": "noul",
        "instructions": {"record": {"name": "Ann"}, "question": "Is `record` a person?"},
        "criteria": {"true": "A named person", "false": "Anything else"},
    }
    choice = {
        "type": "choice",
        "instructions": "Which team?",
        "criteria": {"billing": "Payments", "shipping": None, "déjà": ["a", 1]},
    }
    score = {"type": "score", "instructions": "How upset?", "criteria": ["calm", "upset", "angry"]}
    request_cases: list[tuple[str, object, dict[str, Any]]] = [
        ("jev-1.13.0", "Help! Payouts failing.", {"urgent": noul}),
        (
            "kev-latest",
            {"ticket": {"id": 7, "tags": ["a", None, True, 1.5]}},
            {"a": noul_c, "b": choice, "c": score},
        ),
        ("m", ["x", "y"], {}),
        ("m", "s", {"q": {**choice, "criteria": {}}}),
        ("m", "s", {"q": {**choice, "criteria": {str(i): None for i in range(256)}}}),
        ("m", "s", {"q": {**choice, "criteria": {str(i): None for i in range(255)}}}),
        ("m", "s", {"q": {**score, "criteria": ["one"]}}),
        ("m", "s", {"q": {**score, "criteria": [str(i) for i in range(11)]}}),
        ("m", "s", {"q": {**score, "criteria": [str(i) for i in range(10)]}}),
    ]
    requests = []
    for model, state, qs in request_cases:
        questions = {qid: question_from_wire(q) for qid, q in qs.items()}
        try:
            body: object = request_body(model, state, questions)
            error = False
        except SystemOneError:
            body, error = None, True
        requests.append(
            {"model": model, "state": state, "questions": qs, "body": body, "error": error}
        )

    asked = {
        "u": noul,
        "t": {**choice, "criteria": {"billing": None, "shipping": None}},
        "s": score,
    }
    good = {
        "u": {"type": "noul", "noul": 0.95},
        "t": {
            "type": "choice",
            "choice": "billing",
            "probabilities": {"billing": 0.88, "shipping": 0.12},
            "confidence": 0.76,
        },
        "s": {
            "type": "score",
            "score": 1.05,
            "legend": {"0": "calm"},
            "probabilities": {"0": 0.0, "1": 0.95, "2": 0.05},
            "confidence": 0.92,
        },
    }
    variants: list[object] = [
        {
            "model": "jev-1.13.0",
            "answers": good,
            "usage": {"input_tokens": 296, "output_tokens": 20},
        },
        {"answers": {**good, "extra": {"type": "noul", "noul": 2}}},
        {"model": "x", "answers": good, "usage": None},
        {"answers": {**good, "u": {"type": "noul", "noul": 0}}},
        {"answers": {**good, "u": {"type": "noul", "noul": 1}}},
        {
            "answers": {
                **good,
                "t": {**good["t"], "probabilities": {"billing": 0.96, "shipping": 0.0}},
            }
        },
        [],
        {"answers": []},
        {"model": 3, "answers": good},
        {"answers": good, "usage": []},
        {"answers": good, "usage": {"input_tokens": -1}},
        {"answers": good, "usage": {"input_tokens": 1.5}},
        {"answers": good, "usage": {"output_tokens": True}},
        {"answers": {k: v for k, v in good.items() if k != "s"}},
        {"answers": {**good, "u": "yes"}},
        {"answers": {**good, "u": {"type": "choice", "noul": 0.5}}},
        {"answers": {**good, "u": {"type": "noul", "noul": 1.01}}},
        {"answers": {**good, "u": {"type": "noul", "noul": "0.5"}}},
        {"answers": {**good, "u": {"type": "noul", "noul": False}}},
        {"answers": {**good, "u": {"type": "noul"}}},
        {"answers": {**good, "t": {**good["t"], "choice": "returns"}}},
        {"answers": {**good, "t": {**good["t"], "choice": None}}},
        {"answers": {**good, "t": {**good["t"], "probabilities": {"billing": 1.0}}}},
        {
            "answers": {
                **good,
                "t": {**good["t"], "probabilities": {"billing": 0.5, "shipping": 0.2}},
            }
        },
        {"answers": {**good, "t": {**good["t"], "probabilities": []}}},
        {"answers": {**good, "t": {**good["t"], "confidence": 1.2}}},
        {"answers": {**good, "s": {**good["s"], "score": 2.01}}},
        {"answers": {**good, "s": {**good["s"], "score": -0.01}}},
        {"answers": {**good, "s": {**good["s"], "probabilities": {"0": 0.5, "1": 0.5}}}},
        {"answers": {**good, "s": {**good["s"], "confidence": None}}},
    ]
    parsed_questions = {qid: question_from_wire(q) for qid, q in asked.items()}
    responses = []
    for data in variants:
        try:
            result: object = parse_response(data, parsed_questions).model_dump()
            error = False
        except SystemOneError:
            result, error = None, True
        responses.append({"response": data, "result": result, "error": error})

    choice_probs = [
        [1.0],
        [0.5, 0.5],
        [0.88, 0.12],
        [0.47, 0.28, 0.25],
        [0.1, 0.1, 0.8],
        [0.25] * 4,
        [0.0, 1.0, 0.0],
    ]
    score_probs = [
        [1.0],
        [0.0, 0.95, 0.05],
        [0.0, 0.56, 0.44],
        [1 / 3] * 3,
        [0.5, 0.0, 0.5],
        [0.1, 0.2, 0.3, 0.4],
        [0.0, 0.0, 1.0],
        [0.5, 0.5],
    ]

    def answer(probs: dict[str, float], confidence: float) -> ChoiceAnswer:
        return ChoiceAnswer(
            choice=max(probs, key=lambda k: probs[k]), probabilities=probs, confidence=confidence
        )

    action_cases = [
        (choice_, conf, threshold, can_split)
        for choice_ in triage.CAUSES
        for conf in (0.5, 0.89, 0.9, 0.99)
        for threshold in (0.9, 0.0)
        for can_split in (True, False)
    ]
    actions = []
    for choice_, conf, threshold, can_split in action_cases:
        probs = {k: (0.9 if k == choice_ else 0.05) for k in triage.CAUSES}
        actions.append(
            {
                "choice": choice_,
                "confidence": conf,
                "threshold": threshold,
                "can_split": can_split,
                "action": triage.triage_action(
                    answer(probs, conf), threshold=threshold, can_split=can_split
                ),
            }
        )
    long_failure = "E   AssertionError: expected 3\n" * 200 + "key sk-ant-api03-" + "A" * 40
    state_cases = [
        ({"id": "01", "description": "Add greet", "witnesses": ["go:TestHello"]}, "boom", "", 3000),
        (
            {"id": "02", "description": "token ghp_" + "b" * 36, "witnesses": []},
            long_failure,
            "déjà vu 🐟",
            100,
        ),
        ({"id": "03", "description": "x"}, "", "", 5),
    ]
    states = [
        {
            "item": item,
            "latest": latest,
            "previous": previous,
            "max_chars": limit,
            "state": triage.triage_state(_Item(**item), latest, previous, max_chars=limit),
        }
        for item, latest, previous, limit in state_cases
    ]

    from lha.contracts.memory import MemoryRecord, RetrievalHit

    def hits(texts: list[str]) -> list[RetrievalHit]:
        return [
            RetrievalHit(record=MemoryRecord(id=f"r{i}", kind="semantic", text=t), score=0.1)
            for i, t in enumerate(texts)
        ]

    rerank_requests = []
    for query, texts in [
        ("fix the parser", ["the parser fails on tabs", "unrelated"]),
        ("q Bearer abcdefghijklmnopqrstuvwxyz0123", ["x" * 2000 + "tail"]),
    ]:
        state, questions = system_one_rerank_request(query, hits(texts))
        rerank_requests.append(
            {
                "query": query,
                "texts": texts,
                "state": state,
                "questions": {qid: question_body(q) for qid, q in questions.items()},
            }
        )
    apply_cases = [
        ([0.2, 0.9, 0.9, 0.05], 2, 0.1),
        ([0.2, 0.9, 0.9, 0.05], None, 0.1),
        ([0.2, 0.9, 0.9, 0.05], None, 0.0),
        ([0.5, 0.5, 0.5], 1, 0.5),
        ([0.0], None, 0.0),
        ([0.3, 0.1], 0, 0.0),
    ]
    applied = []
    for relevance, k, min_p in apply_cases:
        ranked = apply_relevance(
            hits([f"t{i}" for i in range(len(relevance))]), relevance, k=k, min_p=min_p
        )
        applied.append(
            {
                "relevance": relevance,
                "k": k,
                "min_p": min_p,
                "order": [int(h.record.id[1:]) for h in ranked],
                "scores": [h.score for h in ranked],
            }
        )
    endpoints = [
        "https://api.typesafe.ai/v1/systemone",
        "https://API.TypeSafe.ai:443/v1/systemone",
        "http://127.0.0.1:8009/v1/systemone",
        "http://localhost:8009/v1/systemone",
        "http://kev.localhost/v1/systemone",
        "http://[::1]:8009/v1/systemone",
        "https://kev.example.com/v1/systemone",
        "http://10.0.0.2/v1/systemone",
    ]
    _write(
        "systemone/wire.json",
        {
            "limits": {
                "max_choice_options": MAX_CHOICE_OPTIONS,
                "min_score_levels": MIN_SCORE_LEVELS,
                "max_score_levels": MAX_SCORE_LEVELS,
                "sum_tolerance": SUM_TOLERANCE,
            },
            "requests": requests,
            "questions": asked,
            "responses": responses,
            "choice_confidence": [
                {"probabilities": p, "confidence": choice_confidence(p)} for p in choice_probs
            ],
            "score_confidence": [
                {"probabilities": p, "confidence": score_confidence(p)} for p in score_probs
            ],
            "triage": {
                "question_id": triage.QUESTION_ID,
                "question": question_body(triage.QUESTION),
                "max_failure_chars": triage.MAX_FAILURE_CHARS,
                "actions": actions,
                "states": states,
            },
            "rerank": {"requests": rerank_requests, "apply": applied},
            "endpoints": [
                {
                    "endpoint": e,
                    "local": is_local_endpoint(parse_url(e)),
                    "default_price_in_per_mtok": default_price_in_per_mtok(e),
                }
                for e in endpoints
            ],
        },
    )


def export_code_query() -> None:
    """spec/execution/code_query.json: the code_query tool's spec, the ripwire command for each
    kind of question, which questions are refused, and how answers are clipped."""
    from lha.execution.tools.code_query import (
        KINDS,
        MAX_ANSWER_CHARS,
        MAX_FIND_CHARS,
        MAX_SYMBOL_CHARS,
        CodeQueryTool,
        clip_answer,
        code_query_argv,
    )

    questions = [
        ("find", " refuse overlong URLs ", 1500),
        ("find", "two\nlines déjà 🐟", 900),
        ("find", "x" * MAX_FIND_CHARS, 200),
        ("find", "x" * (MAX_FIND_CHARS + 1), 200),
        ("definition", "redact_text", 1500),
        ("callers", "redact_text", 1500),
        ("uses", "src/a.py:Thing", 1500),
        ("impact", "-rf", 1500),
        ("impact", "x" * MAX_SYMBOL_CHARS, 1500),
        ("impact", "x" * (MAX_SYMBOL_CHARS + 1), 1500),
        ("callers", "   ", 1500),
        ("callers", "a\nb", 1500),
        ("callers", "a\x00b", 1500),
        ("grep", "x", 1500),
        ("", "x", 1500),
    ]
    cases = []
    for kind, target, budget in questions:
        try:
            argv: object = code_query_argv(kind, target, budget)
            error: object = None
        except ValueError as exc:
            argv, error = None, str(exc)
        cases.append(
            {"kind": kind, "target": target, "budget": budget, "argv": argv, "error": error}
        )
    _write(
        "execution/code_query.json",
        {
            "kinds": KINDS,
            "max_symbol_chars": MAX_SYMBOL_CHARS,
            "max_find_chars": MAX_FIND_CHARS,
            "max_answer_chars": MAX_ANSWER_CHARS,
            "spec": CodeQueryTool.spec.model_dump(mode="json"),
            "questions": cases,
            "clip": [
                {"text": t, "clipped": clip_answer(t)}
                for t in ["", "short", "é" * MAX_ANSWER_CHARS, "é" * (MAX_ANSWER_CHARS + 3)]
            ],
        },
    )


def main() -> None:
    export_wire_bytes()
    export_memory()
    export_voyage()
    export_paths()
    export_arguments()
    export_sandbox_egress()
    export_classifier()
    export_egress()
    export_redact()
    export_check_names()
    export_checklist()
    export_decision_chain()
    export_ownership()
    export_harness_files()
    export_flaky_retry()
    export_pricing()
    export_fallback_models()
    export_vendor_paths()
    export_agent_prompts()
    export_agent_org()
    export_system_one()
    export_code_query()


if __name__ == "__main__":
    main()
