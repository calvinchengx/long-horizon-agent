"""Suite-wide fixtures.

Run paths persist missions / spend / memory to a SQLite store. So tests never write into the
developer's ``.lha/``: ``LHA_SQLITE_PATH`` points at a session temp dir from the moment this file
is imported (before test modules build module-level ``Settings``), and every test then gets its
own store file on top of that.
"""

from __future__ import annotations

import os
import shutil
import tempfile
from collections.abc import Iterator
from pathlib import Path

import pytest

from lha.config import get_settings

_SESSION_STORE_DIR = tempfile.mkdtemp(prefix="lha-test-store-")
os.environ["LHA_SQLITE_PATH"] = str(Path(_SESSION_STORE_DIR) / "lha.sqlite3")


def pytest_unconfigure(config: pytest.Config) -> None:
    shutil.rmtree(_SESSION_STORE_DIR, ignore_errors=True)


@pytest.fixture(autouse=True)
def _isolated_mission_store(tmp_path_factory: pytest.TempPathFactory) -> Iterator[Path]:
    path = tmp_path_factory.mktemp("lha-store") / "lha.sqlite3"
    mp = pytest.MonkeyPatch()
    mp.setenv("LHA_SQLITE_PATH", str(path))
    mp.delenv("LHA_POSTGRES_DSN", raising=False)
    get_settings.cache_clear()
    try:
        yield path
    finally:
        mp.undo()
        get_settings.cache_clear()
