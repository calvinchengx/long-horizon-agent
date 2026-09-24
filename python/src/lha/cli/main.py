"""``lha`` command-line entry point.

Run paths (``run-local`` / ``mission`` / ``orchestrate`` / ``mission-start``) persist the mission
row and every metered model call to the mission store (SQLite by default, Postgres with
``LHA_POSTGRES_DSN``); ``missions`` and ``costs`` read it back.
"""

from __future__ import annotations

import shlex
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING, Any, NoReturn

import typer

from lha import __version__
from lha.config import Settings, get_settings

if TYPE_CHECKING:
    from lha.contracts.hitl import HITLGate
    from lha.durable.types import GateView
    from lha.persistence.store import MissionStore
    from lha.state.checklist_import import ImportedChecklist

app = typer.Typer(
    help="LHA — a durable, self-improving agent organization for long-horizon software missions.",
    no_args_is_help=True,
    add_completion=False,
)
db_app = typer.Typer(help="Database maintenance (Postgres).", no_args_is_help=True)
app.add_typer(db_app, name="db")


@app.callback()
def _startup(ctx: typer.Context) -> None:
    """Process start: install the trace exporter when one is configured (``lha.obs.otel``)."""
    from lha.obs.otel import configure_tracing

    configure_tracing(component="worker" if ctx.invoked_subcommand == "worker" else "cli")


_CHECK_HELP = (
    'A gating verification command, shell-quoted (repeatable), e.g. --check "uv run pytest -q". '
    "Added to the default Python checks (ruff, ty, pytest) unless --no-default-checks."
)
_NO_DEFAULT_CHECKS_HELP = (
    "Do not add the default Python checks; requires at least one --check "
    "(an item is never marked done without a gating check)."
)
_SANDBOX_HELP = "Execution sandbox: docker | e2b | local (default: LHA_SANDBOX, else docker)."
_UNSAFE_LOCAL_HELP = (
    "Allow the 'local' sandbox: agent commands run directly on this host with NO isolation."
)
_ALLOW_HOST_HELP = (
    "Add a host to this run's web allow-list (repeatable; added to LHA_WEB_ALLOW_HOSTS). A "
    "non-empty allow-list registers the web tools (fetch_url, and web_search when configured)."
)
_CHECKLIST_HELP = (
    "Seed the mission with your own checklist instead of planning: a .json checklist or a "
    ".md roadmap ('- [ ] item (witness: go:TestX)'; each '## ' section depends on the previous)."
)
_REFERENCE_HELP = (
    "A workspace-relative path of vendored reference material (repeatable; see 'lha vendor'), "
    "recited to the agent every cycle."
)
_APPROVE_HELP = (
    "Ask on this terminal (y/N, default reject) before any irreversible command (git push, "
    "publish, uploads); rejected without asking when stdin is not a TTY, and after "
    "LHA_CONSOLE_APPROVAL_TIMEOUT_S (default 1h) without an answer. Without this flag such "
    "commands are refused."
)


def _load_checklist_file(path: str) -> ImportedChecklist:
    from lha.state.checklist_import import ChecklistImportError, load_checklist

    try:
        return load_checklist(path)
    except (OSError, ChecklistImportError) as exc:
        _fail(f"cannot import checklist {path!r}: {exc}")


def _merge_references(references: list[str], extra: list[str]) -> list[str]:
    return [*references, *(r for r in extra if r not in references)]


def _gate(approve_interactive: bool, settings: Settings | None = None) -> HITLGate | None:
    if not approve_interactive:
        return None
    from lha.hitl.approvals import console_gate

    return console_gate(settings)


# Optional third-party modules -> the pip extra / package that provides them.
_OPTIONAL_MODULES = {
    "docker": "the 'sandbox' extra (lha[sandbox])",
    "e2b_code_interpreter": "the 'e2b-code-interpreter' package",
    "psycopg": "the 'postgres' extra (lha[postgres])",
}


def _fail(message: str, code: int = 2) -> NoReturn:
    typer.echo(f"error: {message}", err=True)
    raise typer.Exit(code)


