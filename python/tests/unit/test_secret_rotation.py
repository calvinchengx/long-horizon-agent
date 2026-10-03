"""Secret rotation: every activity re-reads the environment and takes the rotated secrets, so a
key rotated mid-mission is used without a worker restart (``config.refresh_secrets``)."""

from __future__ import annotations

import asyncio
from pathlib import Path

import pytest
from pydantic import SecretStr
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.config import (
    SECRET_FIELDS,
    Settings,
    refresh_secrets,
    secret_fingerprint,
    secret_fingerprints,
    with_rotated_secrets,
)
from lha.contracts.state import Checklist, ChecklistItem, SituationSnapshot
from lha.durable.activities import SECRETS_ROTATED_EVENT, make_cycle_activity
from lha.durable.types import CycleInput
from lha.model.stub import StubModel
from lha.state.mission_anchor import GitMissionAnchor
from tests.durability._support import CHECK_COMMANDS, SETTINGS, _done, write_turn

runner = CliRunner()


def test_secret_fields_and_fingerprints() -> None:
    assert "anthropic_api_key" in SECRET_FIELDS and "postgres_dsn" in SECRET_FIELDS
    assert "model_name" not in SECRET_FIELDS
    assert secret_fingerprint(None) == ""
    fp = secret_fingerprint(SecretStr("sk-ant-one"))
    assert len(fp) == 12 and fp == secret_fingerprint(SecretStr("sk-ant-one"))
    assert fp != secret_fingerprint(SecretStr("sk-ant-two"))
    settings = Settings(_env_file=None, anthropic_api_key=SecretStr("sk-ant-one"))  # type: ignore[call-arg]
    assert secret_fingerprints(settings) == {"anthropic_api_key": fp}
    assert "sk-ant-one" not in str(secret_fingerprints(settings))


def test_with_rotated_secrets_changes_only_the_secrets_that_differ() -> None:
    base = Settings(
        _env_file=None,
        model_name="m-old",
        anthropic_api_key=SecretStr("a1"),
        voyage_api_key=SecretStr("v1"),
    )  # type: ignore[call-arg]
    fresh = Settings(
        _env_file=None,
        model_name="m-new",
        anthropic_api_key=SecretStr("a2"),
        voyage_api_key=SecretStr("v1"),
        openai_api_key=SecretStr("o1"),
    )  # type: ignore[call-arg]
    rotated, changed = with_rotated_secrets(base, fresh)
    assert changed == ["openai_api_key", "anthropic_api_key"]  # declaration order
    assert (
        rotated.anthropic_api_key is not None
        and rotated.anthropic_api_key.get_secret_value() == "a2"
    )
    assert rotated.openai_api_key is not None and rotated.openai_api_key.get_secret_value() == "o1"
    assert rotated.voyage_api_key is not None and rotated.voyage_api_key.get_secret_value() == "v1"
    assert rotated.model_name == "m-old"  # a non-secret change is not a rotation
    same, none = with_rotated_secrets(base, base)
    assert same is base and none == []
    # A secret absent from the fresh read is kept: removing a key is a restart, not a rotation.
    kept, gone = with_rotated_secrets(
        base, Settings(_env_file=None, anthropic_api_key=SecretStr("a1"))
    )  # type: ignore[call-arg]
    assert gone == [] and kept is base


def test_refresh_secrets_reads_the_environment(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.chdir(tmp_path)  # no .env here
    monkeypatch.delenv("LHA_ANTHROPIC_API_KEY", raising=False)
    base = Settings(_env_file=None, anthropic_api_key=SecretStr("old"))  # type: ignore[call-arg]
    monkeypatch.setenv("LHA_ANTHROPIC_API_KEY", "new")
    fresh, changed = refresh_secrets(base)
    assert changed == ["anthropic_api_key"]
    assert (
        fresh.anthropic_api_key is not None and fresh.anthropic_api_key.get_secret_value() == "new"
    )
    (tmp_path / ".env").write_text("LHA_VOYAGE_API_KEY=from-dotenv\n", encoding="utf-8")
    again, changed = refresh_secrets(fresh)
    assert changed == ["voyage_api_key"]
    assert (
        again.voyage_api_key is not None
        and again.voyage_api_key.get_secret_value() == "from-dotenv"
    )


def test_a_cycle_uses_the_rotated_key_and_records_it(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.chdir(tmp_path)
    monkeypatch.delenv("LHA_ANTHROPIC_API_KEY", raising=False)
    workdir = tmp_path / "ws"
    anchor = GitMissionAnchor(workdir)
    asyncio.run(
        anchor.initialize(
            title="t",
            description="d",
            items=Checklist(items=[ChecklistItem(id="01", description="x")]),
        )
    )
    seen: list[str] = []

    def factory(settings: Settings, snapshot: SituationSnapshot) -> StubModel:
        key = settings.anthropic_api_key
        seen.append(key.get_secret_value() if key else "")
        return StubModel(script=[write_turn("work/01.txt"), _done()])

    started = SETTINGS.model_copy(update={"anthropic_api_key": SecretStr("key-at-start")})
    cycle = make_cycle_activity(settings=started, model_factory=factory)
    monkeypatch.setenv("LHA_ANTHROPIC_API_KEY", "key-rotated")
    result = asyncio.run(
        cycle(
            CycleInput(
                mission_id="m", workdir=str(workdir), cycle_id="c1", check_commands=CHECK_COMMANDS
            )
        )
    )
    assert result.advanced and seen == ["key-rotated"]
    events = [e for e in asyncio.run(anchor.read_events()) if e.kind == SECRETS_ROTATED_EVENT]
    assert len(events) == 1 and events[0].cycle_id == "c1"
    assert events[0].payload["fields"] == ["anthropic_api_key"]
    fp = events[0].payload["fingerprints"]["anthropic_api_key"]
    assert fp == secret_fingerprint(SecretStr("key-rotated")) and "key-rotated" not in str(
        events[0].payload
    )
    # The worker's own settings object is untouched; the next cycle without a change records nothing.
    assert (
        started.anthropic_api_key is not None
        and started.anthropic_api_key.get_secret_value() == "key-at-start"
    )


def test_config_fingerprints(monkeypatch: pytest.MonkeyPatch) -> None:
    settings = Settings(_env_file=None, anthropic_api_key=SecretStr("sk-ant-one"))  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    plain = runner.invoke(cli.app, ["config"])
    assert (
        plain.exit_code == 0
        and "fingerprint" not in plain.output
        and "sk-ant-one" not in plain.output
    )
    with_fp = runner.invoke(cli.app, ["config", "--fingerprints"])
    assert with_fp.exit_code == 0
    assert (
        f"anthropic_api_key fingerprint = {secret_fingerprint(SecretStr('sk-ant-one'))}"
        in with_fp.output
    )
    assert "sk-ant-one" not in with_fp.output
