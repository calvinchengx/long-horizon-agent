"""Every run path persists the mission row (status transitions) and EVERY metered model call:
``run-local`` / ``mission`` (local runner + Planner), ``orchestrate`` (Orchestrator), the Temporal
cycle + sub-agent activities, ``mission-start`` — and the ``missions`` / ``costs`` read commands."""

from __future__ import annotations

import asyncio
import sys
from pathlib import Path

import pytest
from temporalio.exceptions import ApplicationError
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agent.runner import build_meter, plan_and_run_local, run_mission_local
from lha.agents.orchestrator import Orchestrator
from lha.config import Settings, get_settings
from lha.contracts.model import ModelMessage, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.contracts.verify import Check
from lha.durable import activities as acts
from lha.durable.agent_activities import run_subagent
from lha.durable.types import ERROR_BUDGET_EXCEEDED, CycleInput, SubAgentInput
from lha.governor.cost import CostEntry
from lha.model.stub import StubModel
from lha.persistence.sqlite import SqliteStore
from lha.state.mission_anchor import GitMissionAnchor

_PASS = Check(name="green", command=[sys.executable, "-c", "pass"])
_FAIL = Check(name="red", command=[sys.executable, "-c", "raise SystemExit(1)"])
_DONE = TurnResult(text='{"done": true, "summary": "ok"}')


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 20,
        "max_turns_per_cycle": 2,
        "stall_limit": 50,
    }
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


async def _store(path: Path) -> SqliteStore:
    store = SqliteStore(path)
    await store.open()
    return store


def _items(n: int = 1) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id=f"{i:02d}", description=f"thing {i}") for i in range(1, n + 1)]
    )


# --- local runner (lha run-local / lha mission) -----------------------------------------------
async def test_run_local_persists_mission_and_every_model_call(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    meter = build_meter(_settings())
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="Persisted",
        description="desc",
        checklist=_items(2),
        checks=[_PASS],
        settings=_settings(),
        model=StubModel(script=[_DONE]),
        meter=meter,
    )
    assert summary.completed
    store = await _store(_isolated_mission_store)
    row = await store.get_mission(summary.mission_id)
    assert row is not None
    assert (row.status, row.title, row.description) == ("DONE", "Persisted", "desc")
    assert row.head_sha == summary.head_sha
    rows = await store.list_costs(summary.mission_id)
    assert len(rows) == len(meter.ledger.entries) == 2
    assert [r.cycle_id for r in rows] == ["c1", "c2"] and {r.role for r in rows} == {"lead"}
    assert all(r.usd == 0.0 and r.cost_known for r in rows)  # a stub is genuinely $0
    await store.close()