def resolve_check_commands(check: list[str], no_default_checks: bool) -> list[list[str]]:
    """``--check`` values (shlex-split) plus the default Python checks unless opted out."""
    from lha.verify.verifier import DEFAULT_PYTHON_CHECK_COMMANDS

    try:
        extra = [shlex.split(c) for c in check]
    except ValueError as exc:
        _fail(f"invalid --check value: {exc}")
    extra = [cmd for cmd in extra if cmd]
    if no_default_checks and not extra:
        _fail("--no-default-checks requires at least one non-empty --check")
    defaults = [] if no_default_checks else [list(c) for c in DEFAULT_PYTHON_CHECK_COMMANDS]
    return defaults + extra


def _run_settings(
    sandbox: str | None, unsafe_local: bool, allow_host: list[str] | None = None
) -> Settings:
    """Settings with the CLI's sandbox / web overrides; refuses an unsafe local sandbox, a
    lethal-trifecta run (Rule of Two) and invalid web settings up front (before any planning
    spend or workspace writes)."""
    from lha.execution.factory import SANDBOX_KINDS, UnsafeSandboxError, build_sandbox
    from lha.execution.tools.toolset import preflight_run_tools, with_allow_hosts
    from lha.safety.rule_of_two import RuleOfTwoViolation

    settings = get_settings()
    update: dict[str, object] = {}
    if sandbox is not None:
        kind = sandbox.strip().lower()
        if kind not in SANDBOX_KINDS:
            _fail(f"unknown --sandbox {sandbox!r}; expected one of {', '.join(SANDBOX_KINDS)}")
        update["sandbox"] = kind
    if unsafe_local:
        update["allow_unsafe_local"] = True
    if update:
        settings = settings.model_copy(update=update)
    settings = with_allow_hosts(settings, allow_host or [])
    if settings.sandbox == "local":
        try:
            build_sandbox("local", allow_unsafe_local=settings.allow_unsafe_local)
        except UnsafeSandboxError as exc:
            _fail(f"{exc} (or pass --unsafe-local)")
    try:
        preflight_run_tools(settings)
    except RuleOfTwoViolation as exc:
        _fail(str(exc))
    except ValueError as exc:  # WebConfigError, bad LHA_WEB_ALLOW_PORTS
        _fail(f"invalid web settings: {exc}")
    return settings


def _run[T](coro: Awaitable[T]) -> T:
    """Run a coroutine, turning expected operator errors into clean CLI errors."""
    import asyncio

    from lha.execution.factory import UnsafeSandboxError
    from lha.governor.metering import BudgetExceeded
    from lha.persistence.store import StoreUnavailableError
    from lha.safety.rule_of_two import RuleOfTwoViolation

    async def _main() -> T:
        return await coro

    try:
        return asyncio.run(_main())
    except (UnsafeSandboxError, StoreUnavailableError, RuleOfTwoViolation) as exc:
        _fail(str(exc))
    except BudgetExceeded as exc:
        _fail(str(exc), code=3)
    except ModuleNotFoundError as exc:
        _fail_missing_module(exc)


def _fail_missing_module(exc: ModuleNotFoundError) -> NoReturn:
    root = (exc.name or "").split(".")[0]
    if root not in _OPTIONAL_MODULES:
        raise exc
    _fail(f"python module {root!r} is not installed; install {_OPTIONAL_MODULES[root]}")


def _report(summary: object) -> None:
    from lha.agent.runner import MissionSummary

    assert isinstance(summary, MissionSummary)
    typer.echo(f"mission {summary.mission_id}: {summary.stopped_reason}")
    typer.echo(
        f"items {summary.items_done}/{summary.items_total}  "
        f"cycles {summary.cycles}  cost ${summary.total_usd:.4f}"
    )
    typer.echo(f"head {summary.head_sha or '(none)'}")
    if not summary.completed:
        raise typer.Exit(1)


@app.command()
def version() -> None:
    """Print the installed LHA version."""
    typer.echo(f"lha {__version__}")


