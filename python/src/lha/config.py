"""Central configuration.

A single ``Settings`` object, populated from environment variables (prefix ``LHA_``)
and an optional ``.env`` file. Every LHA setting is read here and nowhere else, so configuration
is one auditable surface. The few direct ``os.environ`` reads elsewhere are deliberate, and none
of them reads an LHA setting:

* ``execution/proc.py`` (``child_env``), ``state/git_ops.py`` (``_git_env``),
  ``verify/trusted.py`` (trusted checks) and ``model/claude_code.py`` (``child_env``) build a
  child process's environment from this one: only an allowlisted subset for sandboxed commands;
  the host environment plus fixed overrides for ``git``, trusted host-side checks and the
  ``claude`` CLI (which needs its own login and ``ANTHROPIC_*``/``CLAUDE_*`` variables).
* ``persistence/store.py`` (``default_sqlite_path``) follows the platform conventions
  ``XDG_DATA_HOME`` and ``LOCALAPPDATA`` for the default store location; ``LHA_SQLITE_PATH``
  (``sqlite_path``) overrides it.
* ``execution/egress_proxy.py``'s ``_main`` runs only when the file is executed as a standalone
  script inside the egress proxy container, where ``Settings`` is not available; it reads
  ``LHA_PROXY_ALLOW`` and ``LHA_PROXY_PORT`` (set by the Docker sandbox) and ``LHA_PROXY_BIND``.

Third-party libraries also read their own standard variables (the OpenTelemetry OTLP exporter
reads ``OTEL_EXPORTER_OTLP_HEADERS``, for example), and tests read their ``LHA_IT_*`` and
``LHA_RECORD_HISTORY`` switches directly; neither is application configuration.

Secrets (API keys, the Postgres DSN which embeds a password) are ``SecretStr`` so ``repr``/logs
never show them; read the raw value only at the point of use via ``.get_secret_value()``. Use
``Settings.redacted()`` to display configuration.
"""

from __future__ import annotations

import hashlib
import json
from functools import lru_cache
from typing import Literal

from pydantic import AliasChoices, Field, SecretStr, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

