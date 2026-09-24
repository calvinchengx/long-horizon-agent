"""OpenTelemetry export: configuration at process start, spans on every run path, never blocking."""

from __future__ import annotations

import base64
import sys
import time
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import ReadableSpan
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode
from opentelemetry.util._once import Once
from typer.testing import CliRunner

from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.durable import activities
from lha.durable.types import CycleInput, CycleResult
from lha.model.stub import StubModel
from lha.obs import otel


def _reset_global_provider() -> None:
    # The OTel API lets a process set its tracer provider once; tests need a fresh one each.
    trace._TRACER_PROVIDER_SET_ONCE = Once()  # type: ignore[attr-defined]
    trace._TRACER_PROVIDER = None  # type: ignore[attr-defined]
    otel._provider = None


@pytest.fixture(autouse=True)
def _fresh_tracing() -> Iterator[None]:
    _reset_global_provider()
    yield
    otel.shutdown_tracing()
    _reset_global_provider()


@pytest.fixture
def exported() -> Iterator[InMemorySpanExporter]:
    """Tracing configured to an in-memory exporter; read spans after ``_finished``."""
    exporter = InMemorySpanExporter()
    names = otel.configure_tracing(
        Settings(otel_exporter_otlp_endpoint="http://collector:4318"),
        exporter_factory=lambda target, timeout_s: exporter,
    )
    assert names == ["otlp"]
    yield exporter


def _finished(exporter: InMemorySpanExporter) -> list[ReadableSpan]:
    assert otel._provider is not None
    otel._provider.force_flush()
    return list(exporter.get_finished_spans())


def _by_name(spans: list[ReadableSpan], prefix: str) -> list[ReadableSpan]:
    return [s for s in spans if s.name.startswith(prefix)]


# --- configuration --------------------------------------------------------------------------
def test_no_target_means_no_provider() -> None:
    assert otel.export_targets(Settings()) == []
    assert otel.configure_tracing(Settings()) == []
    assert otel._provider is None


def test_otlp_endpoint_gets_the_traces_path_once() -> None:
    [target] = otel.export_targets(Settings(otel_exporter_otlp_endpoint="http://c:4318/"))
    assert (target.name, target.endpoint, target.headers) == (
        "otlp",
        "http://c:4318/v1/traces",
        None,  # the exporter reads OTEL_EXPORTER_OTLP_HEADERS itself
    )
    [same] = otel.export_targets(Settings(otel_exporter_otlp_endpoint="http://c/v1/traces"))
    assert same.endpoint == "http://c/v1/traces"


def test_standard_otel_endpoint_is_read(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://std:4318")
    assert Settings().otel_exporter_otlp_endpoint == "http://std:4318"
    monkeypatch.setenv("LHA_OTEL_EXPORTER_OTLP_ENDPOINT", "http://lha:4318")
    assert Settings().otel_exporter_otlp_endpoint == "http://lha:4318"


def test_langfuse_goes_through_its_otlp_endpoint_with_basic_auth() -> None:
    settings = Settings(
        langfuse_host="https://lf.example/",
        langfuse_public_key="pk-lf-1",
        langfuse_secret_key="sk-lf-2",  # type: ignore[arg-type]
    )
    [target] = otel.export_targets(settings)
    assert target.name == "langfuse"
    assert target.endpoint == "https://lf.example/api/public/otel/v1/traces"
    assert target.headers == {
        "Authorization": "Basic " + base64.b64encode(b"pk-lf-1:sk-lf-2").decode()
    }
    # Incomplete Langfuse settings configure nothing.
    assert otel.export_targets(Settings(langfuse_host="https://lf.example")) == []


def test_sdk_disabled_turns_tracing_off() -> None:
    settings = Settings(otel_exporter_otlp_endpoint="http://c", otel_sdk_disabled=True)
    assert otel.configure_tracing(settings) == []


def test_missing_extra_is_a_warning_not_an_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "opentelemetry.sdk.trace", None)
    assert otel.configure_tracing(Settings(otel_exporter_otlp_endpoint="http://c")) == []


def test_broken_exporter_setup_does_not_stop_the_process() -> None:
    def boom(target: otel.ExportTarget, timeout_s: float) -> Any:
        raise RuntimeError("bad exporter")

    settings = Settings(otel_exporter_otlp_endpoint="http://c")
    assert otel.configure_tracing(settings, exporter_factory=boom) == []


def test_configure_is_idempotent(exported: InMemorySpanExporter) -> None:
    first = otel._provider
    assert otel.configure_tracing(Settings(otel_exporter_otlp_endpoint="http://c")) == ["otlp"]
    assert otel._provider is first


def test_real_exporter_to_a_dead_backend_never_blocks() -> None:
    settings = Settings(otel_exporter_otlp_endpoint="http://127.0.0.1:9", otel_export_timeout_s=1)
    assert otel.configure_tracing(settings) == ["otlp"]
    started = time.monotonic()
    for i in range(20):
        with otel.span("work", {"i": i}):
            pass
    assert time.monotonic() - started < 1.0  # export happens off the agent's path
    started = time.monotonic()
    otel.shutdown_tracing()
    assert time.monotonic() - started < 5.0  # bounded by the export timeout


