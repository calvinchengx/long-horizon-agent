"""Settings keep secrets out of repr/dumps and expose sandbox selection (C8)."""

from __future__ import annotations

import pytest
from pydantic import SecretStr

from lha.config import REDACTED, Settings


def test_secrets_never_in_repr_and_redacted_helper() -> None:
    settings = Settings(
        anthropic_api_key="sk-ant-VALUE1",
        openai_api_key="VALUE2",
        langfuse_secret_key="VALUE3",
        postgres_dsn="postgresql://u:pw@h/db",
    )
    assert "VALUE" not in repr(settings)
    assert "pw@" not in repr(settings)
    shown = settings.redacted()
    for key in ("anthropic_api_key", "openai_api_key", "langfuse_secret_key", "postgres_dsn"):
        assert shown[key] == REDACTED
    assert shown["model_backend"] == "stub"
    assert isinstance(settings.anthropic_api_key, SecretStr)
    assert settings.anthropic_api_key.get_secret_value() == "sk-ant-VALUE1"


def test_unset_secrets_shown_as_none() -> None:
    assert Settings(_env_file=None).redacted()["anthropic_api_key"] is None  # type: ignore[call-arg]


def test_sandbox_settings_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    defaults = Settings(_env_file=None)  # type: ignore[call-arg]
    assert defaults.sandbox == "docker"
    assert defaults.allow_unsafe_local is False
    monkeypatch.setenv("LHA_SANDBOX", "local")
    monkeypatch.setenv("LHA_ALLOW_UNSAFE_LOCAL", "true")
    settings = Settings(_env_file=None)  # type: ignore[call-arg]
    assert settings.sandbox == "local"
    assert settings.allow_unsafe_local is True
