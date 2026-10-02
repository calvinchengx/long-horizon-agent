"""Worker wiring + helpers to run the durable spine against a real Temporal server.

Tests construct their own worker against an in-process time-skipping test server (no Docker, no
API key); this module is the production entrypoint (``python -m lha.durable.worker``) and the
helpers shared by both.

Cross-language guard: the Python and Go Temporal SDKs number timers and activities differently,
so a history recorded by one implementation's worker does not replay on the other's. Workers
therefore mark their identity (``lha-py:<pid>@<host>`` here, ``lha-go:...`` in Go) and, before
polling, refuse to start when the task queue is already polled by the other implementation
(``check_task_queue_pollers``). Temporal lists a poller for a few minutes after it stopped.

Two workers of different implementations started at the same moment can both pass that startup
check. So a running worker re-checks the pollers every ``LHA_WORKER_GUARD_INTERVAL_S`` seconds
(``guard_task_queue``) and, when a poller of the other implementation appears, shuts down
gracefully and exits non-zero with the same message (fail closed: in such a race both stop).

Worker versioning (``LHA_WORKER_DEPLOYMENT`` + ``LHA_WORKER_BUILD_ID``): the worker polls as one
build of a Temporal Worker Deployment. Temporal starts new missions on the deployment's current
version and, with the default ``pinned`` behaviour, keeps each mission (and its Continue-As-New
runs and sub-agent children) on the build that started it, so a deploy never replays an
in-flight mission on changed workflow code; old workers serve their missions until they finish.
``LHA_WORKER_PROMOTE`` makes the new build current once it polls (``promote_build``).
"""

from __future__ import annotations

import asyncio
import logging
import os
import socket
from collections.abc import Awaitable, Callable

from temporalio.api.enums.v1 import TaskQueueType
from temporalio.api.taskqueue.v1 import TaskQueue
from temporalio.api.workflowservice.v1 import (
    DescribeTaskQueueRequest,
    DescribeWorkerDeploymentRequest,
    SetWorkerDeploymentCurrentVersionRequest,
)
from temporalio.client import Client
from temporalio.common import VersioningBehavior, WorkerDeploymentVersion
from temporalio.service import RPCError, RPCStatusCode
from temporalio.worker import Worker, WorkerDeploymentConfig

from lha.config import Settings, get_settings
from lha.durable.activities import (
    check_mission_health,
    declare_impossible,
    notify_gate,
    read_mission_snapshot,
    record_mission_status,
    run_agent_cycle,
    unblock_items,
)
from lha.durable.agent_activities import run_subagent
from lha.durable.data_converter import build_data_converter
from lha.durable.org_activities import (
    integrate_branch,
    plan_round,
    review_cycle,
    run_implementer,
)
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.types import MissionInput, MissionResult
from lha.durable.workflows import MissionWorkflow
from lha.persistence.object_store import PruneResult, prune_objects

_log = logging.getLogger(__name__)


async def connect_client(settings: Settings | None = None) -> Client:
    """Connect to Temporal with the ClaimCheck data converter installed."""
    settings = settings or get_settings()
    return await Client.connect(
        settings.temporal_address,
        namespace=settings.temporal_namespace,
        data_converter=build_data_converter(object_store_root=settings.object_store_root),
    )


#: Marks the workers of each implementation in Temporal's poller list.
IDENTITY_MARKER = "lha-py"
GO_IDENTITY_MARKER = "lha-go"


def worker_identity() -> str:
    """This process's worker identity: ``lha-py:<pid>@<host>`` (the SDK default is
    ``<pid>@<host>``)."""
    return f"{IDENTITY_MARKER}:{os.getpid()}@{socket.gethostname()}"


class MixedWorkersError(RuntimeError):
    """The task queue is already polled by the Go implementation's workers."""


async def check_task_queue_pollers(client: Client, task_queue: str) -> None:
    """Raise ``MixedWorkersError`` when a poller of ``task_queue`` (workflow or activity tasks)
    is a Go lha worker (fail closed: one mission's history must stay with one implementation)."""
    for kind in (
        TaskQueueType.TASK_QUEUE_TYPE_WORKFLOW,
        TaskQueueType.TASK_QUEUE_TYPE_ACTIVITY,
    ):
        resp = await client.workflow_service.describe_task_queue(
            DescribeTaskQueueRequest(
                namespace=client.namespace,
                task_queue=TaskQueue(name=task_queue),
                task_queue_type=kind,
            )
        )
        for identity in sorted(p.identity for p in resp.pollers):
            if GO_IDENTITY_MARKER in identity:
                raise MixedWorkersError(
                    f"task queue {task_queue!r} is already polled by a Go lha worker ({identity}). "
                    "A Go and a Python worker cannot serve the same missions: their Temporal SDKs "
                    "number timers and activities differently, so a history recorded by one does "
                    "not replay on the other. Stop the Go workers (Temporal lists a poller for a "
                    "few minutes after it stops), or start this worker on another queue, e.g. "
                    f"LHA_TASK_QUEUE={task_queue}-py (and start its missions with the same "
                    "LHA_TASK_QUEUE)"
                )


