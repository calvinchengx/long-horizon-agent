"""Suite-wide fixtures.

Run paths persist missions / spend / memory to a SQLite store. So tests never write into the
developer's per-user store (``persistence.store.default_sqlite_path``, used when
``LHA_SQLITE_PATH`` is unset): ``LHA_SQLITE_PATH`` points at a session temp dir from the moment
this file is imported (before test modules build module-level ``Settings``), and every test then
gets its own store file on top of that.
"""

from __future__ import annotations

import os
import shutil
import signal
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
    # Never export the suite's spans to a collector configured in the developer's shell.
    for name in (
        "LHA_OTEL_EXPORTER_OTLP_ENDPOINT",
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "LHA_LANGFUSE_HOST",
        "LHA_LANGFUSE_PUBLIC_KEY",
        "LHA_LANGFUSE_SECRET_KEY",
    ):
        mp.delenv(name, raising=False)
    get_settings.cache_clear()
    try:
        yield path
    finally:
        mp.undo()
        get_settings.cache_clear()


# Modules that exercise the shell-command classifier. Every input must be classified quickly: a
# parser loop that never ends would hang the dispatcher, so these tests fail after a deadline
# (this also turns a mutation that makes such a loop infinite into a failure, not a timeout).
_CLASSIFIER_TEST_MODULES = frozenset(
    {
        "test_safety_commands",
        "test_safety_bypasses",
        "test_safety_command_paths",
        "test_review_fixes_safety",
        "test_spec_conformance",
    }
)
_CLASSIFIER_DEADLINE_S = 5


def _classifier_deadline_hit(signum: int, frame: object) -> None:
    raise TimeoutError(f"no result within {_CLASSIFIER_DEADLINE_S}s (a parser loop never ends?)")


@pytest.fixture(autouse=True)
def _classifier_deadline(request: pytest.FixtureRequest) -> Iterator[None]:
    module = request.module.__name__.rpartition(".")[2]
    if module not in _CLASSIFIER_TEST_MODULES or not hasattr(signal, "SIGALRM"):
        yield
        return
    previous = signal.signal(signal.SIGALRM, _classifier_deadline_hit)
    signal.alarm(_CLASSIFIER_DEADLINE_S)
    try:
        yield
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, previous)


# --- contract coverage (docs/27-mission-ui.md#contract-coverage) -------------------------------
def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--contract-coverage",
        action="store_true",
        help="fail unless every spec/ file and top-level case group was checked",
    )


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    if not session.config.getoption("--contract-coverage"):
        return
    import sys

    module = sys.modules.get("tests.unit.test_spec_conformance") or sys.modules.get(
        "test_spec_conformance"
    )
    gaps = ["test_spec_conformance did not run"] if module is None else module.uncovered()
    if gaps:
        print("\nspec coverage gaps:\n  " + "\n  ".join(gaps))
        session.exitstatus = 1
