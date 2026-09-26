"""The cross-language worker guard: a Python worker refuses a task queue a Go worker polls.

The Python and Go Temporal SDKs number timers and activities differently, so one mission's
history must stay with one implementation (docs/08, docs/19).
"""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any

import pytest
from temporalio.api.enums.v1 import TaskQueueType
from temporalio.api.taskqueue.v1 import PollerInfo
from temporalio.api.workflowservice.v1 import DescribeTaskQueueResponse

from lha.durable.worker import (
    MixedWorkersError,
    check_task_queue_pollers,
    worker_identity,
)


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
