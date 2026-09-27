"""The cross-language worker guard: a Python worker refuses a task queue a Go worker polls.

The Python and Go Temporal SDKs number timers and activities differently, so one mission's
history must stay with one implementation (docs/08, docs/19).
"""

from __future__ import annotations

import asyncio
import uuid
from collections.abc import Iterator
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from temporalio.api.enums.v1 import TaskQueueType
from temporalio.api.taskqueue.v1 import PollerInfo, TaskQueue
from temporalio.api.workflowservice.v1 import DescribeTaskQueueRequest, DescribeTaskQueueResponse
from temporalio.client import Client
from temporalio.service import RPCError, RPCStatusCode
from temporalio.worker import Worker

from lha.config import get_settings
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.worker import (
    MixedWorkersError,
    check_task_queue_pollers,
    guard_task_queue,
    run_worker,
    serve_guarded,
    worker_identity,
)
from tests.durability._support import real_temporal_server


def _client(pollers: dict[int, list[str]]) -> Any:
    async def describe_task_queue(req: Any) -> DescribeTaskQueueResponse:
        ids = pollers.get(req.task_queue_type, [])
        return DescribeTaskQueueResponse(pollers=[PollerInfo(identity=i) for i in ids])

    service = SimpleNamespace(describe_task_queue=describe_task_queue)
    return SimpleNamespace(namespace="default", workflow_service=service)


def test_identity_is_marked() -> None:
    assert worker_identity().startswith("lha-py:")


@pytest.mark.asyncio
async def test_python_and_legacy_pollers_are_accepted() -> None:
    client = _client({TaskQueueType.TASK_QUEUE_TYPE_WORKFLOW: ["lha-py:1@h", "42@legacy"]})
    await check_task_queue_pollers(client, "lha-mission")


@pytest.mark.asyncio
async def test_a_go_poller_is_refused() -> None:
    client = _client({TaskQueueType.TASK_QUEUE_TYPE_ACTIVITY: ["lha-go:7@h"]})
    with pytest.raises(MixedWorkersError, match="LHA_TASK_QUEUE=lha-mission-py"):
        await check_task_queue_pollers(client, "lha-mission")


def _changing_client(answers: list[list[str] | Exception]) -> tuple[Any, list[int]]:
    """A client whose n-th describe call answers ``answers[n]`` (the last answer repeats);
    ``calls[0]`` counts the describe calls."""
    calls = [0]

    async def describe_task_queue(req: Any) -> DescribeTaskQueueResponse:
        answer = answers[min(calls[0], len(answers) - 1)]
        calls[0] += 1
        if isinstance(answer, Exception):
            raise answer
        return DescribeTaskQueueResponse(pollers=[PollerInfo(identity=i) for i in answer])

    service = SimpleNamespace(describe_task_queue=describe_task_queue)
    return SimpleNamespace(namespace="default", workflow_service=service), calls


@pytest.mark.asyncio
async def test_the_guard_rechecks_until_a_go_poller_appears() -> None:
    unavailable = RPCError("unavailable", RPCStatusCode.UNAVAILABLE, b"")
    py, both = ["lha-py:1@h"], ["lha-py:1@h", "lha-go:9@h"]
    client, calls = _changing_client([py, py, unavailable, both])
    with pytest.raises(MixedWorkersError, match=r"lha-go:9@h"):
        await asyncio.wait_for(guard_task_queue(client, "lha-mission", 0.001), 5)
    # A clean round (2 calls), a failed one that is only logged (1 call), then the Go poller.
    assert calls[0] == 4


class _FakeWorker:
    def __init__(self) -> None:
        self.events: list[str] = []

    async def __aenter__(self) -> _FakeWorker:
        self.events.append("started")
        return self

    async def __aexit__(self, *exc: object) -> None:
        self.events.append("shut down")


@pytest.mark.asyncio
async def test_a_go_poller_that_appears_shuts_the_worker_down() -> None:
    client, _ = _changing_client([[], [], ["lha-go:7@h"]])
    worker = _FakeWorker()
    with pytest.raises(MixedWorkersError, match="LHA_TASK_QUEUE=q-py"):
        await asyncio.wait_for(serve_guarded(worker, client, "q", 0.001), 5)  # type: ignore[arg-type]
    assert worker.events == ["started", "shut down"]