@app.command()
def config() -> None:
    """Show the resolved runtime configuration (secrets redacted), and where the store is."""
    from lha.persistence.store import describe_store

    settings = get_settings()
    for key, value in settings.redacted().items():
        typer.echo(f"{key} = {value}")
    typer.echo(f"mission store = {describe_store(settings)}")


@db_app.command()
def migrate(
    migrations_dir: str | None = typer.Option(
        None,
        help="Directory of *.sql migrations (default: db/migrations here or in the parent dir).",
    ),
) -> None:
    """Apply the SQL migrations to the Postgres database at LHA_POSTGRES_DSN."""
    from pathlib import Path

    if migrations_dir is None:
        found = next((d for d in ("db/migrations", "../db/migrations") if Path(d).is_dir()), None)
        if found is None:
            _fail("cannot find db/migrations here or in the parent dir; pass --migrations-dir")
        migrations_dir = found
    from lha.model import secret_value

    dsn = secret_value(get_settings().postgres_dsn)
    if not dsn:
        _fail("LHA_POSTGRES_DSN is not set; export it (or put it in .env) to run migrations")
    try:
        from lha.persistence.db import apply_migrations
    except ModuleNotFoundError as exc:
        _fail_missing_module(exc)

    result = _run(apply_migrations(dsn, migrations_dir=migrations_dir))
    typer.echo(f"migrations applied: {result}")


@app.command()
def vendor(
    url: list[str] = typer.Argument(..., help="URL(s) of reference pages to snapshot."),
    into: str = typer.Option(
        "reference", help="Directory (inside the mission workspace) to store them in."
    ),
) -> None:
    """Snapshot reference material into the workspace, with a SHA-256 manifest.

    Pass the directory to missions with --reference so the agent reads it offline.
    """
    from lha.safety.egress import EgressDenied
    from lha.state.vendor import VendorError, vendor_urls

    try:
        saved = _run(vendor_urls(list(url), into))
    except (VendorError, EgressDenied) as exc:
        _fail(str(exc))
    for item in saved:
        typer.echo(
            f"{item.url} -> {into}/{item.path} ({item.bytes} bytes, sha256 {item.sha256[:12]})"
        )
    typer.echo(f"manifest: {into}/MANIFEST.json")


async def _with_store[T](fn: Callable[[MissionStore], Awaitable[T]]) -> T:
    from lha.persistence.store import open_store

    store = await open_store(get_settings())
    try:
        if store.degraded_reason:
            typer.echo(f"warning: {store.degraded_reason}; reading SQLite instead", err=True)
        return await fn(store)
    finally:
        await store.close()


def _usd(value: float | None) -> str:
    return "unknown" if value is None else f"${value:.4f}"


@app.command()
def missions(
    limit: int = typer.Option(20, min=1, help="How many missions (most recently updated first)."),
) -> None:
    """List persisted missions with their status and recorded spend.

    Reads the mission store: Postgres when LHA_POSTGRES_DSN is set, else the SQLite file at
    LHA_SQLITE_PATH (default: one per-user file, e.g. ~/.local/share/lha/lha.sqlite3 or
    ~/Library/Application Support/lha/lha.sqlite3). `lha config` prints the resolved location.
    """
    from lha.persistence.store import CostSummary, MissionRow

    async def _read(store: MissionStore) -> list[tuple[MissionRow, CostSummary]]:
        rows = await store.list_missions(limit=limit)
        return [(row, await store.cost_summary(row.mission_id)) for row in rows]

    rows = _run(_with_store(_read))
    if not rows:
        typer.echo("no missions recorded")
        return
    for row, cost in rows:
        unknown = f" (+{cost.unknown_cost_calls} unknown-cost)" if cost.unknown_cost_calls else ""
        typer.echo(
            f"{row.mission_id}  {row.status:<16} {_usd(cost.known_usd)}{unknown}  "
            f"calls {cost.calls}  head {(row.head_sha or '-')[:12]}  "
            f"updated {row.updated_at[:19]}  {row.title}"
        )


