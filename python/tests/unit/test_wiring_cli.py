"""CLI wiring: --check / --no-default-checks, sandbox flags, config redaction, db migrate."""

from __future__ import annotations

import sys
import types
from pathlib import Path
from typing import Any

import pytest
from pydantic import SecretStr
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.config import Settings
from lha.verify.verifier import DEFAULT_PYTHON_CHECK_COMMANDS

runner = CliRunner()
_DEFAULTS = [list(c) for c in DEFAULT_PYTHON_CHECK_COMMANDS]


def _use_settings(monkeypatch: pytest.MonkeyPatch, settings: Settings) -> None:
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)


def test_resolve_check_commands_defaults_and_extras() -> None:
    assert cli.resolve_check_commands([], False) == _DEFAULTS
    extra = cli.resolve_check_commands(['python -c "print(1)"'], False)
    assert extra == [*_DEFAULTS, ["python", "-c", "print(1)"]]
    assert cli.resolve_check_commands(["make test"], True) == [["make", "test"]]


def test_no_default_checks_requires_a_check() -> None:
    result = runner.invoke(
        cli.app,
        ["run-local", "--title", "t", "--item", "x", "--no-default-checks"],
    )
    assert result.exit_code == 2
    assert "--no-default-checks requires at least one" in result.output


def test_local_sandbox_without_opt_in_is_a_clean_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _use_settings(monkeypatch, Settings(model_backend="stub"))
    workdir = tmp_path / "w"
    result = runner.invoke(
        cli.app,
        [
            "run-local",
            "--title",
            "t",
            "--item",
            "x",
            "--workdir",
            str(workdir),
            "--sandbox",
            "local",
        ],
    )
    assert result.exit_code == 2
    assert "--unsafe-local" in result.output and "Traceback" not in result.output
    assert not workdir.exists()

    bad = runner.invoke(cli.app, ["run-local", "--title", "t", "--item", "x", "--sandbox", "vm"])
    assert bad.exit_code == 2 and "unknown --sandbox" in bad.output


def test_run_local_passes_checks_and_unsafe_local(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _use_settings(monkeypatch, Settings(model_backend="stub"))
    seen: dict[str, Any] = {}

    async def fake_run(**kwargs: Any) -> Any:
        seen.update(kwargs)
        from lha.agent.runner import MissionSummary

        return MissionSummary("m", True, 1, 1, 1, 0.0, "sha", "complete", "")

    monkeypatch.setattr("lha.agent.runner.run_mission_local", fake_run)
    cmd = f"{sys.executable} -c pass"
    result = runner.invoke(
        cli.app,
        [
            *["run-local", "--title", "t", "--item", "x", "--workdir", str(tmp_path)],
            *["--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", cmd],
        ],
    )
    assert result.exit_code == 0, result.output
    assert [c.command for c in seen["checks"]] == [[sys.executable, "-c", "pass"]]
    assert seen["settings"].sandbox == "local" and seen["settings"].allow_unsafe_local


def test_incomplete_run_exits_nonzero(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    _use_settings(monkeypatch, Settings(model_backend="stub", sandbox="docker"))

    async def fake_run(**kwargs: Any) -> Any:
        from lha.agent.runner import MissionSummary

        return MissionSummary("m", False, 3, 0, 1, 0.0, "", "deadlocked: 01 blocked", "")

    monkeypatch.setattr("lha.agent.runner.run_mission_local", fake_run)
    result = runner.invoke(cli.app, ["run-local", "--title", "t", "--item", "x"])
    assert result.exit_code == 1
    assert "deadlocked: 01 blocked" in result.output


def test_config_redacts_secrets(monkeypatch: pytest.MonkeyPatch) -> None:
    _use_settings(
        monkeypatch,
        Settings(anthropic_api_key=SecretStr("sk-very-secret"), postgres_dsn=SecretStr("pg://p")),
    )
    result = runner.invoke(cli.app, ["config"])
    assert result.exit_code == 0
    assert "sk-very-secret" not in result.output and "pg://p" not in result.output
    assert "anthropic_api_key = ***" in result.output
    assert "allow_unpriced_models = False" in result.output


def test_db_migrate_requires_dsn(monkeypatch: pytest.MonkeyPatch) -> None:
    _use_settings(monkeypatch, Settings(postgres_dsn=None))
    result = runner.invoke(cli.app, ["db", "migrate"])
    assert result.exit_code == 2
    assert "LHA_POSTGRES_DSN is not set" in result.output


def test_db_migrate_applies_with_dsn(monkeypatch: pytest.MonkeyPatch) -> None:
    _use_settings(monkeypatch, Settings(postgres_dsn=SecretStr("postgresql://u:p@h/db")))
    calls: list[tuple[str, str]] = []

    async def apply_migrations(dsn: str, *, migrations_dir: str = "db/migrations") -> int:
        calls.append((dsn, migrations_dir))
        return 2

    fake = types.ModuleType("lha.persistence.db")
    fake.apply_migrations = apply_migrations  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "lha.persistence.db", fake)
    result = runner.invoke(cli.app, ["db", "migrate"])
    assert result.exit_code == 0, result.output
    # Run from python/, the default finds the repo's shared ../db/migrations.
    assert calls == [("postgresql://u:p@h/db", "../db/migrations")]
    assert "postgresql://" not in result.output


def test_mission_start_passes_check_commands(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pytest.importorskip("temporalio")
    _use_settings(monkeypatch, Settings(model_backend="stub"))
    started: list[Any] = []

    class _Client:
        async def start_workflow(self, _run: Any, inp: Any, **_kw: Any) -> None:
            started.append(inp)

    async def connect_client(_settings: Settings) -> _Client:
        return _Client()

    monkeypatch.setattr("lha.durable.worker.connect_client", connect_client)
    result = runner.invoke(
        cli.app,
        ["mission-start", "--task", "do it", "--workdir", str(tmp_path), "--check", "make test"],
    )
    assert result.exit_code == 0, result.output
    assert started[0].check_commands == [*_DEFAULTS, ["make", "test"]]


def test_claude_code_failure_is_a_clean_cli_error() -> None:
    """An expired `claude` login is an operator error: `error: ...` and exit 1 (as Go), no traceback."""
    from lha.model.claude_code import ClaudeCodeError

    async def boom() -> None:
        raise ClaudeCodeError("claude -p failed: Failed to authenticate: OAuth session expired")

    with pytest.raises(cli.typer.Exit) as exited:
        cli._run(boom())
    assert exited.value.exit_code == 1
