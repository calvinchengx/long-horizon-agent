"""A long local mission must not grow memory with every cycle (docs/15, "Memory and disk").

Forty cycles editing a small checkout, with the bounded structures' caps set low so they saturate
early; from cycle 20 on, the median per-cycle growth of traced memory must stay within a small
allowance, and the memory plane's embedding cache may grow only by the recalled skills. Before the
bounds existed, each edit added its file's re-chunked vectors per cycle for good.
"""

from __future__ import annotations

import gc
import statistics
import subprocess
import sys
import tracemalloc
from pathlib import Path

import pytest

from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import ModelMessage, ToolCall, TurnResult, Usage
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.governor import cost
from lha.memory.service import MissionMemory
from lha.model.stub import StubModel
from lha.obs import events as events_mod

CYCLES, FILES, LINES = 40, 20, 240  # 6 chunks per file: an edit re-chunks all six
WARM = 20  # cycles before growth is measured: caches fill, every file is edited once
# What legitimately grows per verified item: one recalled skill's vector (at most 200 skills are
# recalled, 8 KB each with the hash embedder) and a ledger entry until the cap. Without the
# bounds, each edit kept its file's six re-chunked vectors (48 KB) per cycle for good.
ALLOWANCE_KB_PER_CYCLE = 20


class _Editor(StubModel):
    """Inserts a line at the top of one file per cycle (shifting its chunks), then signals done."""

    def __init__(self, files: dict[str, list[str]]) -> None:
        super().__init__(script=[])
        self.files = files
        self.n = 0

    async def complete(self, messages: list[ModelMessage], **kw: object) -> TurnResult:
        usage = Usage(model="stub", input_tokens=1000, output_tokens=100)
        if messages and messages[-1].role == "tool":
            return TurnResult(text='{"done": true, "summary": "ok"}', usage=usage)
        self.n += 1
        name = f"src/mod{self.n % FILES:03d}.py"
        self.files[name].insert(0, f"# edit {self.n}\n")
        call = ToolCall(
            id=f"w{self.n}",
            name="write_file",
            arguments={"path": name, "content": "".join(self.files[name])},
        )
        return TurnResult(tool_calls=[call], stop_reason="tool_use", usage=usage)


@pytest.mark.asyncio
async def test_a_long_local_mission_does_not_grow_with_every_cycle(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(cost, "MAX_LEDGER_ENTRIES", 40)
    monkeypatch.setattr(events_mod, "MAX_TRACE_EVENTS", 60)
    workdir = tmp_path / "ws"
    files = {
        f"src/mod{i:03d}.py": [f"def f{i}_{j}(x):\n    return x + {j}\n" for j in range(LINES // 2)]
        for i in range(FILES)
    }
    for name, lines in files.items():
        (workdir / name).parent.mkdir(parents=True, exist_ok=True)
        (workdir / name).write_text("".join(lines))
    for args in (
        ["init", "-q"],
        ["add", "-A"],
        ["-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
    ):
        subprocess.run(["git", *args], cwd=workdir, check=True, capture_output=True)

    samples: dict[int, tuple[float, int]] = {}
    memory: list[MissionMemory] = []
    orig_recall = MissionMemory.recall

    async def recall(self: MissionMemory, **kw: object) -> str:
        if not memory:
            memory.append(self)
        return await orig_recall(self, **kw)

    monkeypatch.setattr(MissionMemory, "recall", recall)
    orig_record = events_mod.TraceRecorder.record
    cycle = [0]

    def record(self: events_mod.TraceRecorder, kind: str, **kw: object) -> events_mod.TraceEvent:
        event = orig_record(self, kind, **kw)
        if kind == "checkpoint":
            cycle[0] += 1
            if cycle[0] >= WARM:
                gc.collect()
                samples[cycle[0]] = (tracemalloc.get_traced_memory()[0], len(memory[0]._vectors))
        return event

    monkeypatch.setattr(events_mod.TraceRecorder, "record", record)
    settings = Settings(
        _env_file=None,  # type: ignore[call-arg]
        sandbox="local",
        allow_unsafe_local=True,
        model_backend="stub",
        budget_usd_ceiling=1e9,
        max_cycles=CYCLES + 5,
        sqlite_path=str(tmp_path / "store.sqlite3"),
        flaky_retries=0,
    )
    tracemalloc.start()
    try:
        summary = await run_mission_local(
            workdir=str(workdir),
            title="growth",
            description="memory must stay bounded",
            checklist=Checklist(
                items=[ChecklistItem(id=f"{i:03d}", description=f"edit {i}") for i in range(CYCLES)]
            ),
            checks=[Check(name="ok", command=[sys.executable, "-c", "pass"])],
            settings=settings,
            model=_Editor(files),
        )
    finally:
        tracemalloc.stop()
    assert summary.completed and summary.cycles == CYCLES
    (_, warm_vectors), (_, end_vectors) = samples[WARM], samples[CYCLES]
    # The median of the per-cycle growth: memory that grows with every cycle moves it, while a
    # one-off step (Python resizing an internal table, such as the interned-string dict pathlib
    # fills) is a single delta and does not.
    deltas = sorted((samples[c + 1][0] - samples[c][0]) / 1024 for c in range(WARM, CYCLES))
    per_cycle_kb = statistics.median(deltas)
    assert per_cycle_kb < ALLOWANCE_KB_PER_CYCLE, f"{per_cycle_kb:.1f} KB per cycle ({deltas})"
    # One recalled skill per verified item, plus this cycle's query and progress note.
    assert end_vectors - warm_vectors <= CYCLES - WARM + 2, (warm_vectors, end_vectors)
    assert len(memory[0]._vectors) == len(memory[0]._touched)
