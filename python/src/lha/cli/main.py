"""``lha`` command-line entry point.

Run paths (``run-local`` / ``mission`` / ``orchestrate`` / ``mission-start``) persist the mission
row and every metered model call to the mission store (SQLite by default, Postgres with
``LHA_POSTGRES_DSN``); ``missions``, ``costs`` and ``gates`` read it back.
"""

from __future__ import annotations

import shlex
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import TYPE_CHECKING, Any, NoReturn

import typer

from lha import __version__
from lha.config import Settings, get_settings

if TYPE_CHECKING:
    from lha.contracts.hitl import HITLGate
    from lha.durable.types import GateView
    from lha.persistence.store import GateRow, MissionStore
    from lha.state.checklist_import import ImportedChecklist

app = typer.Typer(
    help="LHA — a durable, self-improving agent organization for long-horizon software missions.",
    no_args_is_help=True,
    add_completion=False,
)
db_app = typer.Typer(help="Database maintenance (Postgres).", no_args_is_help=True)
app.add_typer(db_app, name="db")
memory_app = typer.Typer(help="Tiered memory maintenance.", no_args_is_help=True)
app.add_typer(memory_app, name="memory")
objects_app = typer.Typer(help="ClaimCheck object store maintenance.", no_args_is_help=True)
app.add_typer(objects_app, name="objects")
labels_app = typer.Typer(help="Labels for fitting System One thresholds.", no_args_is_help=True)
app.add_typer(labels_app, name="labels")
eval_app = typer.Typer(
    help="Gold evaluation sets: check them, and score a judge against them.", no_args_is_help=True
)
app.add_typer(eval_app, name="eval")
workspace_app = typer.Typer(
    help="Multi-repo workspaces: one mission over several repositories.", no_args_is_help=True
)
app.add_typer(workspace_app, name="workspace")


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
    "e2b_code_interpreter": "the 'e2b' extra (lha[e2b])",
    "psycopg": "the 'postgres' extra (lha[postgres])",
    "starlette": "the 'serve' extra (lha[serve])",
    "uvicorn": "the 'serve' extra (lha[serve])",
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
    from lha.execution.egress_hosts import SandboxEgressError
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
    except SandboxEgressError as exc:
        _fail(f"invalid sandbox egress settings: {exc}")
    except ValueError as exc:  # WebConfigError, bad LHA_WEB_ALLOW_PORTS
        _fail(f"invalid web settings: {exc}")
    return settings


def _run[T](coro: Awaitable[T]) -> T:
    """Run a coroutine, turning expected operator errors into clean CLI errors."""
    import asyncio

    from temporalio.service import RPCError

    from lha.execution.factory import UnsafeSandboxError
    from lha.governor.metering import BudgetExceeded
    from lha.model.claude_code import ClaudeCodeError
    from lha.model.retry import ModelUnavailableError
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
    except ClaudeCodeError as exc:  # e.g. an expired `claude` login: an operator error, not a crash
        _fail(str(exc), code=1)
    except ModelUnavailableError as exc:  # the Planner's model is down: no mission was started
        _fail(str(exc), code=1)
    except RPCError as exc:  # e.g. "workflow not found for ID: mission:..." (as the Go lha prints)
        _fail(str(exc), code=1)
    except ModuleNotFoundError as exc:
        _fail_missing_module(exc)


# The mission-* commands define their own inner ``_run`` coroutines, which shadow ``_run``.
_run_cli = _run


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
def config(
    fingerprints: bool = typer.Option(
        False,
        "--fingerprints",
        help="Also print a 12-hex-digit SHA-256 fingerprint of each secret that is set, so a "
        "rotation can be checked without showing a value.",
    ),
) -> None:
    """Show the resolved runtime configuration (secrets redacted), and where the store is."""
    from lha.config import secret_fingerprints
    from lha.persistence.store import describe_store

    settings = get_settings()
    for key, value in settings.redacted().items():
        typer.echo(f"{key} = {value}")
    typer.echo(f"mission store = {describe_store(settings)}")
    if fingerprints:
        for key, fp in secret_fingerprints(settings).items():
            typer.echo(f"{key} fingerprint = {fp}")


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


_REPO_HELP = (
    "A member repository: URL or local path, optionally NAME=URL and @REF "
    "(e.g. svc=git@host:org/svc.git@v2); repeatable."
)


def parse_member_spec(spec: str) -> tuple[str, str, str]:
    """``[NAME=]URL[@REF]`` -> ``(name, url, ref)``; the name defaults to the URL's last path
    component without ``.git``. ``ValueError`` names what is wrong."""
    text = spec.strip()
    name, sep, rest = text.partition("=")
    if not sep or "/" in name or ":" in name:
        name, rest = "", text
    url, ref = rest, ""
    at = rest.rfind("@")
    if at > 0 and "/" not in rest[at:] and ":" not in rest[at:]:
        url, ref = rest[:at], rest[at + 1 :]
    url = url.strip()
    if not url:
        raise ValueError(f"--repo {spec!r}: missing a URL or path")
    if not name:
        name = url.rstrip("/").rsplit("/", 1)[-1].rsplit(":", 1)[-1]
        name = name[:-4] if name.endswith(".git") else name
    name = name.strip()
    if not name or name in (".", "..") or "/" in name or name.startswith("."):
        raise ValueError(f"--repo {spec!r}: {name!r} is not a usable member name")
    return name, url, ref.strip()


