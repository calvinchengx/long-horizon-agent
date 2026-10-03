"""The mission store: one interface, two backends (SQLite by default, Postgres with a DSN).

Everything a run persists outside git goes through ``MissionStore``:

* ``missions`` — one row per mission with its status transitions (RUNNING → DONE / ABORTED / ...);
* ``hitl_gates`` — one row per human gate: opened, reminders, resolved or defaulted, by whom;
* ``cost_ledger`` — EVERY metered model call (``usd`` is NULL when the cost is unknown), written
  idempotently by key so a retried/replayed write never double-counts;
* ``episodic_events`` — cycle outcomes and memory bookkeeping (the episodic memory tier);
* ``semantic_memory`` — distilled facts / progress notes, optionally with embeddings;
* ``skills`` — verified, reusable know-how (the procedural tier).

``open_store(settings)`` picks the backend: ``LHA_POSTGRES_DSN`` set → ``PostgresStore`` (tables
from ``db/migrations``; run ``lha db migrate`` first), otherwise ``SqliteStore`` (stdlib
``sqlite3``, WAL mode, schema created on open) at ``resolve_sqlite_path``: ``LHA_SQLITE_PATH`` if
set, else one per-user file (``default_sqlite_path``) that every process shares. If Postgres is configured but unusable, the run
falls back to SQLite (``settings.postgres_fallback_to_sqlite``) and the store says so via
``degraded_reason``.
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol, runtime_checkable

from lha.config import Settings
from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill
from lha.obs.events import get_logger

BACKEND_SQLITE = "sqlite"
BACKEND_POSTGRES = "postgres"

#: Migrations the Postgres backend needs (``db/migrations``), checked when the store opens.
REQUIRED_PG_MIGRATIONS = (
    "0001_init",
    "0002_idempotent_ledger",
    "0003_cost_unknown_usd_null",
    "0004_memory_skills",
    "0005_hitl_gates",
    "0006_mission_events",
)


class StoreUnavailableError(RuntimeError):
    """The configured store cannot be used (unreachable, unmigrated, driver missing)."""


@dataclass
class MissionRow:
    mission_id: str
    title: str
    status: str
    description: str = ""
    head_sha: str | None = None
    workflow_id: str | None = None
    created_at: str = ""
    updated_at: str = ""


@dataclass
class CostRow:
    mission_id: str
    cycle_id: str
    model: str
    role: str
    input_tokens: int
    output_tokens: int
    usd: float | None  # None = cost unknown (never recorded as $0)
    cost_known: bool
    ts: str = ""


@dataclass
class CostSummary:
    mission_id: str
    calls: int = 0
    known_usd: float = 0.0
    unknown_cost_calls: int = 0
    input_tokens: int = 0
    output_tokens: int = 0


@dataclass
class EventRow:
    id: int
    mission_id: str
    cycle_id: str
    kind: str
    payload: dict[str, Any] = field(default_factory=dict)
    ts: str = ""


@dataclass
class MissionEvent:
    """One entry of a mission's shared event record (``mission_events``): a trace event persisted
    so any reader (``lha serve``, ``lha mission-report``, SQL) can follow a run without its logs.
    See ``spec/state/mission_events.json`` and docs/27-mission-ui.md."""

    mission_id: str
    cycle_id: str
    kind: str
    payload: dict[str, Any] = field(default_factory=dict)
    ts: str = ""


#: ``missions.status`` values a mission ends in; the store never moves a row out of one to a
#: non-terminal status (see ``MissionStore.upsert_mission``). Mirrors ``lha.durable.signals``.
TERMINAL_STATUSES = ("DONE", "IMPOSSIBLE", "ABORTED")


def terminal_guard_sql(existing: str, incoming: str, reopen: str) -> str:
    """SQL for the status an upsert stores: ``existing`` stays when it is terminal, the
    ``incoming`` status is not, and ``reopen`` (a boolean SQL expression) is false."""
    terminal = ", ".join(f"'{s}'" for s in TERMINAL_STATUSES)
    return (
        f"CASE WHEN {existing} IN ({terminal}) AND {incoming} NOT IN ({terminal}) "
        f"AND NOT {reopen} THEN {existing} ELSE {incoming} END"
    )


#: ``hitl_gates.status`` values. ``ESCALATED`` = open, at least one reminder sent.
GATE_OPEN = "OPEN"
GATE_ESCALATED = "ESCALATED"
GATE_RESOLVED = "RESOLVED"
GATE_DEFAULTED = "DEFAULTED"
#: Gate event -> the status it moves the row to.
GATE_EVENT_STATUS = {
    "opened": GATE_OPEN,
    "reminder": GATE_ESCALATED,
    "resolved": GATE_RESOLVED,
    "defaulted": GATE_DEFAULTED,
}


@dataclass
class GateEvent:
    """One human-gate event, as written to ``hitl_gates`` (``record_gate_event``).

    ``at`` is when it happened (ISO-8601 UTC). The durable workflow stamps it from workflow time,
    so a retried write carries the same value and the write is idempotent.
    """

    mission_id: str
    gate_id: str
    kind: str  # "tool_call" | "deadlock"
    event: str  # "opened" | "reminder" | "resolved" | "defaulted"
    at: str
    question: str = ""
    options: list[str] = field(default_factory=list)
    default_action: str = ""
    deadline: str = ""
    decision: str = ""
    resolved_by: str = ""
    step: int = 0
    risk: str = ""
    request: dict[str, str] | None = None


@dataclass
class GateRow:
    """One gate's current state (``hitl_gates``): ``opened_at`` is its latest opening."""

    mission_id: str
    gate_id: str
    kind: str
    status: str
    question: str = ""
    options: list[str] = field(default_factory=list)
    default_action: str = ""
    risk: str = ""
    deadline: str = ""
    decision: str | None = None
    resolved_by: str | None = None
    reminders: int = 0
    request: dict[str, str] | None = None
    opened_at: str = ""
    resolved_at: str = ""
    updated_at: str = ""


