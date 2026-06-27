"""Apply SQL migrations to the app Postgres (requires the ``postgres`` extra).

Each ``db/migrations/NNNN_*.sql`` file is ONE migration, identified by its file stem (e.g.
``0001_init``). ``apply_migrations``:

  * takes a session-level advisory lock, so concurrent deployers/workers serialize;
  * records applied versions in ``schema_migrations`` and skips them on later runs;
  * executes each pending file as a single statement batch inside its own transaction (no
    parameters are passed, so psycopg sends the whole script unsplit — comments and semicolons in
    strings are safe), inserting its ``schema_migrations`` row in the same transaction: a failing
    migration leaves no partial schema and no record.

The migration files also insert their own ``schema_migrations`` row, so a database initialized by
Postgres' ``docker-entrypoint-initdb.d`` (which runs the files with ``psql``) is recognized as
already migrated.
"""

from __future__ import annotations

from contextlib import AbstractAsyncContextManager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

# Arbitrary, stable 64-bit key for pg_advisory_lock (ASCII "lhamigr8").
MIGRATION_LOCK_KEY = 0x6C68616D69677238

_CREATE_TABLE = (
    "CREATE TABLE IF NOT EXISTS schema_migrations ("
    "version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"
)


@dataclass(frozen=True)
class Migration:
    version: str  # file stem, e.g. "0001_init"
    path: Path

    def sql(self) -> str:
        return self.path.read_text(encoding="utf-8")


def discover_migrations(migrations_dir: str | Path) -> list[Migration]:
    """All ``*.sql`` files in ``migrations_dir``, ordered by name."""
    directory = Path(migrations_dir)
    if not directory.is_dir():
        raise FileNotFoundError(f"migrations directory not found: {directory}")
    return [Migration(p.stem, p) for p in sorted(directory.glob("*.sql"))]


class _Cursor(Protocol):
    async def fetchall(self) -> list[tuple[Any, ...]]: ...


class _Conn(Protocol):
    async def execute(self, query: Any, params: Any = ...) -> Any: ...

    def transaction(self) -> AbstractAsyncContextManager[Any]: ...


async def apply_pending(conn: _Conn, migrations: list[Migration]) -> list[str]:
    """Apply the not-yet-applied ``migrations`` over an open (autocommit) connection.

    Returns the versions applied by THIS call, in order.
    """
    await conn.execute("SELECT pg_advisory_lock(%s)", (MIGRATION_LOCK_KEY,))
    try:
        await conn.execute(_CREATE_TABLE)
        cursor = await conn.execute("SELECT version FROM schema_migrations")
        applied = {str(row[0]) for row in await cursor.fetchall()}
        newly: list[str] = []
        for migration in migrations:
            if migration.version in applied:
                continue
            async with conn.transaction():
                # No params => simple-query protocol: the whole file runs as one batch.
                await conn.execute(migration.sql())
                await conn.execute(
                    "INSERT INTO schema_migrations (version) VALUES (%s) ON CONFLICT DO NOTHING",
                    (migration.version,),
                )
            newly.append(migration.version)
        return newly
    finally:
        await conn.execute("SELECT pg_advisory_unlock(%s)", (MIGRATION_LOCK_KEY,))


async def apply_migrations(dsn: str, *, migrations_dir: str | Path = "db/migrations") -> list[str]:
    """Apply every pending migration in ``migrations_dir`` to ``dsn``.

    Returns the versions (file stems) newly applied by this call — ``[]`` when up to date.
    """
    import psycopg

    migrations = discover_migrations(migrations_dir)
    async with await psycopg.AsyncConnection.connect(dsn, autocommit=True) as conn:
        return await apply_pending(conn, migrations)