@app.command()
def costs(
    mission_id: str = typer.Argument(..., help="Mission id."),
    limit: int = typer.Option(50, min=0, help="Show the most recent N calls (0 = summary only)."),
) -> None:
    """Show a mission's persisted cost ledger: every metered model call, plus totals.

    Reads the same mission store as `lha missions` (see `lha config` for its location).
    """
    from lha.persistence.store import CostRow, CostSummary

    async def _read(store: MissionStore) -> tuple[CostSummary, list[CostRow]]:
        rows = await store.list_costs(mission_id, limit=limit) if limit else []
        return await store.cost_summary(mission_id), rows

    summary, rows = _run(_with_store(_read))
    if not summary.calls:
        _fail(f"no cost ledger rows for mission {mission_id}", code=1)
    for row in rows:
        typer.echo(
            f"{row.ts[:19]}  {row.cycle_id:<12} {row.role or '-':<11} {row.model:<24} "
            f"in {row.input_tokens:>7}  out {row.output_tokens:>6}  {_usd(row.usd)}"
        )
    typer.echo(
        f"total: {summary.calls} calls  known {_usd(summary.known_usd)}  "
        f"unknown-cost calls {summary.unknown_cost_calls}  "
        f"tokens in {summary.input_tokens} out {summary.output_tokens}"
    )


@app.command(name="run-local")
def run_local(
    title: str | None = typer.Option(None, help="Mission title (default: the checklist's)."),
    item: list[str] = typer.Option([], "--item", help="A checklist item description (repeatable)."),
    checklist_file: str | None = typer.Option(None, "--checklist", help=_CHECKLIST_HELP),
    workdir: str = typer.Option(
        ".lha/workspaces/local", help="Workspace dir (becomes a git repo)."
    ),
    description: str = typer.Option("", help="Mission description."),
    reference: list[str] = typer.Option([], "--reference", help=_REFERENCE_HELP),
    approve_interactive: bool = typer.Option(False, "--approve-interactive", help=_APPROVE_HELP),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
    allow_host: list[str] = typer.Option([], "--allow-host", help=_ALLOW_HOST_HELP),
) -> None:
    """Run a mission locally (no Temporal) until complete / deadlocked / over-budget."""
    from lha.agent.runner import run_mission_local
    from lha.contracts.state import Checklist, ChecklistItem
    from lha.contracts.verify import checks_from_commands

    if bool(item) == bool(checklist_file):
        _fail("give either --item (repeatable) or --checklist FILE")
    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local, allow_host)
    references = list(reference)
    if checklist_file:
        imported = _load_checklist_file(checklist_file)
        checklist = imported.checklist
        title = title or imported.title
        description = description or imported.description
        references = _merge_references(references, imported.references)
    else:
        checklist = Checklist(
            items=[
                ChecklistItem(id=f"{i + 1:02d}", description=desc) for i, desc in enumerate(item)
            ]
        )
    summary = _run(
        run_mission_local(
            workdir=workdir,
            title=title or "mission",
            description=description,
            checklist=checklist,
            checks=checks,
            settings=settings,
            gate=_gate(approve_interactive, settings),
            references=references,
        )
    )
    _report(summary)


@app.command()
def mission(
    task: str = typer.Option(
        "", help="The mission / task description (the Planner decomposes it)."
    ),
    title: str | None = typer.Option(None, help="Mission title (default: 'mission')."),
    checklist_file: str | None = typer.Option(None, "--checklist", help=_CHECKLIST_HELP),
    reference: list[str] = typer.Option([], "--reference", help=_REFERENCE_HELP),
    approve_interactive: bool = typer.Option(False, "--approve-interactive", help=_APPROVE_HELP),
    workdir: str = typer.Option(
        ".lha/workspaces/mission", help="Workspace dir (becomes a git repo)."
    ),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
    allow_host: list[str] = typer.Option([], "--allow-host", help=_ALLOW_HOST_HELP),
) -> None:
    """Plan a task into a checklist (or import one), then run it locally to completion."""
    from lha.agent.runner import plan_and_run_local, run_mission_local
    from lha.contracts.verify import checks_from_commands

    if not task.strip() and not checklist_file:
        _fail("give --task (to plan) or --checklist FILE (to import a checklist)")
    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local, allow_host)
    gate = _gate(approve_interactive, settings)
    if checklist_file:
        imported = _load_checklist_file(checklist_file)
        summary = _run(
            run_mission_local(
                workdir=workdir,
                title=title or imported.title or "mission",
                description=task or imported.description,
                checklist=imported.checklist,
                checks=checks,
                settings=settings,
                gate=gate,
                references=_merge_references(list(reference), imported.references),
            )
        )
    else:
        summary = _run(
            plan_and_run_local(
                workdir=workdir,
                title=title or "mission",
                task=task,
                checks=checks,
                settings=settings,
                gate=gate,
                references=list(reference),
            )
        )
    _report(summary)


