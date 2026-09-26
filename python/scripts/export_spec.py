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
    build_messages,
    corrective_message,
    render_memory_block,
    render_tools,
)
from lha.agents.planner import Planner, assign_ownership, parse_plan  # noqa: E402
from lha.agents.replanner import Replanner  # noqa: E402
from lha.contracts.model import ModelMessage, TurnResult, Usage  # noqa: E402
from lha.contracts.state import (  # noqa: E402
    Checklist,
    ChecklistItem,
    DecisionRecord,
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
from lha.coordination.ownership import is_shared  # noqa: E402
from lha.execution.tools import default_local_tools  # noqa: E402
from lha.execution.tools.decisions import DecisionBuffer, RecordDecisionTool  # noqa: E402
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
        )
        out.append(
            {
                "name": case["name"],
                "anchor_text": case.get("anchor_text", ""),
                "mission_text": case.get("mission_text", ""),
                "memory_text": case.get("memory_text", ""),
                "engine": case.get("engine", False),
                "snapshot": snapshot.model_dump(mode="json"),
                "item": item.model_dump(mode="json"),
                "specs": [s.model_dump(mode="json") for s in specs],
                "messages": [{"role": m.role, "content": m.content} for m in messages],
            }
        )
    return out


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


def main() -> None:
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


if __name__ == "__main__":
    main()
