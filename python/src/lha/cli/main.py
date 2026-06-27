"""``lha`` command-line entry point.

Commands are added as the planes land. For now this exposes ``version`` and ``config``,
which are enough to confirm the package installs and configures correctly at $0.
"""

from __future__ import annotations

import shlex
from collections.abc import Awaitable
from typing import NoReturn

import typer

from lha import __version__
from lha.config import Settings, get_settings

app = typer.Typer(
    help="LHA — a durable, self-improving agent organization for long-horizon software missions.",
    no_args_is_help=True,
    add_completion=False,
)
db_app = typer.Typer(help="Database maintenance (Postgres).", no_args_is_help=True)
app.add_typer(db_app, name="db")

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


def _run_settings(sandbox: str | None, unsafe_local: bool) -> Settings:
    """Settings with the CLI's sandbox overrides; refuses an unsafe local sandbox up front
    (before any planning spend or workspace writes)."""
    from lha.execution.factory import SANDBOX_KINDS, UnsafeSandboxError, build_sandbox

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
    if settings.sandbox == "local":
        try:
            build_sandbox("local", allow_unsafe_local=settings.allow_unsafe_local)
        except UnsafeSandboxError as exc:
            _fail(f"{exc} (or pass --unsafe-local)")
    return settings


def _run[T](coro: Awaitable[T]) -> T:
    """Run a coroutine, turning expected operator errors into clean CLI errors."""
    import asyncio

    from lha.execution.factory import UnsafeSandboxError
    from lha.governor.metering import BudgetExceeded

    async def _main() -> T:
        return await coro

    try:
        return asyncio.run(_main())
    except UnsafeSandboxError as exc:
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
    """Show the resolved runtime configuration (secrets redacted)."""
    for key, value in get_settings().redacted().items():
        typer.echo(f"{key} = {value}")


@db_app.command()
def migrate(
    migrations_dir: str = typer.Option("db/migrations", help="Directory of *.sql migrations."),
) -> None:
    """Apply the SQL migrations to the Postgres database at LHA_POSTGRES_DSN."""
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


@app.command(name="run-local")
def run_local(
    title: str = typer.Option(..., help="Mission title."),
    item: list[str] = typer.Option(
        ..., "--item", help="A checklist item description (repeatable)."
    ),
    workdir: str = typer.Option(
        ".lha/workspaces/local", help="Workspace dir (becomes a git repo)."
    ),
    description: str = typer.Option("", help="Mission description."),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
) -> None:
    """Run a mission locally (no Temporal) until complete / deadlocked / over-budget."""
    from lha.agent.runner import run_mission_local
    from lha.contracts.state import Checklist, ChecklistItem
    from lha.contracts.verify import checks_from_commands

    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local)
    checklist = Checklist(
        items=[ChecklistItem(id=f"{i + 1:02d}", description=desc) for i, desc in enumerate(item)]
    )
    summary = _run(
        run_mission_local(
            workdir=workdir,
            title=title,
            description=description,
            checklist=checklist,
            checks=checks,
            settings=settings,
        )
    )
    _report(summary)


@app.command()
def mission(
    task: str = typer.Option(
        ..., help="The mission / task description (the Planner decomposes it)."
    ),
    title: str = typer.Option("mission", help="Mission title."),
    workdir: str = typer.Option(
        ".lha/workspaces/mission", help="Workspace dir (becomes a git repo)."
    ),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
) -> None:
    """Plan a task into a checklist, then run it locally to completion."""
    from lha.agent.runner import plan_and_run_local
    from lha.contracts.verify import checks_from_commands

    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local)
    summary = _run(
        plan_and_run_local(
            workdir=workdir, title=title, task=task, checks=checks, settings=settings
        )
    )
    _report(summary)