@app.command()
def orchestrate(
    task: str = typer.Option(..., help="The mission / task description."),
    title: str = typer.Option("mission", help="Mission title."),
    reference: list[str] = typer.Option([], "--reference", help=_REFERENCE_HELP),
    approve_interactive: bool = typer.Option(False, "--approve-interactive", help=_APPROVE_HELP),
    workdir: str = typer.Option(".lha/workspaces/org", help="Workspace dir (becomes a git repo)."),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
    allow_host: list[str] = typer.Option([], "--allow-host", help=_ALLOW_HOST_HELP),
) -> None:
    """Plan, then run the FULL multi-agent org (research, Lead or parallel waves, review) locally."""
    from lha.agent.runner import MissionSummary, aclose_provider, build_meter
    from lha.agents.orchestrator import Orchestrator
    from lha.agents.planner import Planner
    from lha.contracts.verify import checks_from_commands
    from lha.model import build_provider

    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local, allow_host)

    async def _mission() -> MissionSummary:
        meter = build_meter(settings)  # planner + every org role share one budget
        planner_model = build_provider(settings)
        try:
            planner = Planner(meter.wrap(planner_model, role="planner"))
            plan = await planner.plan_mission(title=title, description=task)
        finally:
            await aclose_provider(planner_model)
        return await Orchestrator(settings, meter=meter).run_mission(
            workdir=workdir,
            title=title,
            description=task,
            checklist=plan.checklist,
            checks=checks,
            gate=_gate(approve_interactive, settings),
            references=list(reference),
            ownership=plan.ownership,
        )

    _report(_run(_mission()))


@app.command()
def decisions(
    workdir: str = typer.Option(".", help="The mission workspace (the git repo holding .lha/)."),
    verify: bool = typer.Option(
        False, "--verify", help="Verify the hash chain; exit 1 if it does not verify."
    ),
    limit: int = typer.Option(0, help="Print only the newest N decisions (0 = all)."),
) -> None:
    """Print the mission's committed design decisions (.lha/decisions.ndjson), or verify them."""
    from pathlib import Path

    from lha.state.mission_anchor import ANCHOR_DIR, DECISIONS_FILE, GitMissionAnchor

    if not (Path(workdir) / ANCHOR_DIR).is_dir():
        _fail(f"no mission anchor at {workdir!r} (expected a {ANCHOR_DIR}/ directory)")
    anchor = GitMissionAnchor(workdir)
    check = _run(anchor.verify_decisions())
    if verify:
        if not check.ok:
            typer.echo(f"decision chain BROKEN: {check.problem}")
            raise typer.Exit(1)
        chained = check.checked - check.legacy
        typer.echo(f"decision chain OK: {check.checked} record(s), {chained} chained")
        if check.legacy:
            sealed = "sealed by the chain" if chained else "NOT protected until one is chained"
            typer.echo(f"  {check.legacy} legacy (pre-chain) record(s), {sealed}")
        return
    if not check.ok:
        _fail(
            f"{ANCHOR_DIR}/{DECISIONS_FILE} failed verification ({check.problem}); "
            "run `lha decisions --verify`",
            code=1,
        )
    records = _run(anchor.read_decisions())
    shown = records[-limit:] if limit > 0 else records
    if not shown:
        typer.echo("(no decisions recorded)")
    first = len(records) - len(shown) + 1
    for number, record in enumerate(shown, start=first):
        cycle = f" [{record.cycle_id}]" if record.cycle_id else ""
        typer.echo(f"{number}.{cycle} {record.decision}")
        typer.echo(f"   why: {record.rationale}")
        if record.alternatives_rejected:
            typer.echo(f"   rejected: {record.alternatives_rejected}")
        if record.affected:
            typer.echo(f"   affects: {', '.join(record.affected)}")