def test_the_worker_command_exits_2_when_the_guard_trips(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from typer.testing import CliRunner

    from lha.cli.main import app

    async def tripped() -> None:
        raise MixedWorkersError("task queue 'q' is already polled by a Go lha worker (lha-go:1@h)")

    monkeypatch.setattr("lha.durable.worker.run_worker", tripped)
    result = CliRunner().invoke(app, ["worker"])
    assert result.exit_code == 2
    assert "already polled by a Go lha worker" in result.output


def test_the_guard_interval_is_configurable(monkeypatch: pytest.MonkeyPatch) -> None:
    from lha.config import Settings

    assert Settings(_env_file=None).worker_guard_interval_s == 30.0  # type: ignore[call-arg]
    monkeypatch.setenv("LHA_WORKER_GUARD_INTERVAL_S", "0.5")
    assert Settings(_env_file=None).worker_guard_interval_s == 0.5  # type: ignore[call-arg]
    monkeypatch.setenv("LHA_WORKER_GUARD_INTERVAL_S", "0")
    with pytest.raises(ValueError, match="greater than 0"):
        Settings(_env_file=None)  # type: ignore[call-arg]


# --- On a real Temporal server ------------------------------------------------------------------
# The time-skipping test server does not implement DescribeTaskQueue, so these need
# LHA_IT_TEMPORAL_ADDRESS or the temporal CLI (`temporal server start-dev`); otherwise they skip.
# A Python worker with a Go identity stands in for the Go worker (the Go suite's
# TestWorkersRefuseMixedTaskQueues runs the real one).


@pytest.fixture(scope="module")
def temporal_address(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
    with real_temporal_server(tmp_path_factory.mktemp("temporal")) as addr:
        if addr is None:
            pytest.skip(
                "no real Temporal server: set LHA_IT_TEMPORAL_ADDRESS or install the temporal CLI"
            )
        yield addr


async def _connect(addr: str) -> Client:
    deadline = asyncio.get_running_loop().time() + 60
    while True:
        try:
            client = await Client.connect(addr)
            await check_task_queue_pollers(client, "lha-probe")
            return client
        except (RuntimeError, RPCError):
            if asyncio.get_running_loop().time() > deadline:
                raise
            await asyncio.sleep(0.3)


async def _wait_poller(client: Client, queue: str, marker: str) -> None:
    for _ in range(200):
        resp = await client.workflow_service.describe_task_queue(
            DescribeTaskQueueRequest(
                namespace=client.namespace,
                task_queue=TaskQueue(name=queue),
                task_queue_type=TaskQueueType.TASK_QUEUE_TYPE_WORKFLOW,
            )
        )
        if any(marker in p.identity for p in resp.pollers):
            return
        await asyncio.sleep(0.1)
    raise AssertionError(f"no {marker} poller on {queue}")


def _fake_go_worker(client: Client, queue: str) -> Worker:
    return Worker(client, task_queue=queue, identity="lha-go:4242@it", workflows=[SubAgentWorkflow])


def _worker_env(monkeypatch: pytest.MonkeyPatch, addr: str, queue: str, tmp: Path) -> None:
    monkeypatch.setenv("LHA_TEMPORAL_ADDRESS", addr)
    monkeypatch.setenv("LHA_TASK_QUEUE", queue)
    monkeypatch.setenv("LHA_OBJECT_STORE_ROOT", str(tmp / "objects"))
    monkeypatch.setenv("LHA_WORKER_GUARD_INTERVAL_S", "0.2")
    get_settings.cache_clear()


@pytest.mark.asyncio
async def test_real_server_a_worker_refuses_a_go_polled_queue(
    temporal_address: str, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    client = await _connect(temporal_address)
    queue = f"lha-it-guard-{uuid.uuid4().hex[:8]}"
    async with _fake_go_worker(client, queue):
        await _wait_poller(client, queue, "lha-go")
        _worker_env(monkeypatch, temporal_address, queue, tmp_path)
        with pytest.raises(MixedWorkersError, match=f"LHA_TASK_QUEUE={queue}-py"):
            await asyncio.wait_for(run_worker(), 30)


@pytest.mark.asyncio
async def test_real_server_a_go_worker_that_appears_later_stops_the_python_worker(
    temporal_address: str, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    client = await _connect(temporal_address)
    queue = f"lha-it-guard-{uuid.uuid4().hex[:8]}"
    _worker_env(monkeypatch, temporal_address, queue, tmp_path)
    running = asyncio.create_task(run_worker())  # passes the startup check: nobody polls yet
    try:
        await _wait_poller(client, queue, "lha-py")
        assert not running.done()
        # A Go worker that raced past its own startup check starts polling the same queue.
        async with _fake_go_worker(client, queue):
            with pytest.raises(MixedWorkersError, match="lha-go:4242@it"):
                await asyncio.wait_for(running, 30)
    finally:
        running.cancel()
