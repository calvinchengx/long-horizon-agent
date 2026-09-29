"""Worker versioning: a mission stays on the worker build that started it (docs/15, docs/08).

With ``LHA_WORKER_DEPLOYMENT`` + ``LHA_WORKER_BUILD_ID`` the worker polls as one build of a
Temporal Worker Deployment; new missions start on the deployment's current version, and a
``pinned`` mission never moves to another build, so a deploy cannot replay it on changed code.
"""

from __future__ import annotations

import asyncio
import logging
import uuid
from collections.abc import Iterator
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from temporalio.api.common.v1 import WorkflowExecution
from temporalio.api.deployment.v1 import (
    RoutingConfig,
    WorkerDeploymentInfo,
)
from temporalio.api.deployment.v1 import (
    WorkerDeploymentVersion as VersionProto,
)
from temporalio.api.workflowservice.v1 import (
    DescribeWorkerDeploymentResponse,
    DescribeWorkflowExecutionRequest,
)
from temporalio.client import Client
from temporalio.common import VersioningBehavior
from temporalio.service import RPCError, RPCStatusCode

from lha.config import Settings, get_settings
from lha.durable.types import MissionInput
from lha.durable.worker import (
    announce_version,
    connect_client,
    current_build_id,
    deployment_config,
    promote_build,
    run_worker,
)
from lha.durable.workflows import MissionWorkflow
from tests.durability._support import real_temporal_server


# --- settings -----------------------------------------------------------------------------------
def test_versioning_is_off_by_default() -> None:
    assert deployment_config(Settings(_env_file=None)) is None  # type: ignore[call-arg]


def test_deployment_config_carries_the_build_and_behaviour() -> None:
    config = deployment_config(
        Settings(_env_file=None, worker_deployment="lha", worker_build_id="b7")  # type: ignore[call-arg]
    )
    assert config is not None and config.use_worker_versioning
    assert (config.version.deployment_name, config.version.build_id) == ("lha", "b7")
    assert config.default_versioning_behavior == VersioningBehavior.PINNED
    upgrading = deployment_config(
        Settings(  # type: ignore[call-arg]
            _env_file=None,
            worker_deployment="lha",
            worker_build_id="b7",
            worker_versioning_behavior="auto_upgrade",
        )
    )
    assert upgrading is not None
    assert upgrading.default_versioning_behavior == VersioningBehavior.AUTO_UPGRADE


@pytest.mark.parametrize(
    ("fields", "message"),
    [
        ({"worker_deployment": "lha"}, "must be set together"),
        ({"worker_build_id": "b1"}, "must be set together"),
        ({"worker_promote": True}, "LHA_WORKER_PROMOTE needs"),
        ({"worker_deployment": "lha.v2", "worker_build_id": "b1"}, "must not contain '.'"),
    ],
)
def test_half_configured_versioning_is_refused(fields: dict[str, Any], message: str) -> None:
    with pytest.raises(ValueError, match=message):
        deployment_config(Settings(_env_file=None, **fields))  # type: ignore[call-arg]


# --- announcing the build -----------------------------------------------------------------------
def _client(current: str | None, *, missing: bool = False) -> Any:
    async def describe_worker_deployment(req: Any) -> DescribeWorkerDeploymentResponse:
        if missing:
            raise RPCError("not found", RPCStatusCode.NOT_FOUND, b"")
        routing = RoutingConfig(current_deployment_version=VersionProto(build_id=current or ""))
        return DescribeWorkerDeploymentResponse(
            worker_deployment_info=WorkerDeploymentInfo(routing_config=routing)
        )

    service = SimpleNamespace(describe_worker_deployment=describe_worker_deployment)
    return SimpleNamespace(namespace="default", workflow_service=service)


@pytest.mark.asyncio
async def test_a_deployment_without_a_current_version_is_warned_about(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.INFO, logger="lha.durable.worker"):
        await announce_version(_client(None, missing=True), "lha", "b2", promote=False)
        await announce_version(_client(None), "lha", "b2", promote=False)
        await announce_version(_client("b1"), "lha", "b2", promote=False)
        await announce_version(_client("b2"), "lha", "b2", promote=False)
    messages = [r.getMessage() for r in caplog.records]
    command = "temporal worker deployment set-current-version --deployment-name lha --build-id b2"
    assert len(messages) == 3
    assert all("no current version" in m and command in m for m in messages[:2])
    assert "new missions start on build 'b1'" in messages[2]


@pytest.mark.asyncio
async def test_other_describe_errors_propagate() -> None:
    async def describe_worker_deployment(req: Any) -> DescribeWorkerDeploymentResponse:
        raise RPCError("denied", RPCStatusCode.PERMISSION_DENIED, b"")

    client = SimpleNamespace(
        namespace="default",
        workflow_service=SimpleNamespace(describe_worker_deployment=describe_worker_deployment),
    )
    with pytest.raises(RPCError):
        await current_build_id(client, "lha")  # type: ignore[arg-type]
    await announce_version(client, "lha", "b1", promote=False)  # type: ignore[arg-type]  # warns
    with pytest.raises(RuntimeError, match="Set it by hand: temporal worker deployment"):
        await promote_build(client, "lha", "b1", timeout_s=0)  # type: ignore[arg-type]


