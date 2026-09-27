"""Shared fixtures for the durability/load tests: scripted models + a real gating check.

The model is a scripted ``StubModel`` (no network) that writes ``work/<item>.txt`` and then says
done. The gating check is a real command run in the sandbox: it passes only if the working tree
holds more ``work/*.txt`` files than the committed checklist has done items — i.e. only once THIS
cycle's work exists. Nothing is marked done without it.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys
import time
import uuid
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from pathlib import Path

from lha.config import Settings
from lha.contracts.model import ModelProvider, ToolCall, TurnResult
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.durable.types import MissionInput
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor

SETTINGS = Settings(
    _env_file=None,  # type: ignore[call-arg]
    model_backend="stub",
    sandbox="local",
    allow_unsafe_local=True,
    max_turns_per_cycle=4,
)

_CHECK_SRC = (
    "import json, pathlib, sys\n"
    "items = json.loads(pathlib.Path('.lha/checklist.json').read_text())['items']\n"
    "done = sum(1 for i in items if i['status'] == 'done')\n"
    "work = len(list(pathlib.Path('work').glob('*.txt')))\n"
    "sys.exit(0 if work > done else 1)\n"
)
CHECK_COMMANDS: list[list[str]] = [[sys.executable, "-c", _CHECK_SRC]]

ModelFactory = Callable[[Settings, SituationSnapshot], ModelProvider]


def _done() -> TurnResult:
    return TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn")


def write_turn(path: str, content: str = "done") -> TurnResult:
    return TurnResult(
        tool_calls=[
            ToolCall(id="w1", name="write_file", arguments={"path": path, "content": content})
        ],
        stop_reason="tool_use",
    )


def working_model(_settings: Settings, snapshot: SituationSnapshot) -> ModelProvider:
    """Writes the active item's work file, then signals done."""
    assert snapshot.active_item is not None
    return StubModel(script=[write_turn(f"work/{snapshot.active_item.id}.txt"), _done()])


def idle_model() -> ModelProvider:
    """Claims done without doing any work (verification must fail)."""
    return StubModel(script=[_done()])


def checklist(n: int, *, chain: bool = False) -> Checklist:
    items = []
    for i in range(1, n + 1):
        deps = [f"{i - 1:02d}"] if chain and i > 1 else []
        items.append(ChecklistItem(id=f"{i:02d}", description=f"task {i}", depends_on=deps))
    return Checklist(items=items)


async def init_mission(
    workdir: Path,
    n: int,
    *,
    chain: bool = False,
    **overrides: object,
) -> MissionInput:
    anchor = GitMissionAnchor(workdir)
    await anchor.initialize(
        title="Test mission", description="durability", items=checklist(n, chain=chain)
    )
    fields: dict[str, object] = {
        "mission_id": uuid.uuid4().hex[:8],
        "workdir": str(workdir),
        "max_cycles": 50,
        "check_commands": CHECK_COMMANDS,
        "park_initial_seconds": 30,
        "park_max_seconds": 600,
    }
    fields.update(overrides)
    return MissionInput(**fields)  # type: ignore[arg-type]


def commits_with(workdir: Path, marker: str) -> int:
    return sum(1 for line in git_ops.log_oneline(workdir, 1000) if marker in line)


def all_committed_paths(workdir: Path) -> set[str]:
    out = git_ops.run_git(workdir, "log", "--all", "--name-only", "--pretty=format:")
    return {line.strip() for line in out.splitlines() if line.strip()}


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


@contextmanager
def real_temporal_server(tmp: Path) -> Iterator[str | None]:
    """The address of a real Temporal server, or None: ``LHA_IT_TEMPORAL_ADDRESS`` (an existing
    server), else a ``temporal server start-dev`` started here (and stopped on exit) when the
    Temporal CLI is on PATH. The time-skipping test server lacks some APIs (DescribeTaskQueue)."""
    if addr := os.environ.get("LHA_IT_TEMPORAL_ADDRESS"):
        yield addr
        return
    temporal = shutil.which("temporal")
    if temporal is None:
        yield None
        return
    port = _free_port()
    proc = subprocess.Popen(
        [
            temporal, "server", "start-dev", "--headless", "--ip", "127.0.0.1",
            "--port", str(port), "--ui-port", str(_free_port()),
            "--http-port", str(_free_port()), "--metrics-port", str(_free_port()),
            "--db-filename", str(tmp / "temporal.db"), "--log-level", "error",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )  # fmt: skip
    try:
        deadline = time.monotonic() + 60
        while True:
            try:
                socket.create_connection(("127.0.0.1", port), timeout=1).close()
                break
            except OSError:
                if proc.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("temporal server start-dev did not start") from None
                time.sleep(0.2)
        yield f"127.0.0.1:{port}"
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
