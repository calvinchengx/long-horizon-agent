"""A long local mission holds one memory plane, trace recorder, cost ledger and blackboard for
weeks: none of them may grow with the number of cycles."""

from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from lha.contracts.model import Usage
from lha.contracts.state import ChecklistItem, SituationSnapshot
from lha.coordination.blackboard import Blackboard
from lha.governor import cost
from lha.governor.cost import CostEntry, CostLedger
from lha.memory.embeddings import HashEmbedder
from lha.memory.service import MemoryConfig, MissionMemory
from lha.obs.events import TraceRecorder
from lha.ops.degradation import decide_memory_mode
from lha.persistence.sqlite import SqliteStore


def _git(cwd: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


@pytest.mark.asyncio
async def test_the_embedding_cache_keeps_only_what_the_latest_recall_used(tmp_path: Path) -> None:
    ws = tmp_path / "ws"
    ws.mkdir()
    files = {f"mod{i}.py": [f"def f{i}_{j}(): return {j}\n" for j in range(120)] for i in range(5)}
    for name, lines in files.items():
        (ws / name).write_text("".join(lines))
    _git(ws, "init", "-q")
    _git(ws, "add", "-A")
    _git(ws, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
    store = SqliteStore(tmp_path / "mem.sqlite3")
    await store.open()
    memory = MissionMemory(
        store=store,
        workdir=ws,
        config=MemoryConfig(),
        mode=decide_memory_mode([]),
        embedder=HashEmbedder(),
    )
    item = ChecklistItem(id="01", description="edit the modules")
    sizes = []
    for cycle in range(6):
        # Insert a line at the top of one file: every one of its chunks changes.
        name = f"mod{cycle % 5}.py"
        files[name].insert(0, f"# edit {cycle}\n")
        (ws / name).write_text("".join(files[name]))
        _git(ws, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", f"e{cycle}")
        await memory.recall(
            mission_id="m1",
            cycle_id=f"c{cycle}",
            item=item,
            snapshot=SituationSnapshot(head_sha=""),
        )
        assert set(memory._vectors) == memory._touched  # nothing the latest recall did not use
        sizes.append(len(memory._vectors))
    await store.close()
    # Each file grows from 3 chunks to 4 once; after that, edits replace chunks, not add them.
    assert sizes[4] == sizes[5], sizes


def test_the_trace_recorder_keeps_the_newest_events() -> None:
    recorder = TraceRecorder(max_events=10)
    for n in range(100):
        recorder.record("tick", mission_id="m1", n=n)
    assert 10 <= len(recorder.events) <= 11
    assert recorder.events[-1].data["n"] == 99
    assert recorder.dropped + len(recorder.events) == 100


def test_the_cost_ledger_keeps_totals_over_every_entry(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(cost, "MAX_LEDGER_ENTRIES", 10)
    ledger = CostLedger()
    for n in range(100):
        ledger.record(
            cycle_id=f"c{n // 4}",
            usage=Usage(model="m", input_tokens=10, output_tokens=1),
            usd=None if n % 10 == 0 else 0.5,
        )
    assert len(ledger.entries) <= 11 and ledger.entries[-1].cycle_id == "c24"
    assert ledger.total_usd == pytest.approx(45.0)  # 90 priced calls
    assert ledger.unknown_cost_entries == 10
    assert (ledger.total_input_tokens, ledger.total_output_tokens) == (1000, 100)
    assert ledger.mean_usd_per_cycle() == pytest.approx(45.0 / 25)
    assert ledger.max_usd_per_cycle() == pytest.approx(2.0)
    seeded = CostLedger(
        entries=[CostEntry(cycle_id="p", model="m", input_tokens=1, output_tokens=2, usd=3.0)]
    )
    assert (seeded.total_usd, seeded.total_input_tokens, len(seeded.entries)) == (3.0, 1, 1)


def test_the_blackboard_keeps_the_newest_posts_and_counts_them_all() -> None:
    board = Blackboard(max_entries=5)
    for n in range(8):
        board.post("a", f"p{n}")
    board.respond("b", "r")
    board.commit_round()
    assert [e.content for e in board.read()] == ["p4", "p5", "p6", "p7", "r"]
    assert board.posted == 9