def validate_gate_event(event: GateEvent) -> str:
    """The row status ``event`` moves to (raises ``ValueError`` for an unknown event)."""
    try:
        return GATE_EVENT_STATUS[event.event]
    except KeyError:
        raise ValueError(f"unknown gate event {event.event!r}") from None


@runtime_checkable
class MissionStore(Protocol):
    """Backend-neutral persistence for missions, spend, and the memory tiers."""

    backend: str
    #: Non-empty when this store is a fallback for a configured-but-unusable backend.
    degraded_reason: str

    # --- missions -----------------------------------------------------------------------
    async def upsert_mission(
        self,
        *,
        mission_id: str,
        title: str,
        status: str,
        description: str = "",
        head_sha: str | None = None,
        workflow_id: str | None = None,
        reopen: bool = False,
    ) -> None:
        """Insert or update; ``None``/empty fields keep the stored values (never null them).

        Monotonic: a terminal status (``TERMINAL_STATUSES``) is never replaced by a non-terminal
        one, so a late write from a cycle that was still finishing when the mission ended
        (``lha mission-abort`` mid-cycle) cannot turn ``ABORTED`` back into ``RUNNING``. The
        other fields are still updated. ``reopen=True`` is the explicit exception, for a caller
        that deliberately resumes an ended mission under the same id.
        """
        ...

    async def get_mission(self, mission_id: str) -> MissionRow | None: ...

    async def list_missions(self, *, limit: int = 20) -> list[MissionRow]:
        """Most recently updated first."""
        ...

    # --- human gates --------------------------------------------------------------------
    async def record_gate_event(self, event: GateEvent) -> None:
        """Apply one gate event to its ``hitl_gates`` row (key: mission id + gate id).

        Idempotent: ``opened`` (re)opens the row unless it repeats the same opening (same ``at``);
        ``reminder`` raises the reminder count on an open row; ``resolved`` / ``defaulted``
        close an open row and are no-ops on a closed one. An event whose row is missing inserts
        it, so a lost earlier write never loses the outcome.
        """
        ...

    async def list_gates(self, mission_id: str | None = None, *, limit: int = 50) -> list[GateRow]:
        """Gates of ``mission_id`` (every mission when ``None``), most recently opened first."""
        ...

    # --- cost ledger --------------------------------------------------------------------
    async def record_cost(self, mission_id: str, entry: CostEntry, *, call_key: str) -> bool:
        """Insert one ledger row; ``False`` if this logical call (key) was already recorded."""
        ...

    async def list_costs(self, mission_id: str, *, limit: int = 200) -> list[CostRow]:
        """Oldest first (the most recent ``limit`` rows)."""
        ...

    async def cost_summary(self, mission_id: str) -> CostSummary: ...

    # --- episodic events ----------------------------------------------------------------
    async def append_event(
        self, mission_id: str, *, cycle_id: str, kind: str, payload: dict[str, Any]
    ) -> int: ...

    async def list_events(
        self,
        mission_id: str,
        *,
        kinds: tuple[str, ...] | None = None,
        after_id: int = 0,
        limit: int = 200,
    ) -> list[EventRow]:
        """The newest ``limit`` matching events with ``id > after_id``, oldest first."""
        ...

    async def append_mission_events(self, events: list[MissionEvent]) -> None:
        """Append to the shared event record, in order, in one transaction. An event without a
        ``ts`` is stamped with the time of the write."""
        ...

    async def read_mission_events(
        self, *, mission_id: str | None = None, after_id: int = 0, limit: int = 500
    ) -> list[EventRow]:
        """The OLDEST ``limit`` events with ``id > after_id`` (one mission, or all), oldest first:
        a reader pages forward from the last id it saw."""
        ...

    # --- semantic memory ----------------------------------------------------------------
    async def put_memory(
        self,
        mission_id: str,
        records: list[MemoryRecord],
        *,
        vectors: list[list[float]] | None = None,
        embedding_model: str = "none",
        embedding_version: str = "0",
    ) -> None:
        """Upsert records by id (with their vectors, when an embedder produced them)."""
        ...

    async def list_memory(self, mission_id: str, *, limit: int = 500) -> list[MemoryRecord]:
        """Valid records for the mission, newest ``limit``, oldest first."""
        ...

    async def invalidate_memory(self, ids: list[str]) -> int:
        """Soft-forget (``valid = false``); never deletes. Returns rows changed."""
        ...

    async def stale_memory(
        self,
        mission_id: str | None,
        *,
        embedding_model: str,
        embedding_version: str,
        limit: int = 100,
    ) -> list[tuple[str, MemoryRecord]]:
        """Valid rows (``mission_id``'s, or every mission's when ``None``) with no vector or a
        vector from another embedder model+version, oldest ``limit`` first, as
        ``(mission_id, record)``. The dense channel cannot see them until they are re-embedded."""
        ...

    async def count_stale_memory(
        self, mission_id: str | None, *, embedding_model: str, embedding_version: str
    ) -> dict[str, int]:
        """How many ``stale_memory`` rows each mission has (missions with none are left out)."""
        ...

    # --- skills -------------------------------------------------------------------------
    async def put_skill(self, skill: Skill) -> None:
        """Upsert a VERIFIED skill (unverified skills raise ``SkillNotVerifiedError``)."""
        ...

    async def list_skills(self, namespace: str, *, limit: int = 200) -> list[Skill]:
        """Skills in ``namespace`` plus ``global`` ones, newest first."""
        ...

    async def close(self) -> None: ...