@workspace_app.command()
def init(
    directory: str = typer.Argument(..., help="The workspace directory (created if missing)."),
    repo: list[str] = typer.Option(..., "--repo", help=_REPO_HELP),
) -> None:
    """Create (or extend) a multi-repo workspace: a git repository holding each --repo as a
    git submodule ("member"), committed as one workspace commit.

    Point a mission's --workdir at it: the anchor and every checkpoint live in the workspace,
    each member's changes are committed inside the member first, checks and witnesses run from
    the workspace root (`cmd:sh -c 'cd svc && make test'`), and the reviewer sees each member's
    own diff. Parallel waves (--max-parallel) do not run across members.
    """
    from lha.state import git_ops

    try:
        specs = [parse_member_spec(s) for s in repo]
    except ValueError as exc:
        _fail(str(exc))
    names = [n for n, _, _ in specs]
    if len(set(names)) != len(names):
        _fail(f"--repo names repeat: {', '.join(sorted({n for n in names if names.count(n) > 1}))}")
    root = Path(directory)
    if (root / ".lha").is_dir():
        _fail(f"{directory!r} already anchors a mission; members are added before a mission starts")
    git_ops.init_repo(root)
    existing = set(git_ops.member_paths(root))
    added: list[str] = []
    for name, url, ref in specs:
        if name in existing:
            typer.echo(f"{name}: already a member, kept", err=True)
            continue
        try:
            git_ops.add_member(root, url, name, ref=ref)
        except git_ops.GitError as exc:
            _fail(f"cannot add member {name!r} from {url!r}: {exc}", code=1)
        sha = git_ops.head_sha(root / name)[:12]
        typer.echo(f"{name} <- {url}{'@' + ref if ref else ''} ({sha})")
        added.append(name)
    if added:
        git_ops.commit_all(root, f"lha: workspace members {', '.join(added)}")
    members = git_ops.member_paths(root)
    typer.echo(f"workspace {directory}: {len(members)} member{'s' if len(members) != 1 else ''}")


def _refuse_parallel_waves_on_a_workspace(workdir: str, max_parallel: int) -> None:
    """Parallel waves run implementers in git worktrees of the workspace, which do not carry its
    members; a multi-repo workspace is worked serially."""
    from lha.state import git_ops

    if max_parallel >= 2 and Path(workdir).is_dir():
        members = git_ops.member_paths(workdir)
        if members:
            _fail(
                f"--max-parallel {max_parallel}: {workdir!r} is a multi-repo workspace (members: "
                f"{', '.join(members)}); parallel waves do not run across members, use "
                "--max-parallel 1"
            )


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
    from lha.ops.report import format_cost_total

    typer.echo(format_cost_total(summary))


def format_gate_row(row: GateRow) -> list[str]:
    """Human-readable lines for one recorded gate (``lha gates``)."""
    from lha.ops.report import format_gate_row as _format

    return _format(row)


@app.command()
def gates(
    mission_id: str | None = typer.Argument(None, help="Mission id (default: every mission)."),
    limit: int = typer.Option(50, min=1, help="How many gates (most recently opened first)."),
) -> None:
    """List recorded human gates: kind, question, options, reminders, decision, who and when.

    Durable missions record every gate event (opened, reminder, resolved, defaulted) from the
    notify_gate activity; local runs with --approve-interactive record the terminal approver's.
    Reads the same mission store as `lha missions` (see `lha config` for its location).
    """

    async def _read(store: MissionStore) -> list[GateRow]:
        return await store.list_gates(mission_id, limit=limit)

    rows = _run(_with_store(_read))
    if not rows:
        typer.echo(f"no gates recorded{f' for mission {mission_id}' if mission_id else ''}")
        return
    for row in rows:
        for line in format_gate_row(row):
            typer.echo(line)


