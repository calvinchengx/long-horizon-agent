"""Regression tests for the durable/memory/verify/obs/ops/persistence review fixes."""

from __future__ import annotations

import sys
import types
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.embeddings import HashEmbedder
from lha.memory.semantic_pg import PgSemanticIndex
from lha.obs.redact import REDACTED, is_secret_key, redact_mapping, redact_text
from lha.ops.degradation import DependencyStatus, Health, decide_safe_park
from lha.verify.harness_integrity import harness_violations, snapshot_harness

MIGRATIONS = Path(__file__).resolve().parents[3] / "db" / "migrations"


# --- redaction -----------------------------------------------------------------------------
@pytest.mark.parametrize(
    ("text", "secret", "expected"),
    [
        ("redis://:hunter2secret@localhost:6379/0", "hunter2secret", "redis://:***@localhost"),
        ("postgresql://u:p@ss@host/db", "ss@host", "postgresql://u:***@host/db"),
        ("postgresql://admin:s3cret@db:5432/lha", "s3cret", "postgresql://admin:***@db:5432"),
        ("tok github_pat_11ABCDEFG0123456789_abcdefghijklmnop", "11ABCDEFG", "tok ***"),
        ("Authorization: Basic dXNlcjpodW50ZXIy", "dXNlcjpodW50ZXIy", "Authorization: Basic ***"),
        ("authorization=Bearer eyJhbGciOi.x.y", "eyJhbGciOi", "authorization=Bearer ***"),
        ("env x_sk-ant-api03-abcdefghijklmnopqrstu", "abcdefghijklmnop", "x_***"),
    ],
)
def test_redact_text_catches_previously_missed_secrets(
    text: str, secret: str, expected: str
) -> None:
    out = redact_text(text)
    assert secret not in out
    assert expected in out


@pytest.mark.parametrize(
    "text",
    [
        "see http://host:8080/path?email=a@b.com",
        "ssh://git@github.com:22/repo",
        "a basic setup of the task-abcdefghijklmnopqrstuv",
    ],
)
def test_redact_text_leaves_non_secrets_alone(text: str) -> None:
    assert redact_text(text) == text


def test_camel_case_secret_keys_redacted_but_counters_kept() -> None:
    out = redact_mapping(
        {
            "accessToken": "a",
            "refreshToken": "b",
            "clientSecret": "c",
            "userPassword": "d",
            "inputTokens": 5,
            "maxTokens": 9,
        }
    )
    assert out["accessToken"] == out["refreshToken"] == REDACTED
    assert out["clientSecret"] == out["userPassword"] == REDACTED
    assert out["inputTokens"] == 5 and out["maxTokens"] == 9
    assert not is_secret_key("inputTokens")


# --- harness integrity -----------------------------------------------------------------------
@pytest.mark.parametrize(
    "rel",
    [
        "tests/test_a.py",
        "src/pkg/tests/test_a.py",
        "src/pkg/test/helpers.py",
        "test_app.py",
        "pkg/test_models.py",
        "pkg/models_test.py",
        "pkg/conftest.py",
        "noxfile.py",
        "sub/tox.ini",
        "pytest.ini",
        "pyproject.toml",
        "setup.cfg",
    ],
)
def test_harness_files_protected_anywhere(tmp_path: Path, rel: str) -> None:
    path = tmp_path / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("def test_x():\n    assert True\n", encoding="utf-8")
    before = snapshot_harness(tmp_path)
    assert rel in before
    path.write_text("def test_x():\n    pass\n", encoding="utf-8")
    assert harness_violations(before, snapshot_harness(tmp_path)) == [f"modified: {rel}"]


@pytest.mark.parametrize("rel", ["src/pkg/core.py", "src/pkg/testing_utils.py", "README.md"])
def test_non_harness_files_not_protected(tmp_path: Path, rel: str) -> None:
    path = tmp_path / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("x = 1\n", encoding="utf-8")
    assert rel not in snapshot_harness(tmp_path)


# --- degradation -----------------------------------------------------------------------------
def test_sandbox_outage_parks_the_mission() -> None:
    decision = decide_safe_park(
        [
            DependencyStatus("git", Health.OK),
            DependencyStatus("model", Health.OK),
            DependencyStatus("sandbox", Health.DOWN, "docker unreachable"),
        ]
    )
    assert decision.park
    assert "sandbox" in decision.reason


# --- pgvector parameter cast -------------------------------------------------------------------
class _Cursor:
    async def fetchall(self) -> list[tuple[Any, ...]]:
        return []


