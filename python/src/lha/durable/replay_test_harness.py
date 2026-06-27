"""Worker-versioning safety net: replay recorded histories against the current workflow code.

Run in CI on every PR. If a code change would break the deterministic replay of a real recorded
history, replay fails here — *before* a deploy can corrupt a weeks-long in-flight mission. This is
the determinism guarantee that makes mid-run deploys safe (paired with pinned Build IDs).

The replayer uses the SAME data converter as the worker (ClaimCheck codec over the configured
object store), so histories containing offloaded payloads decode exactly as they did live.
"""

from __future__ import annotations

from pathlib import Path

from temporalio.client import WorkflowHistory
from temporalio.worker import Replayer

from lha.durable.data_converter import build_data_converter
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.workflows import MissionWorkflow


def build_replayer(*, object_store_root: str | Path | None = None) -> Replayer:
    """A Replayer for every durable workflow, with the worker's data converter."""
    if object_store_root is None:
        from lha.config import get_settings

        object_store_root = get_settings().object_store_root
    return Replayer(
        workflows=[MissionWorkflow, SubAgentWorkflow],
        data_converter=build_data_converter(object_store_root=object_store_root),
    )


async def replay_histories(history_dir: str, *, object_store_root: str | Path | None = None) -> int:
    """Replay every ``*.json`` history in ``history_dir``; raise on the first replay failure.

    Returns the number of histories successfully replayed.
    """
    replayer = build_replayer(object_store_root=object_store_root)
    count = 0
    for path in sorted(Path(history_dir).glob("*.json")):
        history = WorkflowHistory.from_json(path.stem, path.read_text(encoding="utf-8"))
        await replayer.replay_workflow(history)
        count += 1
    return count