@memory_app.command()
def reembed(
    mission_id: str | None = typer.Argument(None, help="Mission id (default: every mission)."),
    dry_run: bool = typer.Option(False, "--dry-run", help="Only count the rows to re-embed."),
) -> None:
    """Re-embed stored memory with the configured embedder.

    Rows stored while the dense channel was down, or embedded by another model or model version
    (a changed LHA_MEMORY_EMBEDDER or LHA_MEMORY_EMBEDDING_MODEL, or an `ollama pull` that moved
    the model's digest), are invisible to dense recall until re-embedded. Missions re-embed up to
    64 such rows per cycle on their own; this command does them all now.
    """
    from pathlib import Path

    from lha.memory.service import open_mission_memory

    settings = get_settings()

    async def _reembed(store: MissionStore) -> list[str]:
        memory = await open_mission_memory(
            settings, store=store, workdir=Path.cwd(), mission_id=mission_id or ""
        )
        if memory is None:
            _fail("memory is disabled (LHA_MEMORY_ENABLED=false)")
        try:
            embedder = memory.embedder
            if embedder is None:
                _fail(f"no embedder to re-embed with: {memory.mode.reason}")
            if dry_run:
                counts = await store.count_stale_memory(
                    mission_id,
                    embedding_model=embedder.name,
                    embedding_version=embedder.version,
                )
                verb = "to re-embed"
            else:
                counts = await memory.reembed(mission_id)
                verb = "re-embedded"
            using = f"{embedder.name} ({embedder.version})"
            if not counts:
                return [f"nothing to re-embed for {using}"]
            lines = [f"{owner}  {count} rows" for owner, count in sorted(counts.items())]
            return [*lines, f"{sum(counts.values())} rows {verb} with {using}"]
        finally:
            await memory.close()

    for line in _run(_with_store(_reembed)):
        typer.echo(line)


@objects_app.command()
def prune(
    older_than_days: int = typer.Option(
        ..., "--older-than-days", min=1, help="Delete objects not modified for this many days."
    ),
    dry_run: bool = typer.Option(False, "--dry-run", help="Only count and size them."),
) -> None:
    """Delete old payloads from the object store at LHA_OBJECT_STORE_ROOT.

    Durable missions offload every payload over 32 KiB there and journal only its key, so the
    store grows with every cycle and nothing removes an object on its own. An object is safe to
    delete once no workflow history can still refer to it: choose a retention longer than your
    longest mission plus the Temporal namespace's history retention.
    """
    from lha.persistence.object_store import prune_objects

    result = prune_objects(
        get_settings().object_store_root, older_than_days=older_than_days, dry_run=dry_run
    )
    verb = "to delete" if dry_run else "deleted"
    typer.echo(
        f"{result.count} objects {verb} ({result.bytes / 2**20:.1f} MiB), {result.kept} kept"
    )


@labels_app.command()
def export(
    mission_id: str | None = typer.Argument(
        None, help="Mission whose gates to include (default: the one the anchor names)."
    ),
    workdir: str = typer.Option(".", help="The mission workspace (the git repo holding .lha/)."),
    out: str = typer.Option("-", help="Where to write the JSON Lines ('-' = stdout)."),
    diffs: bool = typer.Option(
        False, "--diffs", help="Include each reviewed diff (git diff base..head, capped)."
    ),
) -> None:
    """Export the mission's judgments as JSON Lines labels: one object per human gate answer,
    tool approval, verifier verdict and review verdict, secrets redacted.

    Reads the anchor's committed events at WORKDIR and the mission's closed gates from the
    mission store. Without an anchor, MISSION_ID is required and only the gates are exported.
    """
    from pathlib import Path

    from lha.agents.waves import diff_since
    from lha.state.mission_anchor import ANCHOR_DIR, GitMissionAnchor
    from lha.systemone.labels import SOURCES, label_rows, mission_id_of, to_jsonl

    root = Path(workdir)
    has_anchor = (root / ANCHOR_DIR).is_dir()
    if not has_anchor and not mission_id:
        _fail(
            f"no mission anchor at {workdir!r} (expected a {ANCHOR_DIR}/ directory); "
            "pass MISSION_ID to export a mission's gates alone"
        )
    events = _run(GitMissionAnchor(workdir).read_events()) if has_anchor else []
    mission = mission_id or mission_id_of(events)

    async def _gates(store: MissionStore) -> list[GateRow]:
        return await store.list_gates(mission, limit=100_000)

    gates = _run(_with_store(_gates)) if mission else []
    if not mission:
        typer.echo("warning: the anchor names no mission; gates are not exported", err=True)
    rows = label_rows(
        events,
        gates,
        mission_id=mission,
        diffs=(lambda base, head: diff_since(workdir, base, head)) if diffs else None,
    )
    text = to_jsonl(rows)
    if out == "-":
        typer.echo(text, nl=False)
    else:
        Path(out).write_text(text, encoding="utf-8")
    counts = ", ".join(f"{sum(1 for r in rows if r.source == s)} {s}" for s in SOURCES)
    typer.echo(f"{len(rows)} labels ({counts})", err=True)


def _load_gold(files: list[str]) -> list[Any]:
    """Every gold row in ``files`` (``lha eval``); a bad line or set exits 2."""
    from pathlib import Path

    from lha.systemone.gold import GoldError, check_gold, parse_gold

    rows: list[Any] = []
    for name in files:
        try:
            text = Path(name).read_text(encoding="utf-8")
        except OSError as exc:
            _fail(f"cannot read {name!r}: {exc}")
        try:
            rows += parse_gold(text, name=name)
        except GoldError as exc:
            _fail(str(exc))
    errors = check_gold(rows)
    if errors:
        for line in errors:
            typer.echo(f"error: {line}", err=True)
        raise typer.Exit(2)
    return rows


