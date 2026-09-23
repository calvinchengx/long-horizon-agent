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

    # --- Multi-agent coordination (lha orchestrate) ---------------------------------
    # Most checklist items one parallel wave runs at once, each by its own implementer in its
    # own git worktree (items need disjoint, Planner-assigned write-sets). 1 disables parallel
    # waves: every item is then worked serially by the Lead.
    max_parallel_implementers: int = 3

    # --- Observability ---------------------------------------------------------------
    langfuse_host: str | None = None
    langfuse_public_key: str | None = None
    langfuse_secret_key: SecretStr | None = None

    # ==================================================================================
    # --- Persistence backend + tiered memory (lha.persistence.store, lha.memory.service)
    # ==================================================================================
    # With ``postgres_dsn`` unset, mission rows, the cost ledger, episodic events, semantic
    # memory and skills go to a local SQLite file (WAL mode). Empty (the default) = one per-user
    # file every process shares: $XDG_DATA_HOME/lha/lha.sqlite3, else ~/Library/Application
    # Support/lha/lha.sqlite3 (macOS) or ~/.local/share/lha/lha.sqlite3
    # (``persistence.store.default_sqlite_path``). A relative path resolves against the process's
    # working directory (with a warning); if the path would land inside a mission's git checkout
    # it is moved under that checkout's ``.git/lha/`` instead (never committed, survives resets).
    sqlite_path: str = ""
    # When ``postgres_dsn`` is set but Postgres is unreachable / unmigrated / psycopg is not
    # installed: ``True`` = log a warning and use SQLite instead; ``False`` = fail the run.
    postgres_fallback_to_sqlite: bool = True
    # Tiered memory in the lead's prompt (episodic + semantic + skills, with consolidation).
    memory_enabled: bool = True
    # Hard cap (characters, ~4 chars/token) on the memory block added to each cycle's prompt.
    memory_prompt_budget_chars: int = 4000
    memory_episodic_k: int = 4  # past attempts/outcomes recalled per cycle
    memory_semantic_k: int = 4  # repo chunks / facts / progress / decisions recalled per cycle
    memory_skills_k: int = 2  # verified skills recalled per cycle
    # Dense channel of hybrid retrieval. "hash" = the built-in offline HashEmbedder (lexical
    # hashing, no extra needed); "sentence_transformers" needs the ``embeddings`` extra (falls
    # back to lexical-only retrieval if missing); "none" = lexical-only (BM25 + git grep).
    memory_embedder: Literal["hash", "sentence_transformers", "none"] = "hash"
    memory_embedding_model: str = "BAAI/bge-m3"  # for memory_embedder=sentence_transformers
    # Second-stage rerank: "none" keeps fusion order; "cross_encoder" needs the embeddings extra.
    memory_rerank: Literal["none", "cross_encoder"] = "none"
    # Consolidate episodic -> semantic every N recorded cycles (0 disables). "extractive" is
    # deterministic and free; "model" asks the (metered) lead model to distill facts.
    memory_consolidate_every: int = 5
    memory_consolidation: Literal["extractive", "model"] = "extractive"
    # Index the checkout's tracked text files (bounded) as semantic-retrieval candidates.
    memory_index_repo_files: bool = True
    memory_max_repo_files: int = 400

    # --- Web tools (fetch_url / web_search; allow-list is ``web_allow_hosts`` above) -----
    # Extra ports fetch_url may use beyond 80/443 (comma-separated).
    web_allow_ports: str = ""
    # Brokered credentials for fetch_url: a JSON object mapping a placeholder the agent may use
    # in headers to ``{"value": "<secret>", "hosts": ["api.example.com", ...]}``. The secret is
    # substituted only into requests to its bound hosts (each must be in ``web_allow_hosts``).
    web_credentials: SecretStr | None = None
    # web_search backend; registered only with a provider AND a key (and a non-empty
    # ``web_allow_hosts``). The endpoint defaults to the provider's public API.
    web_search_provider: Literal["tavily", "exa"] | None = None
    web_search_api_key: SecretStr | None = None
    web_search_endpoint: str | None = None
    web_timeout_s: float = Field(default=30.0, gt=0)
    web_max_response_bytes: int = Field(default=2_000_000, gt=0)
    # Declare that the workspace/sandbox exposes secrets or customer data (Rule of Two: a run
    # with web tools may not also hold private data). ``sandbox=local`` always counts as private.
    private_data: bool = False

    # --- Model resilience -------------------------------------------------------------
    # Ordered fallback chain tried on transient model errors, comma-separated
    # ``backend:model[@in/out]`` (USD per 1M tokens), e.g.
    # ``claude:claude-haiku-4-5,openai_compat:llama-3.3-70b@0.59/0.79,ollama:qwen3:8b``.
    fallback_models: str = ""
    fallback_max_rounds: int = Field(default=2, ge=1)
    # Timeout of the model health probe a parked durable mission runs before resuming.
    model_probe_timeout_s: float = Field(default=10.0, gt=0)

    # --- Human gates: escalation ladder, webhook, deadlock gate, SLEEPING --------------
    # (see src/lha/hitl/ and durable/workflows.py). Every default is the SAFE one: a gate nobody
    # answers REJECTS the tool call, and the deadlock gate aborts.
    # Local runs with --approve-interactive: how long the terminal prompt waits before rejecting.
    # (Durable missions take their approval timeout from ``mission-start``.)
    console_approval_timeout_s: int = Field(default=3600, ge=1)
    # Escalation ladder for every gate: reminder offsets (seconds after the gate opens). Each
    # reminder is recorded as an event and sent to the webhook; offsets at or past the gate's
    # timeout are ignored, then the default applies. JSON list in the environment.
    gate_escalation_seconds: list[int] = Field(default_factory=lambda: [900, 2700, 14_400, 43_200])
    # Deadlock gate decision when nobody answers: "abort" or "impossible" (never "retry").
    deadlock_gate_default: Literal["abort", "impossible"] = "abort"
    # Consecutive failed cycles on one item after which the deadlock gate recommends
    # "impossible" (``ops.lifecycle.should_declare_impossible``).
    impossible_after_failures: int = Field(default=3, ge=1)
    # Durable pause between cycles (status SLEEPING); 0 = none.
    cycle_pause_seconds: int = Field(default=0, ge=0)
    # Optional webhook receiving every gate event (opened / reminder / resolved / defaulted) as a
    # JSON POST. Off by default. A secret: chat webhook URLs embed their credential.
    gate_webhook_url: SecretStr | None = None
    gate_webhook_timeout_seconds: float = Field(default=5.0, gt=0, le=60)

    def sandbox_egress_hosts(self) -> list[str]:
        return _csv(self.sandbox_egress)

    def web_hosts(self) -> list[str]:
        return _csv(self.web_allow_hosts)

    def web_ports(self) -> list[int]:
        """``web_allow_ports`` parsed (raises ``ValueError`` on a non-integer entry)."""
        try:
            return [int(port) for port in _csv(self.web_allow_ports)]
        except ValueError as exc:
            raise ValueError(f"LHA_WEB_ALLOW_PORTS must be integers: {exc}") from None

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
