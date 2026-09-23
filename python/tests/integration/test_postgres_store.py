"""The Postgres ``MissionStore``, pgvector-backed memory, and a run path on a REAL Postgres.

Gated on ``LHA_IT_POSTGRES_DSN`` (see ``conftest.py``); needs the ``postgres`` extra.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest
from pydantic import SecretStr

from lha.config import Settings
from lha.contracts.memory import MemoryRecord
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.contracts.verify import Check
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill, SkillNotVerifiedError
from lha.model.stub import StubModel
from lha.state import git_ops
from tests.integration.conftest import requires_postgres

pytestmark = [pytest.mark.integration, requires_postgres]
psycopg = pytest.importorskip("psycopg")
pytest.importorskip("pgvector")

MIGRATIONS = Path(__file__).resolve().parents[3] / "db" / "migrations"


async def _migrate(dsn: str) -> None:
    from lha.persistence.db import apply_migrations

    await apply_migrations(dsn, migrations_dir=MIGRATIONS)


def _settings(dsn: str, tmp_path: Path, **overrides: object) -> Settings:
    base: dict[str, object] = {
        "postgres_dsn": SecretStr(dsn),
        "sqlite_path": str(tmp_path / "fallback.sqlite3"),
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "max_turns_per_cycle": 1,
    }
    base.update(overrides)
    return Settings(_env_file=None, **base)  # type: ignore[call-arg, arg-type]


async def test_postgres_store_round_trips_everything(pg_dsn: str) -> None:
    from lha.persistence.postgres import PostgresStore

    await _migrate(pg_dsn)
    store = PostgresStore(pg_dsn)
    await store.open()
    try:
        # missions
        await store.upsert_mission(mission_id="m1", title="T", description="D", status="RUNNING")
        await store.upsert_mission(mission_id="m1", title="", status="RUNNING", head_sha="abc")
        await store.upsert_mission(mission_id="m1", title="", status="DONE")
        row = await store.get_mission("m1")
        assert row is not None
        assert (row.title, row.description, row.status, row.head_sha) == ("T", "D", "DONE", "abc")
        assert [m.mission_id for m in await store.list_missions()] == ["m1"]

        # cost ledger: idempotent, unknown cost is NULL
        entry = CostEntry(cycle_id="c1", model="m", input_tokens=10, output_tokens=5, usd=0.25)
        assert await store.record_cost("m1", entry, call_key="k#0") is True
        assert await store.record_cost("m1", entry, call_key="k#0") is False
        unknown = entry.model_copy(update={"usd": 3.0, "cost_known": False})
        assert await store.record_cost("m1", unknown, call_key="k#1") is True
        summary = await store.cost_summary("m1")
        assert (summary.calls, summary.unknown_cost_calls) == (2, 1)
        assert summary.known_usd == pytest.approx(0.25)
        rows = await store.list_costs("m1")
        assert rows[1].usd is None and not rows[1].cost_known

        # episodic events
        for i in range(4):
            await store.append_event(
                "m1", cycle_id=f"c{i}", kind="k" if i % 2 else "j", payload={"i": i}
            )
        assert [e.payload["i"] for e in await store.list_events("m1", kinds=("k",))] == [1, 3]
        assert [e.payload["i"] for e in await store.list_events("m1", limit=2)] == [2, 3]

        # semantic memory (text rows) + soft invalidation
        await store.put_memory(
            "m1", [MemoryRecord(id="a", kind="fact", text="alpha", metadata={"x": "1"})]
        )
        listed = await store.list_memory("m1")
        assert [(r.id, r.kind, r.metadata) for r in listed] == [("a", "fact", {"x": "1"})]
        assert await store.invalidate_memory(["a"]) == 1
        assert await store.list_memory("m1") == []

        # skills
        skill = Skill(id="s", name="n", description="d", code="c", namespace="/r", verified=True)
        await store.put_skill(skill)
        assert await store.list_skills("/r") == [skill]
        with pytest.raises(SkillNotVerifiedError):
            await store.put_skill(skill.model_copy(update={"verified": False}))
    finally:
        await store.close()


async def test_open_store_uses_postgres_and_falls_back_when_unmigrated(
    pg_dsn: str, tmp_path: Path
) -> None:
    from lha.persistence.store import BACKEND_POSTGRES, BACKEND_SQLITE, open_store

    unmigrated = await open_store(_settings(pg_dsn, tmp_path))
    try:
        assert unmigrated.backend == BACKEND_SQLITE
        assert "lha db migrate" in unmigrated.degraded_reason
    finally:
        await unmigrated.close()

    await _migrate(pg_dsn)
    store = await open_store(_settings(pg_dsn, tmp_path))
    try:
        assert store.backend == BACKEND_POSTGRES and store.degraded_reason == ""
    finally:
        await store.close()


async def test_pgvector_memory_recall_skills_and_mission_scoping(
    pg_dsn: str, tmp_path: Path
) -> None:
    from lha.memory.service import CycleObservation, open_mission_memory
    from lha.persistence.store import open_store

    await _migrate(pg_dsn)
    settings = _settings(pg_dsn, tmp_path)
    store = await open_store(settings)
    ws = tmp_path / "ws"
    ws.mkdir()
    git_ops.init_repo(ws)
    (ws / "config.py").write_text("PORT = 8080\n", encoding="utf-8")
    git_ops.commit_all(ws, "init")
    memory = await open_mission_memory(settings, store=store, workdir=ws, mission_id="m1")
    assert memory is not None
    try:
        assert memory.mode.label == "hybrid" and memory.embedder is not None
        assert memory.embedder.dim == 1024
        for mission, cycle, verified in (("m1", "c1", False), ("m2", "c1", True)):
            await memory.observe_cycle(
                CycleObservation(
                    mission_id=mission,
                    cycle_id=cycle,
                    item_id="01",
                    item_description="make the port configurable",
                    verdict="passed" if verified else "failed",
                    verified=verified,
                    status="done" if verified else "in_progress",
                    attempts=1,
                    head_sha="",
                    failure="" if verified else "KeyError: PORT",
                    done_summary="read PORT from the environment",
                    tools=["write_file"],
                )
            )
        # Vectors were written through pgvector, stamped with the embedder model+version.
        async with await psycopg.AsyncConnection.connect(pg_dsn, autocommit=True) as conn:
            cur = await conn.execute(
                "SELECT count(*) FROM semantic_memory WHERE embedding IS NOT NULL "
                "AND embedding_model = %s",
                (memory.embedder.name,),
            )
            assert (await cur.fetchone())[0] == 2  # type: ignore[index]

        block = await memory.recall(
            mission_id="m1",
            cycle_id="c2",
            item=ChecklistItem(id="02", description="make the port configurable via env"),
            snapshot=SituationSnapshot(head_sha=""),
        )
        assert "c1 [01] failed" in block  # m1's own episode
        assert "read PORT from the environment" in block  # the skill learned in m2 (same repo)
        assert "KeyError: PORT" in block
        assert "c1 [01] passed" not in block  # m2 episodes/progress are scoped to m2
    finally:
        await memory.close()
        await store.close()


async def test_run_local_on_postgres_persists_mission_and_ledger(
    pg_dsn: str, tmp_path: Path
) -> None:
    from lha.agent.runner import run_mission_local
    from lha.persistence.postgres import PostgresStore

    await _migrate(pg_dsn)
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="pg run",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[Check(name="green", command=[sys.executable, "-c", "pass"])],
        settings=_settings(pg_dsn, tmp_path),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "did it"}')]),
    )
    assert summary.completed
    store = PostgresStore(pg_dsn)
    await store.open()
    try:
        row = await store.get_mission(summary.mission_id)
        assert row is not None and row.status == "DONE"
        assert (await store.cost_summary(summary.mission_id)).calls == 1
        assert await store.list_events(summary.mission_id, kinds=("cycle_outcome",))
        assert await store.list_skills(str((tmp_path / "ws").resolve()))
    finally:
        await store.close()
    assert not (tmp_path / "fallback.sqlite3").exists()  # nothing fell back to SQLite