# --- factory ---------------------------------------------------------------------------------
def _is_within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


#: The SQLite file name inside the per-user data directory.
SQLITE_FILE = "lha.sqlite3"
_warned_relative: set[str] = set()


def default_sqlite_path() -> Path:
    """The per-user SQLite store used when ``LHA_SQLITE_PATH`` is unset.

    ``$XDG_DATA_HOME/lha/lha.sqlite3`` when ``XDG_DATA_HOME`` is set (absolute), else
    ``~/Library/Application Support/lha/lha.sqlite3`` on macOS,
    ``%LOCALAPPDATA%/lha/lha.sqlite3`` on Windows and ``~/.local/share/lha/lha.sqlite3``
    elsewhere. Every process of one user (the CLI,
    the worker, ``lha missions``) therefore shares one store, wherever it was started from.
    """
    xdg = os.environ.get("XDG_DATA_HOME", "").strip()
    if xdg and Path(xdg).is_absolute():
        base = Path(xdg)
    elif sys.platform == "darwin":
        base = Path.home() / "Library" / "Application Support"
    elif sys.platform == "win32" and os.environ.get("LOCALAPPDATA"):
        base = Path(os.environ["LOCALAPPDATA"])
    else:
        base = Path.home() / ".local" / "share"
    return base / "lha" / SQLITE_FILE