@eval_app.command()
def check(files: list[str] = typer.Argument(..., help="Gold JSON Lines files.")) -> None:
    """Check gold files: every line a schema-1 label row with a 'gold' judgment inside its
    source's vocabulary, no judgment recorded twice. Prints the row counts; exit 2 on the first
    bad file."""
    from lha.systemone.gold import count_by_source

    rows = _load_gold(files)
    plural = "" if len(files) == 1 else "s"
    typer.echo(f"{count_by_source(rows)} in {len(files)} file{plural}")


@eval_app.command()
def run(
    files: list[str] = typer.Argument(..., help="Gold JSON Lines files."),
    judge: str = typer.Option(
        "recorded",
        help="recorded (the label the mission recorded) or screen (the pre-review screen "
        "re-run on each review row's diff).",
    ),
) -> None:
    """Score a judge against gold files: per source, agreement with the gold labels and the
    precision and recall of its refusing label, then every disagreement."""
    from lha.systemone.gold import JUDGES, judge_named, render_scorecards, score

    if judge not in JUDGES:
        _fail(f"unknown --judge {judge!r}; expected {', '.join(JUDGES)}")
    rows = _load_gold(files)
    typer.echo(render_scorecards(score(rows, judge_named(judge)), judge), nl=False)


@app.command(name="mission-report")
def mission_report(
    mission_id: str | None = typer.Argument(
        None, help="Mission whose store rows to include (default: the one the anchor names)."
    ),
    workdir: str = typer.Option(".", help="The mission workspace (the git repo holding .lha/)."),
) -> None:
    """One page about a mission: items and their status, the cycles' verdicts, reviews and
    screens, the human gates, the spend and the commits, from the anchor and the mission store.

    Reads the anchor at WORKDIR and, for the mission it names (or MISSION_ID), the store's
    mission row, gates and cost ledger. Without an anchor, MISSION_ID is required.
    """
    from pathlib import Path

    from lha.ops.report import ReportInput, render_report
    from lha.state import git_ops
    from lha.state.mission_anchor import ANCHOR_DIR, GitMissionAnchor
    from lha.systemone.labels import mission_id_of

    root = Path(workdir)
    has_anchor = (root / ANCHOR_DIR).is_dir()
    if not has_anchor and not mission_id:
        _fail(
            f"no mission anchor at {workdir!r} (expected a {ANCHOR_DIR}/ directory); "
            "pass MISSION_ID to report from the store alone"
        )
    spec = checklist = None
    events: list[Any] = []
    commits, head = 0, ""
    if has_anchor:
        anchor = GitMissionAnchor(workdir)
        spec = _run(anchor.read_mission())
        checklist = _run(anchor.read_checklist())
        events = _run(anchor.read_events())
        head = git_ops.head_sha(workdir)
        commits = int(git_ops.run_git(workdir, "rev-list", "--count", "HEAD").strip() or 0)
    mission = mission_id or mission_id_of(events)

    async def _store(store: MissionStore) -> tuple[Any, list[GateRow], Any]:
        return (
            await store.get_mission(mission),
            await store.list_gates(mission, limit=100_000),
            await store.cost_summary(mission),
        )

    row, gates, cost = _run(_with_store(_store)) if mission else (None, [], None)
    from lha.contracts.state import Checklist

    typer.echo(
        render_report(
            ReportInput(
                mission_id=mission,
                spec=spec,
                checklist=checklist if checklist is not None else Checklist(items=[]),
                events=events,
                row=row,
                gates=sorted(gates, key=lambda g: (g.opened_at, g.gate_id)),
                cost=cost,
                commits=commits,
                head_sha=head,
            )
        ),
        nl=False,
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
    task: str = typer.Option(
        "", help="The mission / task description (required unless --resume or --checklist)."
    ),
    title: str | None = typer.Option(
        None, help="Mission title (default: the checklist's, else 'mission')."
    ),
    checklist_file: str | None = typer.Option(None, "--checklist", help=_CHECKLIST_HELP),
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
    resume: bool = typer.Option(
        False,
        "--resume",
        help="Continue the mission already anchored in --workdir (no planning; its checklist, "
        "ownership map and decisions are kept).",
    ),
    research: int = typer.Option(
        2, "--research", min=0, max=4, help="Read-only researchers per item (0 = none)."
    ),
    review: bool = typer.Option(
        True,
        "--review/--no-review",
        help="An independent reviewer after every verified item; a blocking review reopens it "
        "(the pre-review screen still runs, and forces the review when it finds weakened tests).",
    ),
) -> None:
    """Plan, then run the FULL multi-agent org (research, Lead or parallel waves, review) locally."""
    from lha.agent.runner import MissionSummary, aclose_provider, build_meter
    from lha.agents.orchestrator import Orchestrator, anchor_exists
    from lha.agents.planner import Planner
    from lha.contracts.verify import checks_from_commands
    from lha.model import build_provider

    has_anchor = anchor_exists(workdir)
    if resume and not has_anchor:
        _fail(f"--resume: no mission anchor in {workdir!r} (start one without --resume)")
    if not resume and has_anchor:
        _fail(
            f"{workdir!r} already holds a mission: pass --resume to continue it, or use a new "
            "--workdir (starting over would replace its checklist)"
        )
    if resume and checklist_file:
        _fail("--checklist cannot be combined with --resume (the mission keeps its checklist)")
    if not resume and not task.strip() and not checklist_file:
        _fail("give --task or --checklist FILE (or --resume to continue an existing mission)")
    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local, allow_host)
    imported = _load_checklist_file(checklist_file) if checklist_file and not resume else None

    async def _mission() -> MissionSummary:
        meter = build_meter(settings)  # planner + every org role share one budget
        if resume:
            return await Orchestrator(
                settings, meter=meter, research_per_item=research, do_review=review
            ).run_mission(
                workdir=workdir,
                checks=checks,
                gate=_gate(approve_interactive, settings),
                resume=True,
            )
        if imported is not None:  # a checklist of your own: no planning, no ownership map
            return await Orchestrator(
                settings, meter=meter, research_per_item=research, do_review=review
            ).run_mission(
                workdir=workdir,
                title=title or imported.title or "mission",
                description=task or imported.description,
                checklist=imported.checklist,
                checks=checks,
                gate=_gate(approve_interactive, settings),
                references=_merge_references(list(reference), imported.references),
            )
        planner_model = build_provider(settings)
        try:
            planner = Planner(meter.wrap(planner_model, role="planner"))
            plan = await planner.plan_mission(title=title or "mission", description=task)
        finally:
            await aclose_provider(planner_model)
        return await Orchestrator(
            settings, meter=meter, research_per_item=research, do_review=review
        ).run_mission(
            workdir=workdir,
            title=title or "mission",
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

    from lha.durable.worker import MixedWorkersError, run_worker

    try:
        asyncio.run(run_worker())
    except (MixedWorkersError, ValueError) as exc:  # a Go worker polls this queue; bad settings
        _fail(str(exc))
    except RuntimeError as exc:  # LHA_WORKER_PROMOTE could not promote this build
        _fail(str(exc), code=1)


@app.command()
def serve(
    host: str = typer.Option(
        "127.0.0.1", help="Loopback address to bind (127.0.0.1 or localhost)."
    ),
    port: int = typer.Option(8765, min=0, max=65535, help="Port (0 picks a free one)."),
) -> None:
    """Serve the mission UI's API (spec/serve/openapi.json) on loopback.

    Prints the start-up URL with its token first; LHA_SERVE_TOKEN fixes the token (otherwise it is
    random). Reads the mission store (`lha config`), each mission's anchor, and Temporal for
    durable missions. Needs the `serve` extra (`uv sync --extra serve`).
    """
    import os

    try:
        import uvicorn  # noqa: F401

        import lha.serve.app  # noqa: F401
        from lha.serve import serve as run_server
    except ModuleNotFoundError as exc:
        _fail_missing_module(exc)
    try:
        run_server(
            get_settings(), host=host, port=port, token=os.environ.get("LHA_SERVE_TOKEN", "")
        )
    except (ValueError, OSError) as exc:
        _fail(str(exc))


@app.command()
def mcp() -> None:
    """Serve the missions to an MCP client on stdin and stdout (spec/serve/mcp.json).

    The tools read missions and steer or snooze one; none answers a gate, aborts or edits the
    checklist. Reads the mission store (`lha config`), each mission's anchor, and Temporal for
    durable missions. `lha serve` answers the same tools at /mcp. Needs the `serve` extra.
    """
    try:
        import starlette  # noqa: F401

        from lha.serve import mcp_stdio
    except ModuleNotFoundError as exc:
        _fail_missing_module(exc)
    mcp_stdio(get_settings())


@app.command()
def watch(
    mission_id: str = typer.Argument(..., help="The mission id (`lha missions`)."),
    url: str | None = typer.Option(
        None,
        "--url",
        help="Server base (default: LHA_SERVE_URL, else http://127.0.0.1:8765); the serve "
        "start-up URL with its ?token= is accepted.",
    ),
    token: str | None = typer.Option(
        None, "--token", help="API token (default: LHA_SERVE_TOKEN, else the URL's token)."
    ),
    interval: float = typer.Option(2.0, "--interval", help="Seconds between refreshes."),
    limit: int = typer.Option(10, "--limit", min=1, help="Recent events shown."),
    once: bool = typer.Option(
        False, "--once", help="Render once and exit (for non-TTY, tests, CI)."
    ),
) -> None:
    """Watch one mission: a refreshing terminal view of its state and newest events.

    Reads the same UI API as `lha serve` (spec/serve/openapi.json), so it works over SSH without
    a browser. Without --once it clears the screen and refreshes every --interval seconds until
    interrupted (Ctrl-C exits 0); a non-TTY stdout behaves as --once.
    """
    import os
    import sys
    import time

    import httpx

    from lha.cli.watch import (
        CLEAR,
        DEFAULT_URL,
        WatchError,
        resolve_target,
        watch_fetch,
        watch_render,
    )

    target = url or os.environ.get("LHA_SERVE_URL") or DEFAULT_URL
    base_url, api_token = resolve_target(target, token or os.environ.get("LHA_SERVE_TOKEN"))
    once = once or not sys.stdout.isatty()
    with httpx.Client(timeout=5.0) as client:
        try:
            while True:
                mission, events = watch_fetch(client, base_url, api_token, mission_id, limit)
                if not once:
                    sys.stdout.write(CLEAR)
                sys.stdout.write(watch_render(mission, events))
                sys.stdout.flush()
                if once:
                    return
                time.sleep(interval)
        except WatchError as exc:
            _fail(str(exc), code=1)
        except KeyboardInterrupt:
            return


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
    research: int = typer.Option(
        0,
        "--research",
        min=0,
        max=4,
        help="Read-only researcher child workflows per item before each round (0 = none).",
    ),
    review: bool = typer.Option(
        False,
        "--review/--no-review",
        help="An independent reviewer after every verified item; a blocking review reopens it.",
    ),
    max_parallel: int = typer.Option(
        0,
        "--max-parallel",
        min=0,
        max=8,
        help="Parallel implementer waves of up to N items with disjoint Planner-assigned files, "
        "each in its own git worktree (below 2 = never).",
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
    _refuse_parallel_waves_on_a_workspace(workdir, max_parallel)
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
        ownership = None
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
                if max_parallel >= 2:  # waves need the Planner's single-writer file ownership
                    plan = await planner.plan_mission(title=mission_title, description=task)
                    checklist, ownership = plan.checklist, plan.ownership
                else:
                    checklist = await planner.plan(title=mission_title, description=task)
            finally:
                await aclose_provider(planner_model)
            planner_spend = list(meter.ledger.entries)
        mission_id = new_id("mission")
        anchor = GitMissionAnchor(workdir)
        await anchor.initialize(
            title=mission_title,
            description=description,
            items=checklist,
            references=references,
            ownership=ownership,
            mission_id=mission_id,
        )
        client = await connect_client(settings)
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
                workdir=workdir,
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
                        research_per_item=research,
                        review=review,
                        max_parallel=max_parallel,
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

    if imported is not None and max_parallel >= 2:
        typer.echo(
            "note: an imported checklist declares no file ownership, so no parallel wave can run "
            "(items are worked serially)",
            err=True,
        )
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
    """Query a mission's status, cycles, sleep, open gate (+ pending action), steering notes and
    gate events."""
    from datetime import UTC, datetime

    from lha.config import get_settings
    from lha.durable.signals import (
        QUERY_CYCLES,
        QUERY_GATE_LOG,
        QUERY_PENDING_EDITS,
        QUERY_STATUS,
        QUERY_STEER_NOTES,
    )
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
        notes = await _optional_query(handle.query(QUERY_STEER_NOTES)) or []
        if notes:
            lines.append(f"steering notes ({len(notes)}, latest last):")
            lines += [f"  {_one_line(n, 120)}" for n in notes[-3:]]
        pending = await _optional_query(handle.query(QUERY_PENDING_EDITS)) or 0
        if pending:
            lines.append(f"checklist edits pending: {pending} (applied before the next cycle)")
        log = await _optional_query(handle.query(QUERY_GATE_LOG)) or []
        if log:
            lines.append("recent gate events:")
            lines += [f"  {line}" for line in log[-8:]]
        return lines

    for line in _run_cli(_run()):
        typer.echo(line)


def _one_line(text: str, limit: int) -> str:
    """``text`` on one line, cut to ``limit`` characters with an ellipsis."""
    flat = " ".join(text.split())
    return flat if len(flat) <= limit else flat[: limit - 3] + "..."


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
    by: str = typer.Option(
        "", "--as", help="Who decides; recorded as the gate's resolved_by (`lha gates`)."
    ),
) -> None:
    """Resolve an open human gate on a mission with a decision."""
    if decision.strip().lower() not in _ALL_DECISIONS:
        _fail(f"unknown --decision {decision!r}; expected {', '.join(_ALL_DECISIONS)}")
    by = by.strip()
    if len(by) > 200:
        _fail("--as is longer than 200 characters")

    from lha.config import get_settings
    from lha.durable.signals import SIGNAL_HUMAN_DECISION, SIGNAL_HUMAN_DECISION_V2
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
        if by:
            await handle.signal(SIGNAL_HUMAN_DECISION_V2, {"decision": choice, "by": by})
        else:
            await handle.signal(SIGNAL_HUMAN_DECISION, choice)
        return choice

    choice = _run_cli(_run())
    suffix = f" as {by}" if by else ""
    typer.echo(f"sent decision '{choice}' to mission {mission_id}{suffix}")


@app.command(name="mission-snooze")
def mission_snooze(
    mission_id: str = typer.Argument(..., help="Mission id."),
    seconds: int = typer.Option(
        ..., min=0, help="Sleep (status SLEEPING) this long before the next cycle; 0 wakes it."
    ),
) -> None:
    """Park a mission on a durable timer (SLEEPING) before its next cycle, or wake it."""

    from lha.config import get_settings
    from lha.durable.signals import SIGNAL_SNOOZE
    from lha.durable.worker import connect_client

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.signal(SIGNAL_SNOOZE, seconds)

    _run_cli(_run())
    typer.echo(f"mission {mission_id}: " + (f"snoozed {seconds}s" if seconds else "woken"))


@app.command(name="mission-steer")
def mission_steer(
    mission_id: str = typer.Argument(..., help="Mission id."),
    note: str = typer.Option(
        ..., help="The note (at most 2000 characters); every following cycle's prompt includes it."
    ),
) -> None:
    """Append an operator steering note that every following cycle's prompt includes."""

    from lha.config import get_settings
    from lha.durable.signals import MAX_STEER_CHARS, MAX_STEER_NOTES, SIGNAL_STEER
    from lha.durable.worker import connect_client

    text = note.strip()
    if not text:
        _fail("--note must not be empty")
    if len(text) > MAX_STEER_CHARS:
        _fail(f"--note is {len(text)} characters; at most {MAX_STEER_CHARS} are kept")

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.signal(SIGNAL_STEER, text)

    _run_cli(_run())
    typer.echo(
        f"mission {mission_id}: steering note added ({len(text)} chars; "
        f"the last {MAX_STEER_NOTES} notes are kept)"
    )


_EDIT_ADD_HELP = (
    "Add an item with this description (repeatable); witnesses the roadmap way: "
    "'Do X (witness: cmd:make test)'."
)
_EDIT_DESCRIBE_HELP = "ID=TEXT: replace an open item's description (repeatable)."
_EDIT_DEPENDS_HELP = "ID=DEP[,DEP]: replace an open item's dependencies (ID= clears them)."
_EDIT_FILE_HELP = (
    'A JSON file with a list of edit objects (or {"edits": [...]}): op add | remove | edit | '
    "reopen | block | unblock; applied before the options below."
)
_EDIT_WORKDIR_HELP = (
    "Edit the checklist of the mission anchored in this directory directly (a mission run "
    "with 'lha mission' or 'lha orchestrate', between runs) instead of signalling a durable one."
)


def _edit_ops(
    *,
    edits_file: str | None,
    describe: list[str],
    depends: list[str],
    reopen: list[str],
    unblock: list[str],
    block: list[str],
    remove: list[str],
    add: list[str],
) -> list[dict[str, object]]:
    """The ``checklist_edit_v1`` batch an ``lha mission-edit`` invocation describes."""
    import json
    from pathlib import Path

    ops: list[dict[str, object]] = []
    if edits_file:
        try:
            data = json.loads(Path(edits_file).read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            _fail(f"cannot read --edits {edits_file!r}: {exc}")
        if isinstance(data, dict):
            data = data.get("edits")
        if not isinstance(data, list) or not all(isinstance(e, dict) for e in data):
            _fail(f"--edits {edits_file!r}: expected a JSON list of edit objects")
        ops += data

    def split(option: str, value: str) -> tuple[str, str]:
        item_id, sep, rest = value.partition("=")
        if not sep or not item_id.strip():
            _fail(f"{option} expects ID=VALUE (got {value!r})")
        return item_id.strip(), rest

    for value in describe:
        item_id, text = split("--describe", value)
        ops.append({"op": "edit", "id": item_id, "description": text})
    for value in depends:
        item_id, text = split("--depends", value)
        deps = [d.strip() for d in text.split(",") if d.strip()]
        ops.append({"op": "edit", "id": item_id, "depends_on": deps})
    ops += [{"op": "reopen", "id": i} for i in reopen]
    ops += [{"op": "unblock", "id": i} for i in unblock]
    ops += [{"op": "block", "id": i} for i in block]
    ops += [{"op": "remove", "id": i} for i in remove]
    ops += [{"op": "add", "description": text} for text in add]
    return ops


@app.command(name="mission-edit")
def mission_edit(
    mission_id: str | None = typer.Argument(None, help="Mission id (omit with --workdir)."),
    add: list[str] = typer.Option([], "--add", help=_EDIT_ADD_HELP),
    remove: list[str] = typer.Option([], "--remove", help="Remove an open item (repeatable)."),
    reopen: list[str] = typer.Option([], "--reopen", help="Reopen a done item (repeatable)."),
    block: list[str] = typer.Option([], "--block", help="Block an open item (repeatable)."),
    unblock: list[str] = typer.Option([], "--unblock", help="Unblock a blocked item (repeatable)."),
    describe: list[str] = typer.Option([], "--describe", help=_EDIT_DESCRIBE_HELP),
    depends: list[str] = typer.Option([], "--depends", help=_EDIT_DEPENDS_HELP),
    edits_file: str | None = typer.Option(None, "--edits", help=_EDIT_FILE_HELP),
    by: str = typer.Option("", "--as", help="Who edits; recorded in the anchor's event."),
    workdir: str | None = typer.Option(None, "--workdir", help=_EDIT_WORKDIR_HELP),
) -> None:
    """Add, remove, edit, reopen, block or unblock checklist items of a mission in flight.

    A durable mission applies the batch before its next cycle (or while it sleeps or at the
    deadlock gate's 'retry'), never mid-cycle; 'lha mission-status' shows it pending, then the
    gate log shows what was applied or why the batch was refused. Edits are validated as one
    batch: one bad edit refuses them all.
    """
    from lha.state.checklist_edit import MAX_EDIT_OPS

    if bool(mission_id) == bool(workdir):
        _fail("give MISSION_ID (a durable mission) or --workdir DIR (a local one), not both")
    by = by.strip()
    if len(by) > 200:
        _fail("--as is longer than 200 characters")
    ops = _edit_ops(
        edits_file=edits_file,
        describe=describe,
        depends=depends,
        reopen=reopen,
        unblock=unblock,
        block=block,
        remove=remove,
        add=add,
    )
    if not ops:
        _fail(
            "nothing to do: give at least one of --add, --remove, --reopen, --block, --unblock, "
            "--describe, --depends or --edits FILE"
        )
    if len(ops) > MAX_EDIT_OPS:
        _fail(f"{len(ops)} edits; at most {MAX_EDIT_OPS} per batch")

    if workdir:
        from lha.durable.activities import EDIT_EVENT, _edit_checklist
        from lha.durable.types import EditInput
        from lha.state.locks import WorkdirBusyError
        from lha.state.mission_anchor import GitMissionAnchor

        async def _local() -> str:
            anchor = GitMissionAnchor(workdir)
            done = sum(1 for e in await anchor.read_events() if e.kind == EDIT_EVENT)
            try:
                result = await _edit_checklist(
                    EditInput(
                        mission_id="", workdir=workdir, cycle_id=f"e{done + 1}", edits=ops, by=by
                    )
                )
            except WorkdirBusyError:
                _fail(f"a cycle is running in {workdir}; edit the mission by id instead", code=1)
            if not result.advanced:
                _fail(result.note, code=1)
            return result.note

        typer.echo(_run_cli(_local()))
        return

    from lha.durable.signals import SIGNAL_CHECKLIST_EDIT
    from lha.durable.worker import connect_client

    async def _signal() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.signal(SIGNAL_CHECKLIST_EDIT, {"edits": ops, "by": by})

    _run_cli(_signal())
    plural = "" if len(ops) == 1 else "s"
    typer.echo(
        f"mission {mission_id}: {len(ops)} checklist edit{plural} queued (applied before the "
        "next cycle; 'lha mission-status' shows the outcome)"
    )


@app.command(name="mission-abort")
def mission_abort(mission_id: str = typer.Argument(..., help="Mission id.")) -> None:
    """Cancel a running mission workflow."""

    from lha.config import get_settings
    from lha.durable.worker import connect_client

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.cancel()

    _run_cli(_run())
    typer.echo(f"cancelled mission {mission_id}")


# Commands that start the Temporal Python SDK's native runtime. It can call into Python while the
# interpreter finalizes and abort the process ("Fatal Python error: PyGILState_Release",
# temporalio/sdk-python#300, still open), after the command has already finished: a mission-*
# command that succeeded, or a worker that refused a Go-polled task queue. These commands
# therefore end without interpreter finalization, once tracing and the standard streams are
# flushed, keeping the command's exit code. (A test keeps the list complete.)
_TEMPORAL_COMMANDS = frozenset(
    {
        "worker",
        "mission-start",
        "mission-status",
        "mission-approve",
        "mission-snooze",
        "mission-steer",
        "mission-edit",
        "mission-abort",
    }
)


def main(argv: list[str] | None = None) -> None:
    """The ``lha`` console script."""
    import os
    import sys

    args = sys.argv[1:] if argv is None else argv
    command = next((a for a in args if not a.startswith("-")), None)
    if command not in _TEMPORAL_COMMANDS:
        app(args=args, prog_name="lha")
        return
    try:
        app(args=args, prog_name="lha")
        code: int = 0
    except SystemExit as exc:
        if exc.code is None or isinstance(exc.code, int):
            code = exc.code or 0
        else:
            print(exc.code, file=sys.stderr)
            code = 1
    from lha.obs.otel import shutdown_tracing

    shutdown_tracing()
    for stream in (sys.stdout, sys.stderr):
        stream.flush()
    os._exit(code)


if __name__ == "__main__":
    main()