async def guard_task_queue(client: Client, task_queue: str, interval_s: float) -> None:
    """Re-check the pollers of ``task_queue`` every ``interval_s`` seconds, forever; raise
    ``MixedWorkersError`` once a Go lha worker polls it too. A failed check (the server briefly
    unreachable) is logged and retried at the next interval: the worker already passed the
    fail-closed startup check."""
    while True:
        await asyncio.sleep(interval_s)
        try:
            await check_task_queue_pollers(client, task_queue)
        except RPCError as exc:
            _log.warning("cannot re-check who polls task queue %r: %s", task_queue, exc)


async def serve_guarded(
    worker: Worker,
    client: Client,
    task_queue: str,
    interval_s: float,
    on_start: Callable[[], Awaitable[None]] | None = None,
) -> None:
    """Run ``worker`` until cancelled, or until the guard sees a Go poller on its task queue:
    then the worker stops polling and shuts down (``Worker.shutdown``) and ``MixedWorkersError``
    propagates. ``on_start`` runs once the worker polls (an error in it stops the worker)."""
    async with worker:
        if on_start is not None:
            await on_start()
        await guard_task_queue(client, task_queue, interval_s)


# --- worker versioning -----------------------------------------------------------------------
#: How long ``promote_build`` waits for the server to register the new build (its first poll).
PROMOTE_TIMEOUT_S = 60.0

_BEHAVIORS = {
    "pinned": VersioningBehavior.PINNED,
    "auto_upgrade": VersioningBehavior.AUTO_UPGRADE,
}


def deployment_config(settings: Settings) -> WorkerDeploymentConfig | None:
    """The worker's deployment version per settings (``None``: unversioned). Raises
    ``ValueError`` for a half-configured deployment (``Settings.worker_deployment_version``)."""
    version = settings.worker_deployment_version()
    if version is None:
        return None
    return WorkerDeploymentConfig(
        version=WorkerDeploymentVersion(deployment_name=version[0], build_id=version[1]),
        use_worker_versioning=True,
        default_versioning_behavior=_BEHAVIORS[settings.worker_versioning_behavior],
    )


def _set_current_command(deployment: str, build_id: str) -> str:
    return (
        f"temporal worker deployment set-current-version --deployment-name {deployment} "
        f"--build-id {build_id}"
    )


async def current_build_id(client: Client, deployment: str) -> str | None:
    """The build id of ``deployment``'s current version (``None``: no current version yet, or
    no such deployment: new missions then wait for one)."""
    try:
        resp = await client.workflow_service.describe_worker_deployment(
            DescribeWorkerDeploymentRequest(namespace=client.namespace, deployment_name=deployment)
        )
    except RPCError as exc:
        if exc.status == RPCStatusCode.NOT_FOUND:
            return None
        raise
    current = resp.worker_deployment_info.routing_config.current_deployment_version
    return current.build_id or None


async def promote_build(
    client: Client, deployment: str, build_id: str, *, timeout_s: float = PROMOTE_TIMEOUT_S
) -> None:
    """Make ``build_id`` the current version of ``deployment``: new missions start on it, while
    missions pinned to older builds stay there. The server registers a build at its worker's
    first poll, so this retries until it knows the build (or ``timeout_s`` passes)."""
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout_s
    delay = 0.5
    while True:
        try:
            resp = await client.workflow_service.describe_worker_deployment(
                DescribeWorkerDeploymentRequest(
                    namespace=client.namespace, deployment_name=deployment
                )
            )
            if resp.worker_deployment_info.routing_config.current_deployment_version.build_id == (
                build_id
            ):
                return
            await client.workflow_service.set_worker_deployment_current_version(
                SetWorkerDeploymentCurrentVersionRequest(
                    namespace=client.namespace,
                    deployment_name=deployment,
                    build_id=build_id,
                    conflict_token=resp.conflict_token,
                    identity=worker_identity(),
                )
            )
            return
        except RPCError as exc:
            retryable = exc.status in (
                RPCStatusCode.NOT_FOUND,  # the build has not polled yet
                RPCStatusCode.FAILED_PRECONDITION,  # a concurrent change moved the token
                RPCStatusCode.RESOURCE_EXHAUSTED,  # the server rate-limits one deployment's calls
                RPCStatusCode.UNAVAILABLE,
            )
            if not retryable or loop.time() > deadline:
                raise RuntimeError(
                    f"cannot make build {build_id!r} the current version of worker deployment "
                    f"{deployment!r}: {exc}. Set it by hand: "
                    f"{_set_current_command(deployment, build_id)}"
                ) from exc
            await asyncio.sleep(delay)
            delay = min(delay * 2, 5.0)