ModelBackend = Literal["stub", "ollama", "openai_compat", "claude", "claude_code", "opencode"]
LeadEngine = Literal["loop", "claude_code", "opencode"]
ClaudeCodeTools = Literal["lha", "native"]
OpenCodeTools = Literal["lha", "native"]
SandboxKind = Literal["docker", "e2b", "local"]
WorkerVersioningBehavior = Literal["pinned", "auto_upgrade"]

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

    # --- Claude Code (``claude -p``; see src/lha/model/claude_code.py) -----------------
    # ``model_backend=claude_code`` runs each model turn through the ``claude`` CLI, so a Claude
    # Pro/Max login works without an API key; ``model_name`` is passed as ``--model`` (an alias
    # such as ``sonnet`` or a full model id; ``default`` keeps Claude Code's own choice).
    # ``lead_engine=claude_code`` goes further: each lead cycle is ONE ``claude -p`` session that
    # works the item with its own agentic loop, then LHA verifies and checkpoints as usual.
    lead_engine: LeadEngine = "loop"
    claude_code_bin: str = "claude"
    # lha: Claude Code's built-in tools are off; it gets LHA's tools over MCP, so the sandbox,
    # the human gate and egress rules still apply. native: its own Read/Edit/Bash on the host
    # workdir, with no isolation (needs sandbox=local and allow_unsafe_local).
    claude_code_tools: ClaudeCodeTools = "lha"
    # Passed as ``--max-budget-usd`` to each ``claude -p`` call and used as that call's
    # worst-case cost by the budget governor. Claude Code checks it between API calls, so one
    # call can overshoot it by a single turn.
    claude_code_max_budget_usd: float = Field(default=5.0, gt=0)
    claude_code_timeout_s: float = Field(default=3600.0, gt=0)

    # --- OpenCode (``opencode run``; see src/lha/model/opencode.py) -------------------
    # ``model_backend=opencode`` runs each model turn through the ``opencode`` CLI (a session with
    # every tool denied), so an OpenCode login works without an LHA-side API key; ``model_name`` is
    # passed as ``--model`` (``provider/model#variant``; left at its default, OpenCode chooses).
    # ``lead_engine=opencode`` goes further: each lead cycle is ONE ``opencode run`` session that
    # works the item with its own agentic loop, then LHA verifies and checkpoints as usual.
    opencode_bin: str = "opencode"
    opencode_model: str = ""
    opencode_agent: str = "lha"
    # lha: OpenCode's own tools are denied (an injected agent) and it gets LHA's tools over MCP,
    # so the sandbox, the human gate and egress rules still apply. native: its own read/edit/shell
    # on the host workdir, with no isolation (needs sandbox=local and allow_unsafe_local).
    opencode_tools: OpenCodeTools = "lha"
    # Run each session against a private server (``--standalone``) so it never attaches to the
    # OpenCode that may be running LHA.
    opencode_standalone: bool = True
    # The session's worst-case cost and spend cap. OpenCode has no cap flag, so the engine kills a
    # session that reaches it while streaming; its reported per-step cost is what is recorded.
    opencode_max_budget_usd: float = Field(default=5.0, gt=0)
    opencode_timeout_s: float = Field(default=3600.0, gt=0)

    # --- Durable control plane (Temporal) --------------------------------------------
    temporal_address: str = "localhost:7233"
    temporal_namespace: str = "default"
    task_queue: str = "lha-mission"
    # How often a running worker re-checks who polls its task queue (the cross-language guard,
    # lha.durable.worker): a worker of the other implementation that appears stops this one.
    worker_guard_interval_s: float = Field(default=30.0, gt=0)
    # Worker versioning (Temporal Worker Deployments): off unless both are set. The worker then
    # polls as build ``worker_build_id`` of deployment ``worker_deployment``, and a mission stays
    # on the build that started it (``pinned``) or moves to the current build at its next task
    # (``auto_upgrade``, relying on the workflows' patch guards).
    worker_deployment: str = ""
    worker_build_id: str = ""
    worker_versioning_behavior: WorkerVersioningBehavior = "pinned"
    # Make this worker's build the deployment's current version once it polls, so new missions
    # start on it (otherwise: `temporal worker deployment set-current-version`).
    worker_promote: bool = False

    # --- Persistence -----------------------------------------------------------------
    # If unset, local SQLite + filesystem stores are used (zero-infra default).
    postgres_dsn: SecretStr | None = None  # DSNs carry credentials
    workspace_root: str = ".lha/workspaces"
    object_store_root: str = ".lha/objects"
    # Days after which `lha worker` deletes untouched ClaimCheck objects when it starts (0 = never;
    # choose longer than your longest mission plus the namespace's history retention).
    object_retention_days: int = Field(default=0, ge=0)

    # --- Governor (pre-emptive cost / loop guards) -----------------------------------
    budget_usd_ceiling: float = 10.0
    max_cycles: int = 1000
    # Model turns in one cycle. A strong model spends many finding the code before it edits; with
    # 8, Sonnet often ran out before writing anything (docs/24-large-missions.md).
    max_turns_per_cycle: int = 20
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
    # Hosts the Docker SANDBOX may reach, enforced by an egress proxy on an internal Docker
    # network (not by the agent's goodwill); all three empty = no network. Split by what a host
    # lets code in the sandbox do (lha.execution.egress_hosts):
    # - sandbox_egress: package-registry download hosts only, from a fixed list (pypi.org,
    #   files.pythonhosted.org, registry.npmjs.org, proxy.golang.org, sum.golang.org,
    #   storage.googleapis.com, crates.io, static.crates.io, index.crates.io);
    # - sandbox_egress_extra_hosts: any other host, except known push/upload hosts;
    # - sandbox_egress_allow_write_hosts: hosts accepted although code in the sandbox can push or
    #   upload there (github.com, *.amazonaws.com, upload.pypi.org, ...).
    # Any of them makes the run hold untrusted input + external comms (Rule of Two).
    sandbox_egress: str = ""
    sandbox_egress_extra_hosts: str = ""
    sandbox_egress_allow_write_hosts: str = ""
    # Docker sandbox resource limits (memory incl. swap, in Docker's notation; CPUs). A large
    # build needs more than the default: Go compiling a big dependency is OOM-killed at 2g.
    sandbox_memory: str = "2g"
    sandbox_cpus: float = Field(default=2.0, gt=0)
    # Size of the sandbox's /tmp tmpfs, which holds toolchain caches (Go's module and build
    # cache, uv's cache). It counts against ``sandbox_memory``.
    sandbox_tmp_size: str = "1g"
    # Comma-separated hosts the lead's fetch_url tool may read (reference docs). Empty = no web.
    web_allow_hosts: str = ""
    # Operator-defined checks that run OUTSIDE the sandbox (e.g. e2e suites needing Docker), as a
    # JSON object of name -> argv, referenced by items as witnesses "trusted:<name>".
    trusted_checks: str = ""
    # Comma-separated names of host environment variables passed through to trusted checks, e.g.
    # "GOFLAGS,GOPROXY". A trusted check otherwise gets only PATH, the locale and a fresh empty
    # HOME/TMPDIR (never the operator's credentials); LHA_* names are refused. A listed name is
    # visible to agent-written code (tests, build scripts), so list only what checks need.
    trusted_check_env: str = ""
    # Comma-separated globs (workspace-relative) the agent may not modify, on top of the test
    # files harness integrity always protects, e.g. "Makefile,e2e/**,.github/**".
    harness_paths: str = ""
    # Comma-separated extra paths (gitignore patterns, workspace-relative) that survive the clean
    # at the start of every durable cycle attempt, on top of .venv, venv, node_modules, .env*
    # and .lha/objects: e.g. build caches ("target") or a local git remote. List only IGNORED
    # paths: a kept untracked file that is not ignored would be committed by the next checkpoint.
    reset_keep: str = ""
    # Re-runs of a failing (not timed-out) gating check on the same work tree before it counts
    # as failed; a check that passes on a re-run is quarantined (non-gating for the rest of the
    # mission, see lha.verify.flaky_quarantine). 0 = no re-runs and no quarantine.
    flaky_retries: int = Field(default=1, ge=0, le=5)
    # Opt-in mutation gate (lha.verify.mutation_gate): a shell command run in the sandbox after
    # every gating check passed, with the changed files in LHA_CHANGED_FILES; a non-zero exit
    # (surviving mutants) keeps the item red. Empty = off.
    mutation_check: str = ""
    mutation_timeout_s: int = Field(default=1800, gt=0)

    # --- Multi-agent coordination (lha orchestrate) ---------------------------------
    # Most checklist items one parallel wave runs at once, each by its own implementer in its
    # own git worktree (items need disjoint, Planner-assigned write-sets). 1 disables parallel
    # waves: every item is then worked serially by the Lead.
    max_parallel_implementers: int = 3

    # --- Observability (lha.obs.otel; needs the ``observability`` extra) ---------------
    # OTLP/HTTP collector base URL (``/v1/traces`` is appended). The standard
    # OTEL_EXPORTER_OTLP_ENDPOINT is read too; the LHA_ name wins when both are set.
    otel_exporter_otlp_endpoint: str | None = Field(
        default=None,
        validation_alias=AliasChoices(
            "LHA_OTEL_EXPORTER_OTLP_ENDPOINT", "otel_exporter_otlp_endpoint"
        ),
    )
    # The standard OpenTelemetry kill switch: when true, no tracer provider is installed.
    otel_sdk_disabled: bool = Field(
        default=False, validation_alias=AliasChoices("otel_sdk_disabled", "OTEL_SDK_DISABLED")
    )
    otel_service_name: str = "lha"
    # Seconds one span export may take before it is dropped (the agent never waits on it).
    otel_export_timeout_s: int = 5
    # Langfuse: with all three set, spans also go to <host>/api/public/otel (OTLP, Basic auth).
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
    # hashing, no extra needed; the default because it needs nothing running); "ollama" = real
    # semantic embeddings from a local Ollama (``LHA_OLLAMA_BASE_URL``, POST /api/embed, $0);
    # "voyage" = the paid Voyage AI API (``LHA_VOYAGE_API_KEY``, public HTTPS only);
    # "sentence_transformers" needs the ``embeddings`` extra. Every semantic choice falls back
    # to lexical-only retrieval when unavailable; "none" = lexical-only (BM25 + git grep).
    memory_embedder: Literal["hash", "ollama", "voyage", "sentence_transformers", "none"] = "hash"
    # The embedding model; empty = "nomic-embed-text" for ollama, "voyage-4" for voyage,
    # "BAAI/bge-m3" for sentence_transformers (unused by hash / none).
    memory_embedding_model: str = ""
    # Voyage AI embedder (``memory_embedder=voyage``): the API key (a secret) and an optional
    # endpoint (default https://api.voyageai.com/v1/embeddings; https on a public address only).
    voyage_api_key: SecretStr | None = None
    voyage_endpoint: str | None = None
    # Second-stage rerank: "none" keeps fusion order; "cross_encoder" needs the embeddings extra;
    # "system_one" asks the System One model (below) whether each passage helps with the task.
    memory_rerank: Literal["none", "cross_encoder", "system_one"] = "none"
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

    # --- Code map (lha.agent.code_map) ------------------------------------------------------
    # "ripwire" runs `ripwire . --pack-task=<item> --token-budget=N` in the sandbox at the start of
    # each cycle and puts the task bundle in the lead's first message. Needs ripwire in the
    # sandbox (sandbox/Dockerfile has it); without it the cycle simply runs without a map.
    code_map: Literal["off", "ripwire"] = "off"
    code_map_token_budget: int = Field(default=2000, ge=200)
    code_map_timeout_s: float = Field(default=60.0, gt=0)  # also code_query's timeout
    # The read-only ``code_query`` tool (lha.execution.tools.code_query): every role can ask
    # ripwire find / definition / callers / uses / impact questions. Needs ripwire in the sandbox.
    code_query: bool = False
    code_query_token_budget: int = Field(default=1500, ge=200)

    # --- System One decision models (lha.systemone; docs/25-system-one.md) --------------
    # A model that answers typed questions with calibrated probabilities instead of text:
    # TypeSafe's hosted Jev, or a self-hosted server with the same API (Kev). LHA uses it only
    # to stop work earlier or reorder memory, never to allow an action or mark work done.
    # "off" (default) changes nothing; "stub" answers uniformly (tests).
    system_one_backend: Literal["off", "systemone", "stub"] = "off"
    # POST /v1/systemone URL: https for a remote host; http is allowed on loopback (Kev).
    system_one_endpoint: str = "https://api.typesafe.ai/v1/systemone"
    system_one_api_key: SecretStr | None = None
    # Pin a version, not "jev-latest": thresholds are tuned per model version.
    system_one_model: str = "jev-1.13.0"
    system_one_timeout_s: float = Field(default=5.0, gt=0)
    # USD per million input tokens (output is free). Unset: Jev's price for TypeSafe's host, $0
    # on loopback; any other endpoint must set it (or ``allow_unpriced_models``).
    system_one_price_in_per_mtok: float | None = Field(default=None, ge=0)
    # With ``private_data``, a non-loopback endpoint is refused unless this says the endpoint
    # may receive workspace text (task descriptions, redacted failure output, memory passages).
    system_one_private_data_ok: bool = False
    # Stall triage: after ``..._min_failures`` failures in a row, a confident "needs splitting"
    # splits the item now and a confident "environment problem" blocks it now.
    system_one_triage: bool = True
    system_one_triage_threshold: float = Field(default=0.9, ge=0, le=1)
    system_one_triage_min_failures: int = Field(default=2, ge=1)
    # ``memory_rerank=system_one``: drop passages whose relevance probability is below this.
    system_one_rerank_min: float = Field(default=0.0, ge=0, le=1)

    # --- Model resilience -------------------------------------------------------------
    # Ordered fallback chain tried on transient model errors, comma-separated
    # ``backend:model[@in/out]`` (USD per 1M tokens), e.g.
    # ``claude:claude-haiku-4-5,openai_compat:llama-3.3-70b@0.59/0.79,ollama:qwen3:8b``.
    fallback_models: str = ""
    fallback_max_rounds: int = Field(default=2, ge=1)
    # Client timeout of one Ollama / OpenAI-compatible model call (connect + read), in seconds.
    model_timeout_s: float = Field(default=120.0, gt=0)
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

    @model_validator(mode="after")
    def _claude_code_lead_uses_claude_code(self) -> Settings:
        """A CLI lead engine alone also routes the other roles through its CLI.

        ``lead_engine=claude_code`` / ``opencode`` sets ``model_backend`` to the matching backend,
        so the planner, replanner, reviewer and every other role run on the same engine. Only when
        ``model_backend`` was left at its ``stub`` default: an explicit backend wins.
        """
        if "model_backend" in self.model_fields_set:
            return self
        if self.lead_engine == "claude_code":
            self.model_backend = "claude_code"
        elif self.lead_engine == "opencode":
            self.model_backend = "opencode"
        return self

    def sandbox_egress_hosts(self) -> list[str]:
        """The Docker sandbox's egress allow-list from the three ``sandbox_egress*`` settings.

        Raises ``SandboxEgressError`` (a ``ValueError``) for a malformed entry, a non-package-fetch
        host in ``LHA_SANDBOX_EGRESS`` or a known write host in ``LHA_SANDBOX_EGRESS_EXTRA_HOSTS``.
        """
        from lha.execution.egress_hosts import sandbox_allow_list

        return sandbox_allow_list(
            _csv(self.sandbox_egress),
            _csv(self.sandbox_egress_extra_hosts),
            _csv(self.sandbox_egress_allow_write_hosts),
        )

    def sandbox_egress_enabled(self) -> bool:
        """True when a docker sandbox gets network through the egress proxy (any list set)."""
        return self.sandbox == "docker" and bool(
            _csv(self.sandbox_egress)
            or _csv(self.sandbox_egress_extra_hosts)
            or _csv(self.sandbox_egress_allow_write_hosts)
        )

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

    def reset_keep_paths(self) -> list[str]:
        """``reset_keep`` parsed; raises ``ValueError`` for an entry that could keep the work tree,
        the repository or the harness's anchor (``.``, ``*``, ``**``, ``..``, absolute paths,
        anything under ``.git`` or ``.lha``)."""
        entries = _csv(self.reset_keep)
        for entry in entries:
            parts = [p for p in entry.strip("/").split("/") if p]
            if (
                entry.startswith("/")
                or not parts
                or ".." in parts
                or parts[0] in (".", "*", "**", ".git", ".lha")
                or all(set(p) <= {"*", "."} for p in parts)
            ):
                raise ValueError(
                    f"LHA_RESET_KEEP entry {entry!r} is not allowed: list workspace-relative "
                    "ignored paths such as 'target' or '.cache', not the whole tree, .git or .lha"
                )
        return entries

    def worker_deployment_version(self) -> tuple[str, str] | None:
        """``(deployment, build_id)`` when worker versioning is on, else ``None``; raises
        ``ValueError`` when only one is set, the deployment name has a ``.``, or
        ``worker_promote`` is set without them."""
        name, build = self.worker_deployment.strip(), self.worker_build_id.strip()
        if not name and not build:
            if self.worker_promote:
                raise ValueError(
                    "LHA_WORKER_PROMOTE needs LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID"
                )
            return None
        if not (name and build):
            raise ValueError(
                "LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID must be set together "
                "(worker versioning)"
            )
        if "." in name:
            raise ValueError(f"LHA_WORKER_DEPLOYMENT {name!r} must not contain '.'")
        return name, build

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

    def trusted_check_env_names(self) -> list[str]:
        """``trusted_check_env`` parsed (raises ``ValueError`` for an invalid or ``LHA_*`` name)."""
        from lha.verify.trusted import validate_env_allow_list

        return validate_env_allow_list(_csv(self.trusted_check_env))

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


