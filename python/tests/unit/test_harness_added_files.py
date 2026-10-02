"""A new harness configuration file is a harness-integrity violation (docs/07-verification.md).

Found by a measurement mission: told not to edit `tests/unit/test_review_screen.py`, the lead
added a `conftest.py` whose `pytest_collectstart` hook injected a test into that module. New test
files stay allowed; new `conftest.py`, `pytest.ini`, `tox.ini`, `noxfile.py`, `pyproject.toml`,
`setup.cfg` and `.coveragerc` are not.
"""

from __future__ import annotations

from pathlib import Path

from lha.verify.harness_integrity import harness_violations, is_harness_config, snapshot_harness
from lha.verify.review_screen import screen_diff


def test_a_new_conftest_is_a_violation_but_a_new_test_file_is_not(tmp_path: Path) -> None:
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "test_a.py").write_text("def test_a(): pass\n", encoding="utf-8")
    before = snapshot_harness(tmp_path)
    (tmp_path / "tests" / "test_b.py").write_text("def test_b(): pass\n", encoding="utf-8")
    assert harness_violations(before, snapshot_harness(tmp_path)) == []
    (tmp_path / "tests" / "conftest.py").write_text(
        "def pytest_collectstart(c): pass\n", encoding="utf-8"
    )
    (tmp_path / "pytest.ini").write_text("[pytest]\n", encoding="utf-8")
    assert harness_violations(before, snapshot_harness(tmp_path)) == [
        "added: pytest.ini",
        "added: tests/conftest.py",
    ]


def test_is_harness_config() -> None:
    assert is_harness_config("conftest.py") and is_harness_config("a/b/conftest.py")
    assert is_harness_config("pyproject.toml") and is_harness_config("noxfile.py")
    assert not is_harness_config("tests/test_a.py") and not is_harness_config(
        "src/conftest_helper.py"
    )
    assert not is_harness_config("pkg/pyproject.toml")  # root files protect the root only


def test_the_screen_flags_a_new_harness_file() -> None:
    diff = "--- /dev/null\n+++ b/tests/unit/conftest.py\n@@\n+import pytest\n"
    assert screen_diff(diff) == ["added harness file tests/unit/conftest.py"]
    assert (
        screen_diff("--- /dev/null\n+++ b/tests/test_new.py\n@@\n+def test_new():\n+    assert 1\n")
        == []
    )
