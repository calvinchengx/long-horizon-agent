"""Fixtures for tests that run against REAL services (opt-in, never in the default unit run).

* ``LHA_IT_POSTGRES_DSN`` — an admin DSN for a Postgres with the ``vector`` extension available
  (e.g. ``postgresql://lha:lha@127.0.0.1:55432/lha`` for ``pgvector/pgvector:pg16``). Each test
  gets a fresh, uniquely named database that is dropped afterwards.
* ``LHA_IT_DOCKER=1`` — run the Docker sandbox tests against the local Docker daemon (they pull
  ``python:3.12-slim`` on first use).

Tests whose variable is unset are skipped, so ``pytest tests/integration`` is always safe to run.
"""

from __future__ import annotations

import os
import uuid
from collections.abc import AsyncIterator
from urllib.parse import urlsplit, urlunsplit

import pytest

POSTGRES_DSN = os.environ.get("LHA_IT_POSTGRES_DSN", "")
DOCKER_ENABLED = os.environ.get("LHA_IT_DOCKER", "") == "1"

requires_postgres = pytest.mark.skipif(not POSTGRES_DSN, reason="set LHA_IT_POSTGRES_DSN")
requires_docker = pytest.mark.skipif(not DOCKER_ENABLED, reason="set LHA_IT_DOCKER=1")


def _with_database(dsn: str, name: str) -> str:
    parts = urlsplit(dsn)
    return urlunsplit(parts._replace(path=f"/{name}"))


@pytest.fixture
async def pg_dsn() -> AsyncIterator[str]:
    """A DSN for a brand-new empty database, dropped when the test ends."""
    psycopg = pytest.importorskip("psycopg")
    name = f"lha_it_{uuid.uuid4().hex[:12]}"
    async with await psycopg.AsyncConnection.connect(POSTGRES_DSN, autocommit=True) as admin:
        await admin.execute(f'CREATE DATABASE "{name}"')
    try:
        yield _with_database(POSTGRES_DSN, name)
    finally:
        async with await psycopg.AsyncConnection.connect(POSTGRES_DSN, autocommit=True) as admin:
            await admin.execute(f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')
