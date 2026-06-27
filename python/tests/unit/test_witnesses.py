"""Witness strings -> item-level acceptance checks (``lha.verify.witnesses``)."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from lha.contracts.state import ChecklistItem
from lha.verify.witnesses import (
    UnknownTrustedCheck,
    go_test_command,
    item_checks,
    parse_witness,
    validate_witness,
)

TRUSTED = {"e2e": ["make", "e2e"], "warehouse-tds": ["./ci.sh", "warehouse-tds"]}


# --- syntax ---------------------------------------------------------------------------------
@pytest.mark.parametrize(
    "witness",
    [
        "go:TestX",
        "go:TestX/sub_case",
        "go:TestX@./internal/...",
        "go:TestX@example.com/mod/pkg",
        "pytest:tests/test_a.py::test_b",
        "pytest:tests/test_a.py::TestC::test_d[a-1]",
        "cmd:make check && echo ok",
        "trusted:e2e",
        "ci:warehouse-tds",
        "  go:TestPadded  ",
    ],
)
def test_valid_witnesses(witness: str) -> None:
    validate_witness(witness)


@pytest.mark.parametrize(
    ("witness", "fragment"),
    [
        ("TestX", "unknown kind"),
        ("sdk:TestX", "unknown kind"),
        ("go:", "Go test name"),
        ("go:1Test", "Go test name"),
        ("go:TestX/", "Go test name"),
        ("go:TestX;rm -rf /", "Go test name"),
        ("go:TestX@./...;rm", "package pattern"),
        ("go:TestX@-exec=evil", "package pattern"),
        ("go:TestX@$(evil)", "package pattern"),
        ("pytest:", "pytest node id"),
        ("pytest:tests/a.py -x", "pytest node id"),
        ("pytest:--rootdir=/", "pytest node id"),
        ("pytest:a.py;evil", "pytest node id"),
        ("cmd:   ", "needs a shell command"),
        ("trusted:", "operator-defined"),
        ("ci:has space", "operator-defined"),
    ],
)
def test_invalid_witnesses(witness: str, fragment: str) -> None:
    with pytest.raises(ValueError, match="witness") as info:
        validate_witness(witness)
    assert fragment in str(info.value)


# --- parsing --------------------------------------------------------------------------------
def test_pytest_witness() -> None:
    check = parse_witness("pytest:tests/test_a.py::test_b", TRUSTED)
    assert check.command == ["uv", "run", "pytest", "-q", "tests/test_a.py::test_b"]
    assert check.name == "pytest:tests/test_a.py::test_b"
    assert check.where == "sandbox" and check.gating


def test_cmd_witness() -> None:
    check = parse_witness("cmd: make check ", TRUSTED)
    assert check.command == ["sh", "-c", "make check"]
    assert check.name == "cmd: make check"


def test_long_cmd_name_is_clipped() -> None:
    check = parse_witness("cmd:" + "echo x; " * 30, TRUSTED)
    assert len(check.name) == 80 and check.name.endswith("...")


def test_go_witness_defaults_to_all_packages() -> None:
    check = parse_witness("go:TestX", TRUSTED)
    assert check.command[:2] == ["sh", "-c"]
    script = check.command[2]
    assert "go test -count=1 -run '^TestX$' -v ./..." in script
    assert "--- PASS: TestX( |$)" in script


def test_go_witness_package_pattern_and_subtests() -> None:
    script = parse_witness("go:TestX/sub@./internal/...", TRUSTED).command[2]
    assert "-run '^TestX$/^sub$' -v ./internal/..." in script
    assert "--- PASS: TestX/sub( |$)" in script


def test_trusted_and_ci_witnesses() -> None:
    check = parse_witness("trusted:e2e", TRUSTED)
    assert check.where == "trusted" and check.command == ["make", "e2e"]
    alias = parse_witness("ci:warehouse-tds", TRUSTED)
    assert alias.where == "trusted" and alias.command == ["./ci.sh", "warehouse-tds"]
    assert alias.name == "ci:warehouse-tds"


def test_trusted_command_is_copied() -> None:
    check = parse_witness("trusted:e2e", TRUSTED)
    check.command.append("mutated")
    assert TRUSTED["e2e"] == ["make", "e2e"]


def test_unknown_trusted_check_names_known_ones() -> None:
    with pytest.raises(UnknownTrustedCheck, match=r"known: e2e, warehouse-tds"):
        parse_witness("trusted:nope", TRUSTED)
    with pytest.raises(UnknownTrustedCheck, match="LHA_TRUSTED_CHECKS"):
        parse_witness("ci:e2e", {})
    assert issubclass(UnknownTrustedCheck, ValueError)


def test_parse_rejects_bad_syntax() -> None:
    with pytest.raises(ValueError, match="unknown kind"):
        parse_witness("bogus", TRUSTED)


def test_item_checks_are_ordered_and_unique() -> None:
    item = ChecklistItem(
        id="01",
        description="x",
        witnesses=["go:TestA", "trusted:e2e", "go:TestA", "cmd:true"],
    )
    checks = item_checks(item, TRUSTED)
    assert [c.name for c in checks] == ["go:TestA", "trusted:e2e", "go:TestA-2", "cmd:true"]
    assert [c.where for c in checks] == ["sandbox", "trusted", "sandbox", "sandbox"]
    assert item_checks(ChecklistItem(id="02", description="y"), TRUSTED) == []


def test_item_checks_propagates_unknown_trusted() -> None:
    item = ChecklistItem(id="01", description="x", witnesses=["ci:missing"])
    with pytest.raises(UnknownTrustedCheck):
        item_checks(item, TRUSTED)


# --- the go: witness against a real toolchain -----------------------------------------------
_GO_TEST = """package demo

import "testing"

func TestPasses(t *testing.T) {
	t.Run("inner", func(t *testing.T) {})
}

func TestFails(t *testing.T) { t.Fatal("boom") }

func TestSkips(t *testing.T) { t.Skip("needs a sidecar") }
"""


@pytest.fixture
def go_module(tmp_path: Path) -> Path:
    if shutil.which("go") is None:
        pytest.skip("go toolchain not on PATH")
    (tmp_path / "go.mod").write_text("module example.com/demo\n\ngo 1.21\n")
    (tmp_path / "demo_test.go").write_text(_GO_TEST)
    return tmp_path


def _run(argv: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(argv, cwd=cwd, capture_output=True, text=True, timeout=300, check=False)


def test_go_witness_passes_for_existing_passing_test(go_module: Path) -> None:
    proc = _run(parse_witness("go:TestPasses", {}).command, go_module)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "--- PASS: TestPasses" in proc.stdout


def test_go_witness_subtest(go_module: Path) -> None:
    assert _run(go_test_command("TestPasses/inner"), go_module).returncode == 0


def test_go_witness_fails_for_missing_test(go_module: Path) -> None:
    proc = _run(parse_witness("go:TestDoesNotExist", {}).command, go_module)
    assert proc.returncode != 0
    assert "did not run and pass" in proc.stderr


def test_go_witness_does_not_match_a_prefix(go_module: Path) -> None:
    assert _run(go_test_command("TestPass"), go_module).returncode != 0


def test_go_witness_fails_for_failing_test(go_module: Path) -> None:
    proc = _run(parse_witness("go:TestFails", {}).command, go_module)
    assert proc.returncode != 0
    assert "boom" in proc.stdout


def test_go_witness_fails_for_skipped_test(go_module: Path) -> None:
    proc = _run(parse_witness("go:TestSkips@.", {}).command, go_module)
    assert proc.returncode != 0
