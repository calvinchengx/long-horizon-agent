"""``lha objects prune``: the ClaimCheck store grows with every durable cycle and nothing else
removes an object, so an operator can delete objects older than a retention they choose."""

from __future__ import annotations

import os
import time
from pathlib import Path

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.persistence.object_store import PruneResult, prune_objects

_DAY = 86_400


def _aged(path: Path, days: int, size: int = 5) -> None:
    path.write_bytes(b"x" * size)
    past = time.time() - days * _DAY
    os.utime(path, (past, past))


def test_prune_deletes_only_old_objects(tmp_path: Path) -> None:
    old, fresh, stray = tmp_path / ("a" * 64), tmp_path / ("b" * 64), tmp_path / "notes.txt"
    _aged(old, 40)
    fresh.write_bytes(b"x" * 5)
    _aged(stray, 40)
    dry = prune_objects(tmp_path, older_than_days=30, dry_run=True)
    assert dry == PruneResult(count=1, bytes=5, kept=1) and old.exists()
    assert prune_objects(tmp_path, older_than_days=30) == dry
    assert not old.exists() and fresh.exists() and stray.exists()
    assert prune_objects(tmp_path / "missing", older_than_days=30) == PruneResult(0, 0, 0)
    with pytest.raises(ValueError, match=">= 1"):
        prune_objects(tmp_path, older_than_days=0)


def test_cli_objects_prune(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    root = tmp_path / "objects"
    root.mkdir()
    _aged(root / ("c" * 64), 10, size=2 * 2**20)
    (root / ("d" * 64)).write_bytes(b"x")
    monkeypatch.setenv("LHA_OBJECT_STORE_ROOT", str(root))
    cli.get_settings.cache_clear()
    runner = CliRunner()
    dry = runner.invoke(cli.app, ["objects", "prune", "--older-than-days", "7", "--dry-run"])
    assert dry.exit_code == 0 and dry.output == "1 objects to delete (2.0 MiB), 1 kept\n"
    done = runner.invoke(cli.app, ["objects", "prune", "--older-than-days", "7"])
    assert (
        done.output == "1 objects deleted (2.0 MiB), 1 kept\n" and not (root / ("c" * 64)).exists()
    )
    assert runner.invoke(cli.app, ["objects", "prune"]).exit_code == 2
    assert runner.invoke(cli.app, ["objects", "prune", "--older-than-days", "0"]).exit_code == 2
    cli.get_settings.cache_clear()