@app.command()
def orchestrate(
    task: str = typer.Option(..., help="The mission / task description."),
    title: str = typer.Option("mission", help="Mission title."),
    workdir: str = typer.Option(".lha/workspaces/org", help="Workspace dir (becomes a git repo)."),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
    sandbox: str | None = typer.Option(None, "--sandbox", help=_SANDBOX_HELP),
    unsafe_local: bool = typer.Option(False, "--unsafe-local", help=_UNSAFE_LOCAL_HELP),
) -> None:
    """Plan, then run the FULL multi-agent org (research fan-out + Lead + review) locally."""
    from lha.agent.runner import MissionSummary, aclose_provider, build_meter
    from lha.agents.orchestrator import Orchestrator
    from lha.agents.planner import Planner
    from lha.contracts.verify import checks_from_commands
    from lha.model import build_provider

    checks = checks_from_commands(resolve_check_commands(check, no_default_checks))
    settings = _run_settings(sandbox, unsafe_local)

    async def _mission() -> MissionSummary:
        meter = build_meter(settings)  # planner + every org role share one budget
        planner_model = build_provider(settings)
        try:
            planner = Planner(meter.wrap(planner_model, role="planner"))
            checklist = await planner.plan(title=title, description=task)
        finally:
            await aclose_provider(planner_model)
        return await Orchestrator(settings, meter=meter).run_mission(
            workdir=workdir, title=title, description=task, checklist=checklist, checks=checks
        )

    _report(_run(_mission()))


@app.command()
def worker() -> None:
    """Run a Temporal worker that serves missions (requires a Temporal server)."""
    import asyncio

    from lha.durable.worker import run_worker

    asyncio.run(run_worker())


@app.command(name="mission-start")
def mission_start(
    task: str = typer.Option(..., help="The mission / task description."),
    title: str = typer.Option("mission", help="Mission title."),
    workdir: str = typer.Option(
        ".lha/workspaces/durable", help="Workspace dir (becomes a git repo)."
    ),
    check: list[str] = typer.Option([], "--check", help=_CHECK_HELP),
    no_default_checks: bool = typer.Option(
        False, "--no-default-checks", help=_NO_DEFAULT_CHECKS_HELP
    ),
) -> None:
    """Plan + initialize the anchor, then start a durable MissionWorkflow on Temporal."""
    from lha.agent.runner import aclose_provider, build_meter
    from lha.agents.planner import Planner
    from lha.config import get_settings
    from lha.durable.types import MissionInput
    from lha.durable.worker import connect_client
    from lha.durable.workflows import MissionWorkflow
    from lha.ids import new_id
    from lha.model import build_provider
    from lha.state.mission_anchor import GitMissionAnchor

    check_commands = resolve_check_commands(check, no_default_checks)

    async def _start() -> str:
        settings = get_settings()
        meter = build_meter(settings)
        planner_model = build_provider(settings)
        try:
            planner = Planner(meter.wrap(planner_model, role="planner"))
            checklist = await planner.plan(title=title, description=task)
        finally:
            await aclose_provider(planner_model)
        anchor = GitMissionAnchor(workdir)
        await anchor.initialize(title=title, description=task, items=checklist)
        client = await connect_client(settings)
        mission_id = new_id("mission")
        await client.start_workflow(
            MissionWorkflow.run,
            MissionInput(mission_id=mission_id, workdir=workdir, check_commands=check_commands),
            id=f"mission:{mission_id}",
            task_queue=settings.task_queue,
        )
        return mission_id

    mission_id = _run(_start())
    typer.echo(f"started mission {mission_id} (workflow id: mission:{mission_id})")


@app.command(name="mission-status")
def mission_status(mission_id: str = typer.Argument(..., help="Mission id.")) -> None:
    """Query a running mission's status + cycle count (works mid-run and after completion)."""
    import asyncio

    from lha.config import get_settings
    from lha.durable.signals import QUERY_CYCLES, QUERY_STATUS
    from lha.durable.worker import connect_client

    async def _run() -> str:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        status = await handle.query(QUERY_STATUS)
        cycles = await handle.query(QUERY_CYCLES)
        return f"status={status} cycles={cycles}"

    typer.echo(asyncio.run(_run()))


@app.command(name="mission-approve")
def mission_approve(
    mission_id: str = typer.Argument(..., help="Mission id."),
    decision: str = typer.Option("approve", help="approve | reject | abort"),
) -> None:
    """Resolve an open HITL gate on a mission with a human decision."""
    import asyncio

    from lha.config import get_settings
    from lha.durable.signals import SIGNAL_HUMAN_DECISION
    from lha.durable.worker import connect_client

    async def _run() -> None:
        client = await connect_client(get_settings())
        handle = client.get_workflow_handle(f"mission:{mission_id}")
        await handle.signal(SIGNAL_HUMAN_DECISION, decision)

    asyncio.run(_run())
    typer.echo(f"sent decision '{decision}' to mission {mission_id}")


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