def test_cli_configures_tracing_at_start(monkeypatch: pytest.MonkeyPatch) -> None:
    from lha.cli import main as cli

    seen: list[str] = []
    monkeypatch.setattr(
        otel, "configure_tracing", lambda *a, component="cli", **k: seen.append(component) or []
    )

    async def no_worker() -> None:
        return None

    monkeypatch.setattr("lha.durable.worker.run_worker", no_worker)
    runner = CliRunner()
    assert runner.invoke(cli.app, ["version"]).exit_code == 0
    assert runner.invoke(cli.app, ["worker"]).exit_code == 0
    assert seen == ["cli", "worker"]


# --- spans --------------------------------------------------------------------------------------
def test_spans_are_noops_without_opentelemetry(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setitem(sys.modules, "opentelemetry", None)
    with otel.span("x", {"a": 1}) as handle:
        handle.set({"b": 2})
        handle.error("nothing to mark")
    with otel.agent_span("y", item="1"):
        pass


def test_attributes_are_redacted_and_errors_recorded(exported: InMemorySpanExporter) -> None:
    with pytest.raises(ValueError, match="boom"), otel.span("work", {"api_key": "hunter2"}) as h:
        h.set({"note": "Authorization: Bearer abc.def", "skipped": None, "obj": ["x"]})
        raise ValueError("boom")
    [span] = _finished(exported)
    attrs = dict(span.attributes or {})
    assert attrs["api_key"] == "***"
    assert "abc.def" not in str(attrs["note"])
    assert "skipped" not in attrs and attrs["obj"] == "['x']"
    assert span.status.status_code is StatusCode.ERROR
    assert span.events and span.events[0].name == "exception"


def test_tracing_failures_never_reach_the_traced_work(monkeypatch: pytest.MonkeyPatch) -> None:
    class _BrokenTracer:
        def start_as_current_span(self, name: str) -> Any:
            raise RuntimeError("tracer broke")

    monkeypatch.setattr(otel, "_tracer", lambda: _BrokenTracer())
    ran = []
    with otel.span("x") as handle:
        ran.append(True)
        handle.set({"a": 1})
    assert ran == [True]

    class _BadSpan:
        def set_attribute(self, key: str, value: object) -> None:
            raise RuntimeError("attribute broke")

        def set_status(self, status: object) -> None:
            raise RuntimeError("status broke")

    handle = otel.SpanHandle(_BadSpan())
    handle.set({"a": 1})
    handle.error("x")


def test_local_mission_traces_mission_cycle_model_and_tool(
    tmp_path: Path, exported: InMemorySpanExporter
) -> None:
    import asyncio

    model = StubModel(
        script=[
            TurnResult(
                text="",
                tool_calls=[ToolCall(id="t1", name="write_file", arguments={"path": "a.txt"})],
            ),
            TurnResult(text='{"done": true, "summary": "ok"}'),
        ]
    )
    summary = asyncio.run(
        run_mission_local(
            workdir=str(tmp_path),
            title="t",
            description="d",
            checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
            checks=[Check(name="green", command=[sys.executable, "-c", "pass"])],
            settings=Settings(
                sandbox="local",
                allow_unsafe_local=True,
                model_backend="stub",
                budget_usd_ceiling=100.0,
            ),
            model=model,
        )
    )
    assert summary.completed
    spans = _finished(exported)
    [mission] = _by_name(spans, "lha.mission")
    [cycle] = _by_name(spans, "lha.cycle")
    chats = _by_name(spans, "chat ")
    [tool] = _by_name(spans, "execute_tool write_file")
    assert mission.attributes and mission.attributes["lha.mission_id"] == summary.mission_id
    assert mission.attributes["lha.completed"] is True
    assert cycle.parent is not None and cycle.parent.span_id == mission.context.span_id
    assert cycle.attributes and cycle.attributes["lha.verdict"] == "passed"
    assert len(chats) == 2
    assert all(c.parent is not None and c.parent.span_id == cycle.context.span_id for c in chats)
    assert chats[0].attributes and chats[0].attributes["gen_ai.operation.name"] == "chat"
    assert chats[0].attributes["lha.role"] == "lead"
    assert "gen_ai.usage.output_tokens" in chats[0].attributes
    # write_file without "content" is refused by the dispatcher: still one span, marked failed.
    assert tool.attributes and tool.attributes["lha.tool.ok"] is False
    assert tool.status.status_code is StatusCode.ERROR
    assert tool.parent is not None and tool.parent.span_id == cycle.context.span_id


@pytest.mark.asyncio
async def test_durable_cycle_activity_is_one_span_per_attempt(
    exported: InMemorySpanExporter,
) -> None:
    async def cycle() -> CycleResult:
        return CycleResult(
            item_id="01",
            advanced=True,
            head_sha="abc",
            is_complete=False,
            items_done=0,
            items_total=1,
            verdict="passed",
        )

    inp = CycleInput(mission_id="m1", cycle_id="c3", workdir="/w")
    result = await activities._traced_cycle(inp, cycle())
    assert result.verdict == "passed"
    [span] = _finished(exported)
    assert span.name == "lha.activity.run_agent_cycle"
    assert dict(span.attributes or {}) == {
        "lha.mission_id": "m1",
        "lha.cycle_id": "c3",
        "lha.attempt": 1,
        "lha.verdict": "passed",
        "lha.advanced": True,
    }