# --- secret rotation ---------------------------------------------------------------------
# A durable mission outlives its API keys. Every activity (and the health probe of a parked
# mission) re-reads the environment and ``.env`` and takes the SECRET fields that changed, so an
# operator rotates a key by updating them, never by restarting the worker. Non-secret settings
# are deliberately left as the worker started with them: a model or sandbox change mid-mission
# is a restart, not a rotation (docs/15).

#: The settings fields declared ``SecretStr``, in declaration order.
SECRET_FIELDS: tuple[str, ...] = tuple(
    name
    for name, field in Settings.model_fields.items()
    if field.annotation is not None and "SecretStr" in str(field.annotation)
)


def secret_fingerprint(value: SecretStr | None) -> str:
    """The first 12 hex digits of the SHA-256 of a secret (``""`` when unset): enough to tell two
    keys apart in a log or a report without showing either."""
    if value is None:
        return ""
    return hashlib.sha256(value.get_secret_value().encode("utf-8")).hexdigest()[:12]


def secret_fingerprints(settings: Settings) -> dict[str, str]:
    """``{field: fingerprint}`` for every secret that is set."""
    out: dict[str, str] = {}
    for name in SECRET_FIELDS:
        fp = secret_fingerprint(getattr(settings, name))
        if fp:
            out[name] = fp
    return out


def with_rotated_secrets(settings: Settings, fresh: Settings) -> tuple[Settings, list[str]]:
    """``settings`` with every secret that ``fresh`` SETS to a different value taken from
    ``fresh``, and the names of those fields (in declaration order). Nothing else changes: a
    secret absent from ``fresh`` is kept (removing a key is a restart, not a rotation, and a
    half-written ``.env`` must not unset anything)."""
    changed = [
        name
        for name in SECRET_FIELDS
        if getattr(fresh, name) is not None
        and secret_fingerprint(getattr(settings, name)) != secret_fingerprint(getattr(fresh, name))
    ]
    if not changed:
        return settings, []
    return settings.model_copy(update={n: getattr(fresh, n) for n in changed}), changed


def refresh_secrets(settings: Settings) -> tuple[Settings, list[str]]:
    """Re-read the environment and ``.env`` and apply the rotated secrets to ``settings``."""
    return with_rotated_secrets(settings, Settings())


def _csv(value: str) -> list[str]:
    return [part.strip() for part in value.split(",") if part.strip()]
