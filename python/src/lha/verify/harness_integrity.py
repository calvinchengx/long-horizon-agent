"""Test-harness integrity: the agent must not pass the gate by editing the gate.

The deterministic verifier runs the repo's own tests and config. An agent that can write files
could "fix" a red check by weakening a pre-existing test, deleting it, or loosening
``pyproject.toml``/``conftest.py``. The harness snapshots hashes of those files at cycle start;
any modified or deleted file yields a FAILING gating ``harness_integrity`` check (unless the
checklist item explicitly allows harness edits). New test files are always fine.
"""

from __future__ import annotations

import hashlib
import os
import re
from pathlib import Path

from lha.contracts.verify import HARNESS_INTEGRITY_CHECK, CheckResult

# Any file under a directory with one of these names, at ANY depth (``tests/``, ``src/pkg/tests/``).
HARNESS_DIRS: tuple[str, ...] = ("tests", "test")
# Repo-level config that decides what the test run means.
HARNESS_ROOT_FILES: tuple[str, ...] = ("pyproject.toml", "setup.cfg", ".coveragerc")
# Test runner config / fixtures, protected wherever they live.
HARNESS_ANYWHERE_FILES: tuple[str, ...] = ("conftest.py", "tox.ini", "pytest.ini", "noxfile.py")
# pytest's default discovery patterns, protected anywhere (``pkg/test_models.py``).
_TEST_FILE_RE = re.compile(r"^(test_.*|.*_test)\.py$")
_SKIP_DIRS = frozenset(
    {
        ".git",
        ".lha",
        ".venv",
        "venv",
        "node_modules",
        "__pycache__",
        ".mypy_cache",
        ".pytest_cache",
        ".ruff_cache",
        ".tox",
        ".nox",
        "build",
        "dist",
    }
)

HarnessSnapshot = dict[str, str]


def _is_harness_file(rel: str) -> bool:
    parts = rel.split("/")
    return (
        any(part in HARNESS_DIRS for part in parts[:-1])
        or rel in HARNESS_ROOT_FILES
        or parts[-1] in HARNESS_ANYWHERE_FILES
        or bool(_TEST_FILE_RE.match(parts[-1]))
    )


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def snapshot_harness(workdir: str | Path) -> HarnessSnapshot:
    """Map each existing harness file (posix relpath) to its sha256."""
    root = Path(workdir)
    snapshot: HarnessSnapshot = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
        for filename in filenames:
            path = Path(dirpath) / filename
            rel = path.relative_to(root).as_posix()
            if _is_harness_file(rel) and path.is_file() and not path.is_symlink():
                try:
                    snapshot[rel] = _sha256(path)
                except OSError:
                    continue
    return snapshot


def harness_violations(before: HarnessSnapshot, after: HarnessSnapshot) -> list[str]:
    """Pre-existing harness files that were modified or deleted (new files are allowed)."""
    violations: list[str] = []
    for rel, digest in sorted(before.items()):
        now = after.get(rel)
        if now is None:
            violations.append(f"deleted: {rel}")
        elif now != digest:
            violations.append(f"modified: {rel}")
    return violations


def violated_paths(violations: list[str]) -> list[str]:
    """The relpaths named in ``harness_violations`` output."""
    return [v.split(": ", 1)[1] for v in violations if ": " in v]


def integrity_result(violations: list[str]) -> CheckResult:
    """A failing gating ``harness_integrity`` result describing ``violations``."""
    return CheckResult(
        name=HARNESS_INTEGRITY_CHECK,
        passed=False,
        exit_code=1,
        gating=True,
        output_tail=(
            "Pre-existing test-harness files were changed; this is not allowed for this item "
            "(the changes were reverted):\n" + "\n".join(violations)
        ),
    )