@app.command()
def worker() -> None:
    """Run a Temporal worker that serves missions (requires a Temporal server)."""
    import asyncio

    from lha.durable.worker import run_worker

    asyncio.run(run_worker())


@app.command(name="mission-start")
def mission_start(
    task: str = typer.Option(
        "", help="The mission / task description (the Planner decomposes it)."
    ),
    title: str | None = typer.Option(None, help="Mission title (default: 'mission')."),
    checklist_file: str | None = typer.Option(None, "--checklist", help=_CHECKLIST_HELP),
    reference: list[str] = typer.Option([], "--reference", help=_REFERENCE_HELP),
    workdir: str = typer.Option(
        ".lha/workspaces/durable",
        help="Workspace dir (becomes a git repo); resolved to an absolute path for the worker.",
    ),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    deadlock_gate_hours: float = typer.Option(
        24.0,
        help="On deadlock, wait this long for a human 'retry', 'abort' or 'impossible' (0 = end "
        "immediately).",
    ),
    approval_timeout_hours: float | None = typer.Option(
        None,
        help=(
            "How long an irreversible action waits for approval before it is rejected "
            "(default: LHA_APPROVAL_TIMEOUT_S, 24h)."
        ),
    ),
    max_cycles: int | None = typer.Option(None, help="Cycle ceiling (default: LHA_MAX_CYCLES)."),
    deadlock_default: str | None = typer.Option(
        None,
        "--deadlock-default",
        help="Deadlock gate decision when nobody answers: abort | impossible "
        "(default: LHA_DEADLOCK_GATE_DEFAULT, else abort).",
    ),
    cycle_pause_seconds: int | None = typer.Option(
        None,
        "--cycle-pause-seconds",
        min=0,
        help="Durable pause between cycles (status SLEEPING). Default: LHA_CYCLE_PAUSE_SECONDS.",
    ),
    start_in_seconds: int = typer.Option(
        0, "--start-in-seconds", min=0, help="Sleep (status SLEEPING) before the first cycle."
    ),
) -> None:
    """Plan (or import) a checklist, initialize the anchor, and start a durable MissionWorkflow."""
    import os
    import time

    from lha.agent.runner import aclose_provider, build_meter
    from lha.agents.planner import Planner
    from lha.config import get_settings
    from lha.durable.signals import (
        DEADLOCK_DEFAULTS,
        STATUS_ABORTED,
        STATUS_RUNNING,
        STATUS_SLEEPING,
    )
    from lha.durable.types import MissionInput
    from lha.durable.worker import connect_client
    from lha.durable.workflows import MissionWorkflow
    from lha.governor.cost import CostEntry
    from lha.ids import new_id
    from lha.model import build_provider
    from lha.persistence.store import open_store
    from lha.persistence.tracking import LedgerSink, MissionTracker
    from lha.state.mission_anchor import GitMissionAnchor

    if not task.strip() and not checklist_file:
        _fail("give --task (to plan) or --checklist FILE (to import a checklist)")
    check_commands = resolve_check_commands(check, no_default_checks)
    imported = _load_checklist_file(checklist_file) if checklist_file else None
    workdir = os.path.abspath(workdir)  # the worker may run from another directory
    gate_settings = get_settings()
    default_choice = (deadlock_default or gate_settings.deadlock_gate_default).strip().lower()
    if default_choice not in DEADLOCK_DEFAULTS:
        _fail(f"--deadlock-default must be one of {', '.join(DEADLOCK_DEFAULTS)}")
    pause = (
        gate_settings.cycle_pause_seconds if cycle_pause_seconds is None else cycle_pause_seconds
    )
    resume_at = time.time() + start_in_seconds if start_in_seconds > 0 else 0.0

    async def _start() -> str:
        settings = get_settings()
        references = list(reference)
        planner_spend: list[CostEntry] = []
        if imported is not None:
            checklist = imported.checklist
            mission_title = title or imported.title or "mission"
            description = task or imported.description
            references = _merge_references(references, imported.references)
        else:
            mission_title, description = title or "mission", task
            meter = build_meter(settings)
            planner_model = build_provider(settings)
            try:
                planner = Planner(meter.wrap(planner_model, role="planner"))
                checklist = await planner.plan(title=mission_title, description=task)
            finally:
                await aclose_provider(planner_model)
            planner_spend = list(meter.ledger.entries)
        anchor = GitMissionAnchor(workdir)
        await anchor.initialize(
            title=mission_title, description=description, items=checklist, references=references
        )
        client = await connect_client(settings)
        mission_id = new_id("mission")
        # The mission row + the Planner's spend, written BEFORE the workflow starts so a status
        # the workflow records right away (e.g. SLEEPING for a scheduled start) is never
        # overwritten by this one. The cycle activities and the workflow record the rest.
        store = await open_store(settings, workdir=workdir)
        try:
            await LedgerSink(store, mission_id, key_prefix="planner").backfill(planner_spend)
            tracker = MissionTracker(
                store,
                mission_id,
                title=mission_title,
                description=description,
                workflow_id=f"mission:{mission_id}",
            )
            await tracker.set_status(STATUS_SLEEPING if resume_at else STATUS_RUNNING)
            try:
                await client.start_workflow(
                    MissionWorkflow.run,
                    MissionInput(
                        mission_id=mission_id,
                        workdir=workdir,
                        check_commands=check_commands,
                        max_cycles=max_cycles if max_cycles is not None else settings.max_cycles,
                        deadlock_gate_seconds=int(deadlock_gate_hours * 3600),
                        approval_timeout_seconds=(
                            int(approval_timeout_hours * 3600)
                            if approval_timeout_hours is not None
                            else settings.approval_timeout_s
                        ),
                        gate_escalation_seconds=list(settings.gate_escalation_seconds),
                        deadlock_gate_default=default_choice,
                        impossible_after_failures=settings.impossible_after_failures,
                        cycle_pause_seconds=pause,
                        resume_at=resume_at,
                    ),
                    id=f"mission:{mission_id}",
                    task_queue=settings.task_queue,
                )
            except BaseException:
                await tracker.set_status(STATUS_ABORTED)
                raise
        finally:
            await store.close()
        return mission_id

    mission_id = _run(_start())
    typer.echo(f"started mission {mission_id} (workflow id: mission:{mission_id})")