async def test_deadlock_and_crash_record_terminal_statuses(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    deadlocked = await run_mission_local(
        workdir=str(tmp_path / "a"),
        title="t",
        description="d",
        checklist=_items(),
        checks=[_FAIL],
        settings=_settings(),
        model=StubModel(script=[_DONE]),
    )

    class _Crash(StubModel):
        async def complete(self, messages: list[ModelMessage], **kwargs: object) -> TurnResult:
            raise RuntimeError("provider exploded")

    with pytest.raises(RuntimeError, match="provider exploded"):
        await run_mission_local(
            workdir=str(tmp_path / "b"),
            title="crash",
            description="d",
            checklist=_items(),
            checks=[_PASS],
            settings=_settings(),
            model=_Crash(),
        )
    store = await _store(_isolated_mission_store)
    rows = {m.title: m for m in await store.list_missions()}
    assert rows["t"].mission_id == deadlocked.mission_id and rows["t"].status == "IMPOSSIBLE"
    assert rows["crash"].status == "ABORTED"
    await store.close()


async def test_planner_spend_is_backfilled_into_the_mission_ledger(
    tmp_path: Path, _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    plan = TurnResult(text='[{"id": "01", "description": "only item"}]')
    monkeypatch.setattr(
        "lha.agent.runner.build_provider", lambda _s: StubModel(script=[plan, _DONE])
    )
    summary = await plan_and_run_local(
        workdir=str(tmp_path / "ws"), title="p", task="t", checks=[_PASS], settings=_settings()
    )
    store = await _store(_isolated_mission_store)
    roles = [r.role for r in await store.list_costs(summary.mission_id)]
    assert roles[0] == "planner" and "lead" in roles
    await store.close()


# --- orchestrator (lha orchestrate) -----------------------------------------------------------
async def test_orchestrator_persists_every_role_call(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    approve = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')
    settings = _settings()
    meter = build_meter(settings)
    summary = await Orchestrator(
        settings,
        meter=meter,
        research_per_item=1,
        do_review=True,
        models={
            "lead": StubModel(script=[_DONE]),
            "researcher": StubModel(script=[_DONE]),
            "reviewer": StubModel(script=[approve]),
        },
    ).run_mission(
        workdir=str(tmp_path / "ws"),
        title="org",
        description="d",
        checklist=_items(),
        checks=[_PASS],
    )
    assert summary.completed
    store = await _store(_isolated_mission_store)
    assert (await store.get_mission(summary.mission_id)).status == "DONE"  # type: ignore[union-attr]
    rows = await store.list_costs(summary.mission_id)
    assert len(rows) == len(meter.ledger.entries)
    assert {"lead", "researcher", "reviewer"} <= {r.role for r in rows}
    await store.close()


# --- Temporal activities ----------------------------------------------------------------------
async def _anchor(repo: Path) -> None:
    await GitMissionAnchor(repo).initialize(title="Durable", description="D", items=_items())


class _Priced(StubModel):
    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.5


async def test_cycle_activity_persists_status_and_spend(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    await _anchor(tmp_path)
    settings = _settings(budget_usd_ceiling=1000.0)
    result = await acts._execute_cycle(
        CycleInput(
            mission_id="dm",
            workdir=str(tmp_path),
            cycle_id="c1",
            check_commands=[[sys.executable, "-c", "pass"]],
        ),
        settings=settings,
        model_factory=lambda _s, _snap: _Priced(script=[_DONE]),
    )
    assert result.is_complete
    store = await _store(_isolated_mission_store)
    row = await store.get_mission("dm")
    assert row is not None and row.status == "DONE" and row.title == "Durable"
    assert row.head_sha == result.head_sha
    rows = await store.list_costs("dm")
    assert [(r.cycle_id, r.role, r.usd) for r in rows] == [("c1", "lead", 0.5)]
    # The seeded "(prior)" spend of earlier attempts is never re-written as a new row.
    assert all(r.model != "(prior)" for r in rows)
    await store.close()


async def test_cycle_activity_budget_refusal_marks_the_mission_aborted(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    await _anchor(tmp_path)
    with pytest.raises(ApplicationError) as err:
        await acts._execute_cycle(
            CycleInput(mission_id="poor", workdir=str(tmp_path), cycle_id="c1", budget_usd=0.01),
            settings=_settings(),
            model_factory=lambda _s, _snap: _Priced(script=[_DONE]),
        )
    assert err.value.type == ERROR_BUDGET_EXCEEDED
    store = await _store(_isolated_mission_store)
    assert (await store.get_mission("poor")).status == "ABORTED"  # type: ignore[union-attr]
    await store.close()


async def test_cycle_activity_recalls_memory_across_activity_attempts(tmp_path: Path) -> None:
    await _anchor(tmp_path)
    prompts: list[list[ModelMessage]] = []

    class _Seen(StubModel):
        async def complete(self, messages: list[ModelMessage], **kwargs: object) -> TurnResult:
            prompts.append(list(messages))
            return await super().complete(messages)

    for cycle in ("c1", "c2"):
        result = await acts._execute_cycle(
            CycleInput(
                mission_id="mem",
                workdir=str(tmp_path),
                cycle_id=cycle,
                check_commands=[[sys.executable, "-c", "raise SystemExit(1)"]],
            ),
            settings=_settings(),
            model_factory=lambda _s, _snap: _Seen(script=[_DONE]),
        )
        assert result.verdict == "failed"
    # Each activity opens its own store + memory; cycle 2 still recalls cycle 1's attempt.
    assert "c1 [01] failed" not in prompts[0][1].content
    assert "c1 [01] failed" in prompts[-1][1].content


async def test_cycle_activity_unusable_store_without_fallback_is_a_config_error(
    tmp_path: Path,
) -> None:
    from pydantic import SecretStr

    from lha.durable.types import ERROR_CONFIG

    await _anchor(tmp_path)
    strict = _settings(
        postgres_dsn=SecretStr("postgresql://u:p@127.0.0.1:1/none?connect_timeout=1"),
        postgres_fallback_to_sqlite=False,
    )
    with pytest.raises(ApplicationError) as err:
        await acts._execute_cycle(
            CycleInput(mission_id="x", workdir=str(tmp_path), cycle_id="c1"),
            settings=strict,
            model_factory=lambda _s, _snap: StubModel(script=[_DONE]),
        )
    assert err.value.type == ERROR_CONFIG and err.value.non_retryable
    assert "mission store" in str(err.value)


def test_cycle_status_mapping() -> None:
    base = {"head_sha": "h"}
    assert acts.cycle_status(SituationSnapshot(**base, is_complete=True)) == "DONE"  # type: ignore[arg-type]
    assert acts.cycle_status(SituationSnapshot(**base, is_deadlocked=True)) == "IMPOSSIBLE"  # type: ignore[arg-type]
    assert acts.cycle_status(SituationSnapshot(**base)) == "RUNNING"  # type: ignore[arg-type]
    waiting = acts.cycle_status(SituationSnapshot(**base), awaiting_approval=True)  # type: ignore[arg-type]
    assert waiting == "WAITING_ON_HUMAN"
    # Completion wins over a queued approval (the workflow ends the mission as completed).
    done = acts.cycle_status(SituationSnapshot(**base, is_complete=True), awaiting_approval=True)  # type: ignore[arg-type]
    assert done == "DONE"


async def test_cycle_activity_marks_a_gated_action_as_waiting_on_human(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    from lha.contracts.model import ToolCall

    await _anchor(tmp_path)
    push = TurnResult(
        text="",
        tool_calls=[ToolCall(id="p", name="run_command", arguments={"argv": ["git", "push"]})],
    )
    result = await acts._execute_cycle(
        CycleInput(
            mission_id="gated",
            workdir=str(tmp_path),
            cycle_id="c1",
            check_commands=[[sys.executable, "-c", "raise SystemExit(1)"]],
        ),
        settings=_settings(),
        model_factory=lambda _s, _snap: StubModel(script=[push, _DONE]),
    )
    assert result.pending_approvals
    store = await _store(_isolated_mission_store)
    assert (await store.get_mission("gated")).status == "WAITING_ON_HUMAN"  # type: ignore[union-attr]
    await store.close()


async def test_subagent_activity_writes_its_spend_to_the_mission_ledger(
    tmp_path: Path, _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("LHA_SANDBOX", "local")
    monkeypatch.setenv("LHA_ALLOW_UNSAFE_LOCAL", "true")
    get_settings.cache_clear()
    monkeypatch.setattr(
        "lha.durable.agent_activities.build_provider",
        lambda _s: StubModel(script=[TurnResult(text='{"done": true, "summary": "brief"}')]),
    )
    out = await run_subagent(
        SubAgentInput(
            role_name="researcher", objective="look", workdir=str(tmp_path), mission_id="pm"
        )
    )
    assert out.role == "researcher"
    store = await _store(_isolated_mission_store)
    rows = await store.list_costs("pm")
    assert rows and {(r.cycle_id, r.role) for r in rows} == {("subagent:researcher", "researcher")}
    await store.close()


# --- CLI read commands ------------------------------------------------------------------------
runner = CliRunner()


async def _seed(path: Path) -> None:
    store = await _store(path)
    await store.upsert_mission(mission_id="m1", title="First", status="DONE", head_sha="abc123")
    entry = CostEntry(cycle_id="c1", model="claude-x", input_tokens=100, output_tokens=20, usd=0.12)
    await store.record_cost("m1", entry.model_copy(update={"role": "lead"}), call_key="k#0")
    unknown = entry.model_copy(update={"usd": 0.0, "cost_known": False, "role": "researcher"})
    await store.record_cost("m1", unknown, call_key="k#1")
    await store.close()


def test_cli_missions_and_costs_read_the_store(_isolated_mission_store: Path) -> None:
    asyncio.run(_seed(_isolated_mission_store))
    listed = runner.invoke(cli.app, ["missions"])
    assert listed.exit_code == 0, listed.output
    assert "m1" in listed.output and "DONE" in listed.output and "First" in listed.output
    assert "$0.1200 (+1 unknown-cost)" in listed.output and "calls 2" in listed.output

    costs = runner.invoke(cli.app, ["costs", "m1"])
    assert costs.exit_code == 0, costs.output
    assert "lead" in costs.output and "researcher" in costs.output
    assert "unknown" in costs.output  # an unpriced call is shown as unknown, never $0
    assert "total: 2 calls  known $0.1200  unknown-cost calls 1" in costs.output

    summary_only = runner.invoke(cli.app, ["costs", "m1", "--limit", "0"])
    assert summary_only.output.strip().startswith("total:")
    missing = runner.invoke(cli.app, ["costs", "nope"])
    assert missing.exit_code == 1 and "no cost ledger rows" in missing.output


def test_cli_missions_on_an_empty_store() -> None:
    result = runner.invoke(cli.app, ["missions"])
    assert result.exit_code == 0 and "no missions recorded" in result.output


def test_mission_start_records_the_mission_row_and_planner_spend(
    tmp_path: Path, _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    settings = Settings(model_backend="stub")
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    started: list[str] = []

    class _Client:
        async def start_workflow(self, _run: object, inp: object, **kw: object) -> None:
            started.append(str(kw["id"]))

    async def connect_client(_settings: Settings) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)
    result = runner.invoke(
        cli.app, ["mission-start", "--task", "do it", "--title", "dur", "--workdir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    mission_id = started[0].removeprefix("mission:")

    async def _check() -> None:
        store = await _store(_isolated_mission_store)
        row = await store.get_mission(mission_id)
        assert row is not None
        assert (row.status, row.title, row.workflow_id) == ("RUNNING", "dur", started[0])
        assert [r.role for r in await store.list_costs(mission_id)] == ["planner"]
        await store.close()

    asyncio.run(_check())


def _fake_temporal(monkeypatch: pytest.MonkeyPatch, *, fail: bool = False) -> list[str]:
    settings = Settings(model_backend="stub")
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    started: list[str] = []

    class _Client:
        async def start_workflow(self, _run: object, inp: object, **kw: object) -> None:
            started.append(str(kw["id"]))
            if fail:
                raise RuntimeError("temporal said no")

    async def connect_client(_settings: Settings) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)
    return started


def _row_status(path: Path, mission_id: str) -> str | None:
    async def _read() -> str | None:
        store = await _store(path)
        try:
            row = await store.get_mission(mission_id)
            return row.status if row else None
        finally:
            await store.close()

    return asyncio.run(_read())


def test_mission_start_scheduled_later_records_sleeping(
    tmp_path: Path, _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    started = _fake_temporal(monkeypatch)
    result = runner.invoke(
        cli.app,
        ["mission-start", "--task", "t", "--workdir", str(tmp_path), "--start-in-seconds", "60"],
    )
    assert result.exit_code == 0, result.output
    mission_id = started[0].removeprefix("mission:")
    assert _row_status(_isolated_mission_store, mission_id) == "SLEEPING"


def test_mission_start_that_cannot_start_the_workflow_records_aborted(
    tmp_path: Path, _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    started = _fake_temporal(monkeypatch, fail=True)
    result = runner.invoke(cli.app, ["mission-start", "--task", "t", "--workdir", str(tmp_path)])
    assert result.exit_code != 0
    mission_id = started[0].removeprefix("mission:")
    assert _row_status(_isolated_mission_store, mission_id) == "ABORTED"


def test_record_mission_status_upserts_and_keeps_the_title(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    from lha.durable.types import MissionStatusInput

    async def _go() -> None:
        store = await _store(_isolated_mission_store)
        await store.upsert_mission(mission_id="m9", title="Kept", status="RUNNING", head_sha="a1")
        await store.close()
        inp = MissionStatusInput(
            mission_id="m9", workdir=str(tmp_path), status="SLEEPING", reason="pause"
        )
        assert await acts._record_mission_status(inp) is True
        assert await acts._record_mission_status(inp) is True  # idempotent
        store = await _store(_isolated_mission_store)
        row = await store.get_mission("m9")
        await store.close()
        assert row is not None and (row.status, row.title, row.head_sha) == (
            "SLEEPING",
            "Kept",
            "a1",
        )

    asyncio.run(_go())


def test_record_mission_status_on_an_unusable_store_is_a_config_error(tmp_path: Path) -> None:
    from lha.durable.types import MissionStatusInput

    settings = _settings(
        postgres_dsn="postgresql://u:p@127.0.0.1:1/none?connect_timeout=1",
        postgres_fallback_to_sqlite=False,
    )
    inp = MissionStatusInput(mission_id="m9", workdir=str(tmp_path), status="DONE")
    with pytest.raises(ApplicationError) as info:
        asyncio.run(acts._record_mission_status(inp, settings=settings))
    assert info.value.non_retryable and "mission store" in info.value.message


def test_cli_config_shows_the_resolved_store(
    _isolated_mission_store: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    get_settings.cache_clear()
    result = runner.invoke(cli.app, ["config"])
    assert result.exit_code == 0, result.output
    assert f"mission store = sqlite {_isolated_mission_store.resolve()}" in result.output