async def announce_version(client: Client, deployment: str, build_id: str, promote: bool) -> None:
    """Promote this build (``promote``; raises ``RuntimeError`` when that fails), or say which
    build new missions start on: with no current version, they wait until one is set."""
    if promote:
        await promote_build(client, deployment, build_id)
        _log.info("build %r is the current version of worker deployment %r", build_id, deployment)
        return
    try:
        current = await current_build_id(client, deployment)
    except RPCError as exc:  # only informational: the worker serves its pinned missions anyway
        _log.warning("cannot read worker deployment %r: %s", deployment, exc)
        return
    if current is None:
        _log.warning(
            "worker deployment %r has no current version: new missions wait until one is set, "
            "e.g. %s (or LHA_WORKER_PROMOTE=true)",
            deployment,
            _set_current_command(deployment, build_id),
        )
    elif current != build_id:
        _log.info(
            "worker deployment %r: new missions start on build %r; this build (%r) serves the "
            "missions pinned to it",
            deployment,
            current,
            build_id,
        )


def build_worker(
    client: Client, task_queue: str, *, deployment: WorkerDeploymentConfig | None = None
) -> Worker:
    """Construct a Worker that hosts the mission + sub-agent workflows and their activities (as
    one build of a worker deployment when ``deployment`` is set)."""
    return Worker(
        client,
        task_queue=task_queue,
        identity=worker_identity(),
        deployment_config=deployment,
        workflows=[MissionWorkflow, SubAgentWorkflow],
        activities=[
            run_agent_cycle,
            check_mission_health,
            notify_gate,
            declare_impossible,
            unblock_items,
            read_mission_snapshot,
            record_mission_status,
            run_subagent,
            plan_round,
            run_implementer,
            integrate_branch,
            review_cycle,
        ],
    )


async def start_mission(client: Client, inp: MissionInput, task_queue: str) -> MissionResult:
    """Start (or attach to) a mission workflow keyed by mission id and await its result.

    The workflow id IS the mission id, so re-invoking is idempotent (Temporal returns the running
    handle rather than starting a duplicate) — the basis for fleet-scale, one workflow per mission.
    """
    handle = await client.start_workflow(
        MissionWorkflow.run,
        inp,
        id=f"mission:{inp.mission_id}",
        task_queue=task_queue,
    )
    return await handle.result()


def sweep_objects(settings: Settings) -> PruneResult | None:
    """Delete ClaimCheck objects untouched for ``LHA_OBJECT_RETENTION_DAYS`` (``None`` when 0).

    Runs once when a worker starts, so a long-lived deployment's store stops growing without
    an operator's ``lha objects prune``; the result is logged as ``objects_pruned``.
    """
    days = settings.object_retention_days
    if days <= 0:
        return None
    result = prune_objects(settings.object_store_root, older_than_days=days)
    _log.info(
        "objects_pruned root=%s older_than_days=%d deleted=%d bytes=%d kept=%d",
        settings.object_store_root,
        days,
        result.count,
        result.bytes,
        result.kept,
    )
    return result


async def run_worker() -> None:
    """Connect to the configured Temporal server and serve missions until cancelled."""
    settings = get_settings()
    settings.reset_keep_paths()  # a bad LHA_RESET_KEEP fails here, not in every cycle
    await asyncio.to_thread(sweep_objects, settings)
    client = await connect_client(settings)
    deployment = deployment_config(settings)  # a half-set deployment fails here
    await check_task_queue_pollers(client, settings.task_queue)
    worker = build_worker(client, settings.task_queue, deployment=deployment)
    on_start = None
    if deployment is not None:
        version = deployment.version

        async def on_start() -> None:
            await announce_version(
                client, version.deployment_name, version.build_id, settings.worker_promote
            )

    await serve_guarded(
        worker, client, settings.task_queue, settings.worker_guard_interval_s, on_start
    )


if __name__ == "__main__":
    from lha.obs.otel import configure_tracing

    configure_tracing(component="worker")
    asyncio.run(run_worker())