def format_gate(gate: GateView | None) -> list[str]:
    """Human-readable lines for the open gate (``[]`` when none is open)."""
    if gate is None:
        return []
    lines = [
        f"gate: {gate.kind} {gate.gate_id}",
        f"  question: {gate.question}",
        f"  options: {' | '.join(gate.options)}  (default on timeout: {gate.default_action})",
        f"  opened: {gate.opened_at}  deadline: {gate.deadline}",
        f"  reminders sent: {gate.escalations_sent}"
        + (f"  next reminder: {gate.next_escalation_at}" if gate.next_escalation_at else ""),
    ]
    if gate.recommended:
        lines.append(f"  recommended: {gate.recommended}")
    if gate.request is not None:
        lines += [
            f"  pending action: {gate.request.tool} {gate.request.arguments}",
            f"  reason: {gate.request.reason}",
            f"  fingerprint: {gate.request.fingerprint}",
        ]
    return lines


_ALL_DECISIONS = ("approve", "reject", "retry", "abort", "impossible")


def check_decision(gate: GateView | None, decision: str) -> str:
    """The normalized decision, validated against the open gate's options (``ValueError``).

    With no gate open, any known decision is accepted: the workflow holds it for the next gate.
    """
    choice = decision.strip().lower()
    options = list(gate.options) if gate is not None else list(_ALL_DECISIONS)
    if choice not in options:
        where = f"the open {gate.kind} gate" if gate is not None else "a mission gate"
        raise ValueError(
            f"unknown --decision {decision!r} for {where}; expected {', '.join(options)}"
        )
    return choice


