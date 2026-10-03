"""Observability payloads never carry secrets (C8)."""

from __future__ import annotations

from pydantic import SecretStr

from lha.obs.events import TraceRecorder
from lha.obs.redact import REDACTED, redact_mapping, redact_text, redact_value


def test_secret_keys_redacted_but_token_counters_kept() -> None:
    out = redact_mapping(
        {
            "api_key": "abc",
            "Authorization": "Bearer xyz",
            "db_password": "hunter2",
            "postgres_dsn": "postgresql://u:p@h/db",
            "token": "t0k",
            "input_tokens": 120,
            "max_tokens": 4096,
            "nested": {"anthropic_api_key": "k", "ok": "fine"},
            "wrapped": SecretStr("s"),
        }
    )
    assert out["api_key"] == REDACTED
    assert out["Authorization"] == REDACTED
    assert out["db_password"] == REDACTED
    assert out["postgres_dsn"] == REDACTED
    assert out["token"] == REDACTED
    assert out["input_tokens"] == 120
    assert out["max_tokens"] == 4096
    assert out["nested"] == {"anthropic_api_key": REDACTED, "ok": "fine"}
    assert out["wrapped"] == REDACTED


def test_secret_looking_values_redacted_in_free_text() -> None:
    text = redact_text(
        "called with sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV and header Bearer eyJhbGciOi.x.y "
        "db postgresql://admin:s3cret@db:5432/lha"
    )
    assert "ABCDEFGHIJ" not in text
    assert "eyJhbGciOi" not in text
    assert "s3cret" not in text
    assert "postgresql://admin:***@db:5432/lha" in text


def test_trace_recorder_redacts_event_data() -> None:
    recorder = TraceRecorder()
    event = recorder.record(
        "test_redacted",
        mission_id="m",
        api_key="sk-live",
        output="key=sk-ABCDEFGHIJKLMNOPQRST",
        n=3,
    )
    assert event.data["api_key"] == REDACTED
    assert "ABCDEFGHIJ" not in str(event.data["output"])
    assert event.data["n"] == 3
    assert "sk-live" not in recorder.to_jsonl()


# --- Rules pinned by the mutation audit (docs/20-testing.md#mutation-audit) ---------------------


# Sequences are redacted item by item (tuples come back as lists).
def test_sequences_are_redacted_per_item() -> None:
    key = "sk-" + "a" * 20
    assert redact_value([key, "plain", 3]) == [REDACTED, "plain", 3]
    assert redact_value((key,)) == [REDACTED]


# An empty or unset secret stays visible as such (it leaks nothing and shows it is unset).
def test_empty_secret_values_are_not_masked() -> None:
    assert redact_mapping({"api_key": "", "password": None}) == {"api_key": "", "password": None}