# --- against a real Temporal server ----------------------------------------------------------------
@pytest.fixture(scope="module")
def temporal_address(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
    with real_temporal_server(tmp_path_factory.mktemp("temporal")) as addr:
        if addr is None:
            pytest.skip(
                "no real Temporal server: set LHA_IT_TEMPORAL_ADDRESS or install the temporal CLI"
            )
        yield addr


def _worker_env(
    monkeypatch: pytest.MonkeyPatch, addr: str, queue: str, tmp: Path, deployment: str, build: str
) -> None:
    monkeypatch.setenv("LHA_TEMPORAL_ADDRESS", addr)
    monkeypatch.setenv("LHA_TASK_QUEUE", queue)
    monkeypatch.setenv("LHA_OBJECT_STORE_ROOT", str(tmp / "objects"))
    monkeypatch.setenv("LHA_WORKER_DEPLOYMENT", deployment)
    monkeypatch.setenv("LHA_WORKER_BUILD_ID", build)
    monkeypatch.setenv("LHA_WORKER_PROMOTE", "true")
    get_settings.cache_clear()


async def _eventually(probe: Any, want: object, what: str) -> None:
    got = None
    for _ in range(120):  # gently: the server rate-limits calls to one worker deployment
        try:
            got = await probe()
        except RPCError:
            got = None
        if got == want:
            return
        await asyncio.sleep(0.5)
    raise AssertionError(f"{what}: {got!r}, want {want!r}")


async def _pinned_build(client: Client, workflow_id: str) -> tuple[int, str]:
    resp = await client.workflow_service.describe_workflow_execution(
        DescribeWorkflowExecutionRequest(
            namespace=client.namespace, execution=WorkflowExecution(workflow_id=workflow_id)
        )
    )
    info = resp.workflow_execution_info.versioning_info
    return info.behavior, info.deployment_version.build_id


@pytest.mark.asyncio
async def test_real_server_missions_stay_on_the_build_that_started_them(
    temporal_address: str, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    queue = f"lha-it-versions-{uuid.uuid4().hex[:8]}"
    deployment = f"lha-it-{uuid.uuid4().hex[:8]}"
    workers: list[asyncio.Task[None]] = []
    client: Client | None = None
    started: list[str] = []
    try:
        for build in ("b1", "b2"):
            _worker_env(monkeypatch, temporal_address, queue, tmp_path, deployment, build)
            settings = get_settings()
            client = client or await connect_client(settings)
            workers.append(asyncio.create_task(run_worker()))
            await _eventually(
                lambda c=client: current_build_id(c, deployment), build, "current build"
            )
            # A mission whose workdir has no anchor: its first activity fails and retries, but
            # its first workflow task already pins it to the build that ran it.
            workflow_id = f"mission:{build}-{uuid.uuid4().hex[:6]}"
            inp = MissionInput(mission_id=workflow_id[8:], workdir=str(tmp_path / "missing"))
            await client.start_workflow(MissionWorkflow.run, inp, id=workflow_id, task_queue=queue)
            started.append(workflow_id)
            await _eventually(
                lambda c=client, w=workflow_id: _pinned_build(c, w),
                (VersioningBehavior.PINNED.value, build),
                f"{workflow_id} pinned",
            )
        assert client is not None
        # The first mission did not move when b2 became current.
        assert await _pinned_build(client, started[0]) == (VersioningBehavior.PINNED.value, "b1")
        assert not any(w.done() for w in workers)
    finally:
        for task in workers:
            task.cancel()
        await asyncio.gather(*workers, return_exceptions=True)
        if client is not None:
            for workflow_id in started:
                await client.get_workflow_handle(workflow_id).terminate("test done")


@pytest.mark.asyncio
async def test_promotion_retries_until_the_server_takes_it() -> None:
    answers: list[RPCStatusCode | None] = [
        RPCStatusCode.NOT_FOUND,  # the build has not polled yet
        RPCStatusCode.RESOURCE_EXHAUSTED,
        None,
    ]
    promoted: list[tuple[str, bytes]] = []

    async def describe_worker_deployment(req: Any) -> DescribeWorkerDeploymentResponse:
        routing = RoutingConfig(
            current_deployment_version=VersionProto(build_id=promoted[-1][0] if promoted else "")
        )
        return DescribeWorkerDeploymentResponse(
            conflict_token=b"t", worker_deployment_info=WorkerDeploymentInfo(routing_config=routing)
        )

    async def set_worker_deployment_current_version(req: Any) -> None:
        status = answers.pop(0)
        if status is not None:
            raise RPCError("later", status, b"")
        promoted.append((req.build_id, req.conflict_token))

    client = SimpleNamespace(
        namespace="default",
        workflow_service=SimpleNamespace(
            describe_worker_deployment=describe_worker_deployment,
            set_worker_deployment_current_version=set_worker_deployment_current_version,
        ),
    )
    await promote_build(client, "lha", "b2", timeout_s=30)  # type: ignore[arg-type]
    assert promoted == [("b2", b"t")] and answers == []
    await promote_build(client, "lha", "b2", timeout_s=0)  # type: ignore[arg-type]  # already current
    assert len(promoted) == 1


@pytest.mark.parametrize(
    ("failure", "code"),
    [
        (ValueError("LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID must be set together"), 2),
        (RuntimeError("cannot make build 'b1' the current version of worker deployment"), 1),
    ],
)
def test_the_worker_command_reports_versioning_errors(
    monkeypatch: pytest.MonkeyPatch, failure: Exception, code: int
) -> None:
    from typer.testing import CliRunner

    from lha.cli.main import app

    async def failing() -> None:
        raise failure

    monkeypatch.setattr("lha.durable.worker.run_worker", failing)
    result = CliRunner().invoke(app, ["worker"])
    assert result.exit_code == code and f"error: {failure}" in result.output