async def _optional_query(query: Awaitable[Any]) -> Any:
    """The query's answer, or ``None`` when the workflow does not answer it (older workers)."""
    from temporalio.client import WorkflowQueryFailedError

    try:
        return await query
    except WorkflowQueryFailedError:
        return None


async def _query_gate(handle: Any) -> GateView | None:
    from lha.durable.signals import QUERY_GATE
    from lha.durable.types import GateView

    return await _optional_query(handle.query(QUERY_GATE, result_type=GateView))


@app.command(name="mission-status")
def mission_status(mission_id: str = typer.Argument(..., help="Mission id.")) -> None:
    """Query a mission's status, cycles, sleep, open gate (+ pending action) and gate events."""
    import asyncio
    from datetime import UTC, datetime

    from lha.config import get_settings
    from lha.durable.signals import QUERY_CYCLES, QUERY_GATE_LOG, QUERY_STATUS
    from lha.durable.worker import connect_client

    async def _run() -> list[str]:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        status = await handle.query(QUERY_STATUS)
        cycles = await handle.query(QUERY_CYCLES)
        lines = [f"status={status} cycles={cycles}"]
        gate = await _query_gate(handle)
        if gate is not None:
            lines += format_gate(gate)
        else:
            question = await _optional_query(handle.query("open_question"))
            if question:
                lines.append(f"waiting on: {question}")
        resume_at = await _optional_query(handle.query("resume_at"))
        if resume_at:
            lines.append(f"sleeping until {datetime.fromtimestamp(resume_at, UTC).isoformat()}")
        log = await _optional_query(handle.query(QUERY_GATE_LOG)) or []
        if log:
            lines.append("recent gate events:")
            lines += [f"  {line}" for line in log[-8:]]
        return lines

    for line in asyncio.run(_run()):
        typer.echo(line)


@app.command(name="mission-approve")
def mission_approve(
    mission_id: str = typer.Argument(..., help="Mission id."),
    decision: str = typer.Option(
        ...,
        help=(
            "approve | reject (an irreversible action), retry | abort | impossible (a deadlock); "
            "checked against the open gate - see 'lha mission-status'."
        ),
    ),
) -> None:
    """Resolve an open human gate on a mission with a decision."""
    if decision.strip().lower() not in _ALL_DECISIONS:
        _fail(f"unknown --decision {decision!r}; expected {', '.join(_ALL_DECISIONS)}")
    import asyncio

    from lha.config import get_settings
    from lha.durable.signals import SIGNAL_HUMAN_DECISION
    from lha.durable.worker import connect_client

    async def _run() -> str:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        gate = await _query_gate(handle)
        try:
            choice = check_decision(gate, decision)
        except ValueError as exc:
            for line in format_gate(gate):
                typer.echo(line, err=True)
            _fail(str(exc))
        await handle.signal(SIGNAL_HUMAN_DECISION, choice)
        return choice

    choice = asyncio.run(_run())
    typer.echo(f"sent decision '{choice}' to mission {mission_id}")


@app.command(name="mission-snooze")
def mission_snooze(
    mission_id: str = typer.Argument(..., help="Mission id."),
    seconds: int = typer.Option(
        ..., min=0, help="Sleep (status SLEEPING) this long before the next cycle; 0 wakes it."
    ),
) -> None:
    """Park a mission on a durable timer (SLEEPING) before its next cycle, or wake it."""
    import asyncio

    from lha.config import get_settings
    from lha.durable.signals import SIGNAL_SNOOZE
    from lha.durable.worker import connect_client

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.signal(SIGNAL_SNOOZE, seconds)

    asyncio.run(_run())
    typer.echo(f"mission {mission_id}: " + (f"snoozed {seconds}s" if seconds else "woken"))


@app.command(name="mission-abort")
def mission_abort(mission_id: str = typer.Argument(..., help="Mission id.")) -> None:
    """Cancel a running mission workflow."""
    import asyncio

    from lha.config import get_settings
    from lha.durable.worker import connect_client

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.cancel()

    asyncio.run(_run())
    typer.echo(f"cancelled mission {mission_id}")


if __name__ == "__main__":
    app()
