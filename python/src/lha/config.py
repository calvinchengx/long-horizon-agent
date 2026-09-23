"""Central configuration.

A single ``Settings`` object, populated from environment variables (prefix ``LHA_``)
and an optional ``.env`` file. Nothing in the codebase reads ``os.environ`` directly;
everything goes through here so configuration is one auditable surface.

Secrets (API keys, the Postgres DSN which embeds a password) are ``SecretStr`` so ``repr``/logs
never show them; read the raw value only at the point of use via ``.get_secret_value()``. Use
``Settings.redacted()`` to display configuration.
"""

from __future__ import annotations

import json
from functools import lru_cache
from typing import Literal

from pydantic import Field, SecretStr
from pydantic_settings import BaseSettings, SettingsConfigDict

ModelBackend = Literal["stub", "ollama", "openai_compat", "claude"]
SandboxKind = Literal["docker", "e2b", "local"]

REDACTED = "***"


class Settings(BaseSettings):
    """Runtime configuration for an LHA deployment.

    The defaults are chosen so the system runs at ``$0`` out of the box: the ``stub``
    model backend needs no network, and local SQLite/filesystem stores need no server.
    """

    model_config = SettingsConfigDict(env_prefix="LHA_", env_file=".env", extra="ignore")

    # --- Model layer (pluggable; see src/lha/model/) ---------------------------------
    model_backend: ModelBackend = "stub"
    model_name: str = "stub-1"
    # Local Ollama (truly $0, offline).
    ollama_base_url: str = "http://localhost:11434"
    # Any OpenAI-compatible endpoint (free-tier cloud: Groq / Gemini / OpenRouter, ...).
    openai_base_url: str | None = None
    openai_api_key: SecretStr | None = None
    # Claude (paid API key, or Agent SDK via a Pro/Max subscription).
    anthropic_api_key: SecretStr | None = None
    # Explicit USD prices per million tokens (input / output) for the configured ``model_name``.
    # OpenAI-compatible endpoints have no built-in price table: unset => cost is UNKNOWN (never
    # silently $0) and the budget governor refuses the call unless ``allow_unpriced_models``.
    # For Claude these override the built-in price table for ``model_name`` only.
    openai_price_in_per_mtok: float | None = None
    openai_price_out_per_mtok: float | None = None
    claude_price_in_per_mtok: float | None = None
    claude_price_out_per_mtok: float | None = None
    # Let the governor run models whose cost cannot be computed (spend is then unverifiable).
    allow_unpriced_models: bool = False

    # --- Durable control plane (Temporal) --------------------------------------------
    temporal_address: str = "localhost:7233"
    temporal_namespace: str = "default"
    task_queue: str = "lha-mission"

    # --- Persistence -----------------------------------------------------------------
    # If unset, local SQLite + filesystem stores are used (zero-infra default).
    postgres_dsn: SecretStr | None = None  # DSNs carry credentials
    workspace_root: str = ".lha/workspaces"
    object_store_root: str = ".lha/objects"

    # --- Governor (pre-emptive cost / loop guards) -----------------------------------
    budget_usd_ceiling: float = 10.0
    max_cycles: int = 1000
    max_turns_per_cycle: int = 8
    # Consecutive failed (non-progressing) attempts on the same item before it is declared
    # stuck (the LoopDetector threshold).
    stall_limit: int = 5
    # Replanning: when an item blocks, the model may split it into smaller child items (at most
    # ``max_replans`` splits per mission, nested at most ``max_split_depth`` levels). 0 disables.
    max_replans: int = 20
    max_split_depth: int = 2
    # How long a durable mission waits on a human for an approval / deadlock decision.
    approval_timeout_s: int = 86_400

    # --- Execution sandbox -----------------------------------------------------------
    # Where agent tool calls and verification checks run. "docker" (default) and "e2b" isolate
    # the code; "local" is a plain host subprocess with NO isolation (host filesystem, network
    # and whatever the process can reach), so it is refused unless ``allow_unsafe_local`` is
    # also set (LHA_SANDBOX=local LHA_ALLOW_UNSAFE_LOCAL=true).
    sandbox: SandboxKind = "docker"
    allow_unsafe_local: bool = False
    # Docker image the sandbox runs (needs the toolchains the checks use: this default has Python
    # and uv; see sandbox/Dockerfile for a Go + uv + Node/pnpm image).
    sandbox_image: str = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
    # Comma-separated hosts the SANDBOX may reach (package registries, e.g.
    # "proxy.golang.org,sum.golang.org,pypi.org,files.pythonhosted.org"). Empty = no network.
    # Enforced by an egress proxy on an internal Docker network, not by the agent's goodwill.
    sandbox_egress: str = ""
    # Comma-separated hosts the lead's fetch_url tool may read (reference docs). Empty = no web.
    web_allow_hosts: str = ""
    # Operator-defined checks that run OUTSIDE the sandbox (e.g. e2e suites needing Docker), as a
    # JSON object of name -> argv, referenced by items as witnesses "trusted:<name>".
    trusted_checks: str = ""
    # Comma-separated globs (workspace-relative) the agent may not modify, on top of the test
    # files harness integrity always protects, e.g. "Makefile,e2e/**,.github/**".
    harness_paths: str = ""

    # --- Observability ---------------------------------------------------------------
    langfuse_host: str | None = None
    langfuse_public_key: str | None = None
    langfuse_secret_key: SecretStr | None = None

    # --- Model resilience -------------------------------------------------------------
    # Ordered fallback chain tried on transient model errors, comma-separated
    # ``backend:model[@in/out]`` (USD per 1M tokens), e.g.
    # ``claude:claude-haiku-4-5,openai_compat:llama-3.3-70b@0.59/0.79,ollama:qwen3:8b``.
    fallback_models: str = ""
    fallback_max_rounds: int = Field(default=2, ge=1)

    def sandbox_egress_hosts(self) -> list[str]:
        return _csv(self.sandbox_egress)

    def web_hosts(self) -> list[str]:
        return _csv(self.web_allow_hosts)

    def fallback_model_entries(self) -> list[str]:
        return _csv(self.fallback_models)

    def harness_globs(self) -> list[str]:
        return _csv(self.harness_paths)

    def trusted_check_commands(self) -> dict[str, list[str]]:
        """``trusted_checks`` parsed (raises ``ValueError`` on malformed JSON or entries)."""
        if not self.trusted_checks.strip():
            return {}
        try:
            parsed = json.loads(self.trusted_checks)
        except ValueError as exc:
            raise ValueError(f"LHA_TRUSTED_CHECKS is not valid JSON: {exc}") from exc
        if not isinstance(parsed, dict):
            raise ValueError("LHA_TRUSTED_CHECKS must be a JSON object of name -> argv list")
        out: dict[str, list[str]] = {}
        for name, argv in parsed.items():
            if not (isinstance(argv, list) and argv and all(isinstance(a, str) for a in argv)):
                raise ValueError(
                    f"LHA_TRUSTED_CHECKS[{name!r}] must be a non-empty list of strings"
                )
            out[str(name)] = list(argv)
        return out

    def redacted(self) -> dict[str, object]:
        """All settings as a dict with every ``SecretStr`` field masked (``***`` if set).

        Driven by the field values' types, not a hand-kept list, so a new secret field is
        redacted automatically as long as it is declared ``SecretStr``.
        """
        out: dict[str, object] = {}
        for name in type(self).model_fields:
            value = getattr(self, name)
            out[name] = REDACTED if isinstance(value, SecretStr) else value
        return out


@lru_cache(maxsize=1)
def get_settings() -> Settings:
    """Return the process-wide settings (cached)."""
    return Settings()


def _csv(value: str) -> list[str]:
    return [part.strip() for part in value.split(",") if part.strip()]
