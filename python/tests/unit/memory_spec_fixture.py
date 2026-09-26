"""The memory conformance fixture (``spec/memory/recall.json``), shared by the exporter
(``scripts/export_spec.py``) and the Python conformance test. Go runs the same fixture in
``go/internal/spec/conformance_memory_test.go``.

A case builds a git checkout from ``files``, feeds ``observations`` to a ``MissionMemory`` on a
fresh SQLite store (episodic events, progress notes, skills, consolidation), then renders the
memory block of each ``recalls`` entry.
"""

from __future__ import annotations

import tempfile
from pathlib import Path
from typing import Any

from lha.contracts.state import ChecklistItem, DecisionRecord, SituationSnapshot
from lha.memory.embeddings import HashEmbedder
from lha.memory.service import CycleObservation, MemoryConfig, MissionMemory
from lha.ops.degradation import DependencyStatus, Health, decide_memory_mode
from lha.persistence.sqlite import SqliteStore
from lha.state import git_ops

_FILES = {
    "config_parser.py": (
        "import os\n\n"
        "DEFAULT_PORT = 8080\n\n\n"
        "def parse_config(text):\n"
        '    """Parse key=value lines into a dict."""\n'
        "    out = {}\n"
        "    for line in text.splitlines():\n"
        '        key, _, value = line.partition("=")\n'
        "        out[key.strip()] = value.strip()\n"
        "    return out\n"
    ),
    "server.py": (
        "from config_parser import parse_config\n\n\n"
        "def listen(port=None):\n"
        "    port = port or 8080\n"
        "    return port\n"
    ),
    "README.md": "# demo\n\nA tiny server with a config parser and a listen port.\n",
    "docs/notes.txt": "The listen port comes from the config file (key: port).\n",
}

_OBSERVATIONS: list[dict[str, Any]] = [
    {
        "cycle_id": "c1",
        "item_id": "01",
        "item_description": "fix the config parser for missing port keys",
        "verdict": "failed",
        "verified": False,
        "status": "in_progress",
        "attempts": 1,
        "failure": "- pytest FAILED (exit 1): KeyError 'port' in parse_config",
        "done_summary": "tried a regex",
        "tools": ["read_file", "write_file", "read_file"],
    },
    {
        "cycle_id": "c2",
        "item_id": "01",
        "item_description": "fix the config parser for missing port keys",
        "verdict": "passed",
        "verified": True,
        "status": "done",
        "attempts": 2,
        "failure": "",
        "done_summary": "defaulted the port to DEFAULT_PORT when the key is missing",
        "tools": ["read_file", "write_file", "run_command"],
    },
    {
        "cycle_id": "c3",
        "item_id": "02",
        "item_description": "make the listen port configurable",
        "verdict": "failed",
        "verified": False,
        "status": "in_progress",
        "attempts": 1,
        "failure": "- red FAILED (exit 1): listen() ignores   the\tconfig",
        "done_summary": "",
        "tools": ["grep"],
    },
]

_RECALLS: list[dict[str, Any]] = [
    {
        "cycle_id": "c4",
        "item": {"id": "02", "description": "make the listen port configurable"},
        "decisions": [
            {"decision": "keep ports in config_parser", "rationale": "one source of truth"},
        ],
    },
    {
        "cycle_id": "c4",
        "item": {"id": "03", "description": "add a config parser for missing keys in admin"},
        "decisions": [],
    },
]


def memory_cases() -> list[dict[str, Any]]:
    """The fixture's inputs (``expected`` blocks are filled in by ``run_case``)."""
    base_config = {
        "prompt_budget_chars": 4000,
        "episodic_k": 4,
        "semantic_k": 4,
        "skills_k": 2,
        "consolidate_every": 2,
        "consolidation": "extractive",
        "index_repo_files": True,
        "max_repo_files": 400,
    }
    return [
        {
            "name": "hybrid_hash_sqlite",
            "embedder": "hash",
            "config": base_config,
            "files": _FILES,
            "observations": _OBSERVATIONS,
            "recalls": _RECALLS,
        },
        {
            "name": "lexical_git_grep",
            "embedder": "none",
            "config": {**base_config, "index_repo_files": False, "consolidate_every": 0},
            "files": _FILES,
            "observations": _OBSERVATIONS,
            "recalls": _RECALLS,
        },
        {
            "name": "tight_budget",
            "embedder": "hash",
            "config": {**base_config, "prompt_budget_chars": 700},
            "files": _FILES,
            "observations": _OBSERVATIONS,
            "recalls": _RECALLS[:1],
        },
    ]


async def run_case(case: dict[str, Any]) -> list[str]:
    """The rendered memory block of each of ``case["recalls"]``."""
    with tempfile.TemporaryDirectory(prefix="lha-memspec-") as tmp:
        root = Path(tmp)
        ws = root / "ws"
        ws.mkdir()
        git_ops.init_repo(ws)
        for rel, text in case["files"].items():
            path = ws / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding="utf-8")
        git_ops.commit_all(ws, "init")
        store = SqliteStore(root / "mem.sqlite3")
        await store.open()
        try:
            hybrid = case["embedder"] == "hash"
            statuses = (
                []
                if hybrid
                else [
                    DependencyStatus(
                        "embeddings", Health.DEGRADED, "disabled (LHA_MEMORY_EMBEDDER=none)"
                    )
                ]
            )
            memory = MissionMemory(
                store=store,
                workdir=ws,
                config=MemoryConfig(**case["config"]),
                mode=decide_memory_mode(statuses),
                embedder=HashEmbedder() if hybrid else None,
                namespace="spec",
            )
            for obs in case["observations"]:
                await memory.observe_cycle(
                    CycleObservation(mission_id="m1", head_sha="", before_head="", **obs)
                )
            blocks = []
            for recall in case["recalls"]:
                blocks.append(
                    await memory.recall(
                        mission_id="m1",
                        cycle_id=recall["cycle_id"],
                        item=ChecklistItem(**recall["item"]),
                        snapshot=SituationSnapshot(
                            head_sha="",
                            last_decisions=[DecisionRecord(**d) for d in recall["decisions"]],
                        ),
                    )
                )
            assert memory.errors == 0, "memory errors while running the fixture"
            return blocks
        finally:
            await store.close()