class _Conn:
    def __init__(self, calls: list[tuple[str, tuple[Any, ...]]]) -> None:
        self._calls = calls

    async def execute(self, sql: str, params: tuple[Any, ...]) -> _Cursor:
        self._calls.append((sql, params))
        return _Cursor()


class _ConnCtx:
    def __init__(self, calls: list[tuple[str, tuple[Any, ...]]]) -> None:
        self._conn = _Conn(calls)

    async def __aenter__(self) -> _Conn:
        return self._conn

    async def __aexit__(self, *exc: object) -> None:
        return None


class _Pool:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple[Any, ...]]] = []

    def connection(self) -> _ConnCtx:
        return _ConnCtx(self.calls)


@pytest.mark.asyncio
async def test_pg_vector_params_are_cast_to_vector() -> None:
    pool = _Pool()
    index = PgSemanticIndex(dsn="unused", embedder=HashEmbedder(dim=8), column_dim=8, pool=pool)
    await index.add([MemoryRecord(id="fact_abc", kind="semantic", text="postgres")])
    await index.query("postgres", k=1)
    insert_sql, select_sql = pool.calls[0][0], pool.calls[1][0]
    assert "%s::vector" in insert_sql
    assert select_sql.count("%s::vector") == 2  # score expression + ORDER BY


# --- migrations ---------------------------------------------------------------------------------
def test_semantic_memory_ids_are_text_after_migrations() -> None:
    sql = "\n".join(p.read_text(encoding="utf-8") for p in sorted(MIGRATIONS.glob("*.sql")))
    assert "ALTER TABLE semantic_memory ALTER COLUMN id TYPE text" in sql


def test_cost_ledger_usd_nullable_migration_present() -> None:
    sql = (MIGRATIONS / "0003_cost_unknown_usd_null.sql").read_text(encoding="utf-8")
    assert "ALTER COLUMN usd DROP NOT NULL" in sql
    assert "'0003_cost_unknown_usd_null'" in sql


# --- cost ledger repo: unknown cost is NULL, not $0 ----------------------------------------------
class _RecordingConn:
    def __init__(self, sink: list[tuple[str, tuple[Any, ...]]]) -> None:
        self._sink = sink

    async def __aenter__(self) -> _RecordingConn:
        return self

    async def __aexit__(self, *exc: object) -> None:
        return None

    async def execute(self, sql: str, params: tuple[Any, ...]) -> Any:
        self._sink.append((sql, params))
        return types.SimpleNamespace(rowcount=1)


@pytest.fixture
def fake_psycopg(monkeypatch: pytest.MonkeyPatch) -> Iterator[list[tuple[str, tuple[Any, ...]]]]:
    sink: list[tuple[str, tuple[Any, ...]]] = []

    class _AsyncConnection:
        @staticmethod
        async def connect(dsn: str, autocommit: bool = False) -> _RecordingConn:
            return _RecordingConn(sink)

    fake = types.ModuleType("psycopg")
    fake.AsyncConnection = _AsyncConnection  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "psycopg", fake)
    monkeypatch.delitem(sys.modules, "lha.persistence.repositories", raising=False)
    yield sink
    # Never leak a repositories module bound to the fake psycopg into other tests.
    sys.modules.pop("lha.persistence.repositories", None)


@pytest.mark.asyncio
async def test_unknown_cost_stored_as_null(fake_psycopg: list[tuple[str, tuple[Any, ...]]]) -> None:
    from lha.persistence.repositories import CostLedgerRepo

    repo = CostLedgerRepo("postgresql://unused")
    unknown = CostEntry(
        cycle_id="c1", model="m", input_tokens=1, output_tokens=1, usd=0.0, cost_known=False
    )
    known = CostEntry(cycle_id="c1", model="m", input_tokens=1, output_tokens=1, usd=0.25)
    await repo.record("m1", unknown, call_key=1)
    await repo.record("m1", known, call_key=2)
    (_, p_unknown), (_, p_known) = fake_psycopg
    assert p_unknown[5] is None and p_unknown[7] is False  # usd, cost_known
    assert p_known[5] == 0.25 and p_known[7] is True


@pytest.mark.asyncio
async def test_mission_upsert_keeps_head_sha(
    fake_psycopg: list[tuple[str, tuple[Any, ...]]],
) -> None:
    from lha.persistence.repositories import MissionRepo

    await MissionRepo("postgresql://unused").upsert(mission_id="m1", title="t", status="SLEEPING")
    sql, _ = fake_psycopg[0]
    assert "COALESCE(EXCLUDED.head_sha, missions.head_sha)" in sql
