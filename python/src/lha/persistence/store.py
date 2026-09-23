"""The mission store: one interface, two backends (SQLite by default, Postgres with a DSN).

Everything a run persists outside git goes through ``MissionStore``:

* ``missions`` — one row per mission with its status transitions (RUNNING → DONE / ABORTED / ...);
* ``cost_ledger`` — EVERY metered model call (``usd`` is NULL when the cost is unknown), written
  idempotently by key so a retried/replayed write never double-counts;
* ``episodic_events`` — cycle outcomes and memory bookkeeping (the episodic memory tier);
* ``semantic_memory`` — distilled facts / progress notes, optionally with embeddings;
* ``skills`` — verified, reusable know-how (the procedural tier).

``open_store(settings)`` picks the backend: ``LHA_POSTGRES_DSN`` set → ``PostgresStore`` (tables
from ``db/migrations``; run ``lha db migrate`` first), otherwise ``SqliteStore`` (stdlib
``sqlite3``, WAL mode, schema created on open). If Postgres is configured but unusable, the run
falls back to SQLite (``settings.postgres_fallback_to_sqlite``) and the store says so via
``degraded_reason``.
"""

from __future__ import annotations

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
    ) -> None:
        """Insert or update; ``None``/empty fields keep the stored values (never null them)."""
        ...

    async def get_mission(self, mission_id: str) -> MissionRow | None: ...

    async def list_missions(self, *, limit: int = 20) -> list[MissionRow]:
        """Most recently updated first."""
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


def resolve_sqlite_path(settings: Settings, *, workdir: str | Path | None = None) -> Path:
    """``settings.sqlite_path`` made absolute; relocated under ``.git/lha/`` if it would land
    inside ``workdir`` (a mission checkout is reset/cleaned and committed — never put the DB
    there)."""
    path = Path(settings.sqlite_path).expanduser().resolve()
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