def configured_sqlite_path(settings: Settings) -> Path:
    """``settings.sqlite_path`` made absolute, or ``default_sqlite_path()`` when it is empty.

    A relative ``LHA_SQLITE_PATH`` still resolves against this process's working directory, so
    processes started from different directories would use different stores: a warning says so
    (once per path and process).
    """
    configured = settings.sqlite_path.strip()
    if not configured:
        return default_sqlite_path()
    raw = Path(configured).expanduser()
    path = raw.resolve()
    if not raw.is_absolute() and configured not in _warned_relative:
        _warned_relative.add(configured)
        get_logger("lha.persistence").warning(
            "sqlite_path_relative",
            configured=configured,
            path=str(path),
            reason=(
                "LHA_SQLITE_PATH is relative: it resolves against the working directory, so "
                "processes started elsewhere use another store; set an absolute path"
            ),
        )
    return path


def resolve_sqlite_path(settings: Settings, *, workdir: str | Path | None = None) -> Path:
    """The SQLite store's absolute path (``configured_sqlite_path``); relocated under
    ``.git/lha/`` if it would land inside ``workdir`` (a mission checkout is reset/cleaned and
    committed — never put the DB there)."""
    path = configured_sqlite_path(settings)
    if workdir is not None:
        root = Path(workdir).expanduser().resolve()
        if _is_within(path, root):
            relocated = root / ".git" / "lha" / path.name
            get_logger("lha.persistence").warning(
                "sqlite_path_relocated",
                configured=str(path),
                path=str(relocated),
                reason="inside the mission checkout",
            )
            return relocated
    return path


def describe_store(settings: Settings) -> str:
    """Where ``open_store(settings)`` (no workdir) reads and writes, for humans."""
    from lha.model import secret_value

    if secret_value(settings.postgres_dsn):
        fallback = (
            f" (falls back to SQLite at {resolve_sqlite_path(settings)})"
            if settings.postgres_fallback_to_sqlite
            else ""
        )
        return f"postgres (LHA_POSTGRES_DSN){fallback}"
    return f"sqlite {resolve_sqlite_path(settings)}"


async def open_store(settings: Settings, *, workdir: str | Path | None = None) -> MissionStore:
    """Open the configured store: Postgres when ``postgres_dsn`` is set, else SQLite."""
    from lha.model import secret_value
    from lha.persistence.sqlite import SqliteStore

    dsn = secret_value(settings.postgres_dsn)
    degraded = ""
    if dsn:
        try:
            from lha.persistence.postgres import PostgresStore

            pg = PostgresStore(dsn)
            await pg.open()
            return pg
        except Exception as exc:  # driver missing / unreachable / unmigrated
            degraded = f"postgres unavailable ({type(exc).__name__}: {exc})"
            if not settings.postgres_fallback_to_sqlite:
                raise StoreUnavailableError(degraded) from exc
            get_logger("lha.persistence").warning("store_fallback_sqlite", reason=degraded)
    store = SqliteStore(resolve_sqlite_path(settings, workdir=workdir))
    await store.open()
    store.degraded_reason = degraded
    return store
