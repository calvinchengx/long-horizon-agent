"""A model that stays unreachable after its retries and fallbacks ends a LOCAL run cleanly.

``lha run-local`` / ``lha mission``: the run stops with ``model unavailable: <error> after <n>
attempts``, the mission row is ABORTED, the anchor keeps the last committed cycle, and the CLI
prints its summary and exits 1 (no traceback). ``LHA_MODEL_TIMEOUT_S`` sets the client timeout.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import httpx
import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.model import build_provider
from lha.model.failover import FailoverModel
from lha.model.openai_compat import OpenAICompatModel
from lha.model.retry import attempts_of, model_unavailable_reason, with_retries
from lha.model.stub import StubModel
from lha.persistence.sqlite import SqliteStore

_PASS = Check(name="green", command=[sys.executable, "-c", "pass"])
_DONE = TurnResult(text='{"done": true, "summary": "ok"}')
runner = CliRunner()


async def _no_sleep(_seconds: float) -> None:
    return None


def _timing_out(calls: list[int] | None = None) -> httpx.AsyncClient:
    """An HTTP client whose every request times out (Ollama under load)."""

    def handler(request: httpx.Request) -> httpx.Response:
        if calls is not None:
            calls.append(1)
        raise httpx.ReadTimeout("timed out", request=request)

    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


def _ollama(client: httpx.AsyncClient, *, max_retries: int = 3) -> OpenAICompatModel:
    return OpenAICompatModel(
        base_url="http://ollama.test/v1",
        model_name="qwen3:8b",
        label="ollama",
        price_in_per_mtok=0.0,
        price_out_per_mtok=0.0,
        client=client,
        max_retries=max_retries,
        sleep=_no_sleep,
    )


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {
        "sandbox": "local",
        "allow_unsafe_local": True,
        "model_backend": "stub",
        "budget_usd_ceiling": 100.0,
        "max_cycles": 10,
        "max_turns_per_cycle": 2,
        "stall_limit": 50,
    }
    base.update(overrides)
    return Settings(**base)  # type: ignore[arg-type]


def _items(n: int = 1) -> Checklist:
    return Checklist(
        items=[ChecklistItem(id=f"{i:02d}", description=f"thing {i}") for i in range(1, n + 1)]
    )


def _head(workdir: Path) -> str:
    return subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=workdir, capture_output=True, text=True, check=True
    ).stdout.strip()


class _DiesAfter(ModelProvider):
    """Serves ``ok`` turns, then every call times out."""

    def __init__(self, ok: int) -> None:
        self._inner = StubModel(script=[_DONE])
        self._ok = ok
        self.name = self._inner.name

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        if self._ok <= 0:
            raise httpx.ReadTimeout("timed out")
        self._ok -= 1
        return await self._inner.complete(messages, tools=tools, max_tokens=max_tokens)

    def estimate_cost_usd(self, usage: Usage) -> float:
        return 0.0


# --- attempt counting ---------------------------------------------------------------------------
async def test_retries_record_how_many_calls_timed_out() -> None:
    calls: list[int] = []

    async def call() -> None:
        calls.append(1)
        raise httpx.ReadTimeout("timed out")

    with pytest.raises(httpx.ReadTimeout) as info:
        await with_retries(call, max_retries=3, sleep=_no_sleep)
    assert len(calls) == 4 and attempts_of(info.value) == 4
    assert model_unavailable_reason(info.value) == "model unavailable: ReadTimeout after 4 attempts"


def test_only_transient_errors_are_model_unavailable() -> None:
    request = httpx.Request("POST", "http://x/v1/chat/completions")
    unauthorized = httpx.HTTPStatusError(
        "401", request=request, response=httpx.Response(401, request=request)
    )
    assert model_unavailable_reason(unauthorized) is None
    assert model_unavailable_reason(ValueError("bad")) is None
    one = model_unavailable_reason(httpx.ConnectError("refused"))
    assert one == "model unavailable: ConnectError after 1 attempt"


async def test_failover_counts_every_call_of_the_chain() -> None:
    calls: list[int] = []
    chain = FailoverModel(
        [_ollama(_timing_out(calls), max_retries=1), _ollama(_timing_out(calls), max_retries=1)],
        max_rounds=2,
        sleep=_no_sleep,
    )
    with pytest.raises(httpx.ReadTimeout) as info:
        await chain.complete([ModelMessage(role="user", content="hi")])
    assert len(calls) == 8 and attempts_of(info.value) == 8  # 2 rounds x 2 members x 2 calls


# --- the local runner ---------------------------------------------------------------------------
async def test_local_run_stops_cleanly_when_the_model_keeps_timing_out(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    workdir = tmp_path / "ws"
    summary = await run_mission_local(
        workdir=str(workdir),
        title="Timeouts",
        description="d",
        checklist=_items(2),
        checks=[_PASS],
        settings=_settings(),
        model=_ollama(_timing_out()),
    )
    assert summary.stopped_reason == "model unavailable: ReadTimeout after 4 attempts"
    assert not summary.completed and summary.cycles == 0 and summary.items_done == 0
    assert '"kind":"model_unavailable"' in summary.trace_jsonl
    store = SqliteStore(_isolated_mission_store)
    await store.open()
    row = await store.get_mission(summary.mission_id)
    assert row is not None and row.status == "ABORTED"
    status = subprocess.run(
        ["git", "status", "--porcelain", "--", ".lha"], cwd=workdir, capture_output=True, text=True
    )
    assert status.stdout == ""  # nothing half-committed in the anchor


async def test_committed_cycles_survive_a_later_outage(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    workdir = tmp_path / "ws"
    summary = await run_mission_local(
        workdir=str(workdir),
        title="Outage",
        description="d",
        checklist=_items(2),
        checks=[_PASS],
        settings=_settings(),
        model=_DiesAfter(ok=1),
    )
    assert summary.stopped_reason == "model unavailable: ReadTimeout after 1 attempt"
    assert (summary.cycles, summary.items_done, summary.items_total) == (1, 1, 2)
    assert summary.head_sha and summary.head_sha == _head(workdir)


async def test_non_transient_model_errors_still_raise(
    tmp_path: Path, _isolated_mission_store: Path
) -> None:
    class _Broken(_DiesAfter):
        async def complete(self, *args: object, **kwargs: object) -> TurnResult:
            raise ValueError("malformed response")

    with pytest.raises(ValueError, match="malformed response"):
        await run_mission_local(
            workdir=str(tmp_path / "ws"),
            title="Broken",
            description="d",
            checklist=_items(),
            checks=[_PASS],
            settings=_settings(),
            model=_Broken(ok=0),
        )


# --- the CLI ------------------------------------------------------------------------------------
def _use(monkeypatch: pytest.MonkeyPatch, settings: Settings) -> None:
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    monkeypatch.setattr("lha.agent.runner.build_provider", lambda *_a, **_k: _ollama(_timing_out()))


def test_run_local_prints_the_summary_and_exits_1(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, _isolated_mission_store: Path
) -> None:
    _use(monkeypatch, _settings())
    result = runner.invoke(
        cli.app,
        [
            *["run-local", "--title", "t", "--item", "x", "--workdir", str(tmp_path / "ws")],
            *["--sandbox", "local", "--unsafe-local", "--no-default-checks"],
            *["--check", f"{sys.executable} -c pass"],
        ],
    )
    assert result.exit_code == 1, result.output
    assert "model unavailable: ReadTimeout after 4 attempts" in result.output
    assert "items 0/1" in result.output
    assert "Traceback" not in result.output and isinstance(result.exception, SystemExit)


def test_mission_planner_outage_is_a_clean_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, _isolated_mission_store: Path
) -> None:
    _use(monkeypatch, _settings())
    result = runner.invoke(
        cli.app,
        [
            *["mission", "--task", "build it", "--workdir", str(tmp_path / "ws")],
            *["--sandbox", "local", "--unsafe-local", "--no-default-checks"],
            *["--check", f"{sys.executable} -c pass"],
        ],
    )
    assert result.exit_code == 1, result.output
    assert "error: model unavailable: ReadTimeout after 4 attempts" in result.output
    assert isinstance(result.exception, SystemExit)


# --- LHA_MODEL_TIMEOUT_S ------------------------------------------------------------------------
async def test_model_timeout_is_configurable(monkeypatch: pytest.MonkeyPatch) -> None:
    assert Settings().model_timeout_s == 120.0
    monkeypatch.setenv("LHA_MODEL_TIMEOUT_S", "300")
    assert Settings().model_timeout_s == 300.0
    for backend in ("ollama", "openai_compat"):
        provider = build_provider(
            Settings(model_backend=backend, openai_base_url="http://x.test/v1", model_timeout_s=7.5)
        )
        assert isinstance(provider, OpenAICompatModel)
        assert provider._client.timeout.read == 7.5
        await provider.aclose()
    with pytest.raises(ValueError):
        Settings(model_timeout_s=0)
