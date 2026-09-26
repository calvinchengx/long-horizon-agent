# Configuration

All runtime configuration is one `Settings` object in
[`python/src/lha/config.py`](../python/src/lha/config.py) (pydantic-settings). Application code
does not read `os.environ`; it calls `get_settings()`. The Go port reads a subset of the same names,
with the same defaults, in [`go/internal/config/config.go`](../go/internal/config/config.go) (see
[Go port coverage](#go-port-coverage)).

## Sources and precedence

Highest wins:

1. Process environment variables `LHA_<NAME>` (the name is matched case-insensitively).
2. A `.env` file in the **current working directory**.
3. The defaults below.

`.env` is looked up relative to where `lha` runs, not relative to the repository. Running
`cd python && uv run lha ...` reads `python/.env`; the repository-root `.env` (which
`docker compose` reads, and which [`.env.example`](../.env.example) is a template for) is not read
by `lha` in that case. Copy or symlink it, or export the variables.

`get_settings()` caches the object for the life of the process. A long-running `lha worker`
picks up changes only after a restart.

List settings (`LHA_WEB_ALLOW_HOSTS`, `LHA_WEB_ALLOW_PORTS`, `LHA_FALLBACK_MODELS`,
`LHA_SANDBOX_EGRESS`, `LHA_HARNESS_PATHS`) are comma-separated strings, not JSON; blank items are
dropped. The exception is `LHA_GATE_ESCALATION_SECONDS`, a JSON list (`[900, 2700]`).

Invalid values fail at startup with a pydantic validation error: an unknown `LHA_MODEL_BACKEND` or
`LHA_SANDBOX`, a non-numeric number, or a boolean other than `1/0`, `true/false`, `yes/no`,
`on/off` (and `t/f`, `y/n`).

## Settings

### Model

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_MODEL_BACKEND` | `stub` \| `ollama` \| `openai_compat` \| `claude` \| `claude_code` | `stub` (`claude_code` when `LHA_LEAD_ENGINE=claude_code`) | which backend `build_provider` constructs |
| `LHA_MODEL_NAME` | string | `stub-1` | model id sent to the backend |
| `LHA_OLLAMA_BASE_URL` | string | `http://localhost:11434` | Ollama server; `/v1` is appended for the model backend. `LHA_MEMORY_EMBEDDER=ollama` uses the same server (`/api/tags`, `/api/embed`) |
| `LHA_OPENAI_BASE_URL` | string | unset | chat-completions base URL; required for `openai_compat` |
| `LHA_OPENAI_API_KEY` | secret | unset | bearer token for `openai_compat` |
| `LHA_ANTHROPIC_API_KEY` | secret | unset | API key; required for `claude` |
| `LHA_OPENAI_PRICE_IN_PER_MTOK` | float | unset | USD per 1M input tokens (`openai_compat`); set both or neither |
| `LHA_OPENAI_PRICE_OUT_PER_MTOK` | float | unset | USD per 1M output tokens (`openai_compat`) |
| `LHA_CLAUDE_PRICE_IN_PER_MTOK` | float | unset | overrides the built-in price for `LHA_MODEL_NAME` (both must be set) |
| `LHA_CLAUDE_PRICE_OUT_PER_MTOK` | float | unset | as above |
| `LHA_ALLOW_UNPRICED_MODELS` | bool | `false` | let the governor run calls whose cost cannot be computed |
| `LHA_FALLBACK_MODELS` | comma-separated list | empty | ordered fallback chain of `backend:model[@in/out]` entries; non-empty makes `build_provider` return a `FailoverModel` |
| `LHA_FALLBACK_MAX_ROUNDS` | int (>= 1) | `2` | rounds over the whole chain before the last transient error is raised |
| `LHA_MODEL_PROBE_TIMEOUT_S` | float (> 0) | `10.0` | timeout of the model health probe a parked durable mission runs |
| `LHA_LEAD_ENGINE` | `loop` \| `claude_code` | `loop` | `claude_code` runs each lead cycle as one `claude -p` session |
| `LHA_CLAUDE_CODE_BIN` | string | `claude` | the Claude Code executable |
| `LHA_CLAUDE_CODE_TOOLS` | `lha` \| `native` | `lha` | the lead engine's tools: LHA's over MCP, or Claude Code's own (unsandboxed) |
| `LHA_CLAUDE_CODE_MAX_BUDGET_USD` | float (> 0) | `5.0` | `--max-budget-usd` for each `claude -p` call, and its worst case for the governor |
| `LHA_CLAUDE_CODE_TIMEOUT_S` | float (> 0) | `3600.0` | a `claude -p` call running longer is killed |

See [13-models.md](13-models.md).

### Durable control plane

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_TEMPORAL_ADDRESS` | string | `localhost:7233` | Temporal frontend `host:port` |
| `LHA_TEMPORAL_NAMESPACE` | string | `default` | Temporal namespace |
| `LHA_TASK_QUEUE` | string | `lha-mission` | queue the worker polls and `mission-start` targets |

### Persistence

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_POSTGRES_DSN` | secret | unset | Postgres DSN for `lha db migrate` and the mission store; unset: the SQLite store |
| `LHA_SQLITE_PATH` | string | unset | the SQLite mission store (WAL mode). Unset: one per-user file every process shares, `$XDG_DATA_HOME/lha/lha.sqlite3`, else `~/Library/Application Support/lha/lha.sqlite3` (macOS) or `~/.local/share/lha/lha.sqlite3`. A relative path resolves against the working directory, with a warning. If it would fall inside a mission's checkout it is moved to `<checkout>/.git/lha/`. `lha config` prints the resolved path |
| `LHA_POSTGRES_FALLBACK_TO_SQLITE` | bool | `true` | if `LHA_POSTGRES_DSN` is set but unusable (unreachable, not migrated, `psycopg` missing): `true` warns and uses SQLite, `false` fails the run |
| `LHA_WORKSPACE_ROOT` | string | `.lha/workspaces` | declared but not read by any code; each command's `--workdir` default is hard-coded |
| `LHA_OBJECT_STORE_ROOT` | string | `.lha/objects` | ClaimCheck blob directory (resolved to an absolute path); client, workers and replay must share it |

Every run path persists the mission row (status transitions), every metered model call (the cost
ledger), human gates (`hitl_gates`), episodic events, semantic memory and skills to the mission
store: SQLite when `LHA_POSTGRES_DSN` is unset, Postgres (after `lha db migrate`, which applies
`0005_hitl_gates` for the gate table) when it is set. The mission's checklist and progress still
live in git (the anchor). `lha missions`, `lha costs` and `lha gates` read the store. See [10-cost-and-budget.md](10-cost-and-budget.md#the-persistent-ledger).

`missions.status` as the run paths write it:

- local runs (`run-local`, `mission`, `orchestrate`): `RUNNING` at the start; at the end `DONE`
  (complete), `IMPOSSIBLE` (deadlocked) or `ABORTED` (budget, max cycles, loop, or an error);
- `mission-start`: `RUNNING` (`SLEEPING` with `--start-in-seconds`), with `workflow_id`, before
  it starts the workflow; `ABORTED` if the workflow cannot be started;
- each `run_agent_cycle` activity: `RUNNING` while it works, then from the committed checklist
  `DONE` (complete), `WAITING_ON_HUMAN` (the cycle queued an irreversible action for approval)
  or `RUNNING` (including an item split by the replanner, and a deadlocked checklist, whose
  outcome the workflow decides); `ABORTED` when the budget refuses a call;
- the workflow, through the `record_mission_status` activity: `SLEEPING`, `DEGRADED_PARK`,
  `WAITING_ON_HUMAN` when a gate opens, and the final status of every ending (`DONE`,
  `IMPOSSIBLE`, or `ABORTED` for a deadlock-gate abort, the cycle ceiling, the budget, a
  non-retryable failure or `lha mission-abort`). Best effort: 30 s per attempt, three attempts,
  then a logged warning; the mission never waits longer or fails because of it.

The store never moves a row from `DONE`, `IMPOSSIBLE` or `ABORTED` to a non-terminal status (the
other columns, such as `head_sha`, still update), so a cycle that was still finishing when the
mission was aborted cannot turn `ABORTED` back into `RUNNING`.

`lha mission-status` queries the live status from Temporal.

### Memory

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_MEMORY_ENABLED` | bool | `true` | tiered memory in the lead's prompt (all run paths) |
| `LHA_MEMORY_PROMPT_BUDGET_CHARS` | int | `4000` | cap on the memory block per cycle (characters, ~4 per token) |
| `LHA_MEMORY_EPISODIC_K` | int | `4` | past outcomes of the active item recalled per cycle |
| `LHA_MEMORY_SEMANTIC_K` | int | `4` | facts / progress / decisions / repo chunks recalled per cycle |
| `LHA_MEMORY_SKILLS_K` | int | `2` | verified skills recalled per cycle |
| `LHA_MEMORY_EMBEDDER` | `hash` \| `ollama` \| `sentence_transformers` \| `none` | `hash` | dense channel. `hash` is lexical and needs nothing; `ollama` is semantic, from the Ollama server at `LHA_OLLAMA_BASE_URL` ($0, no extra); `sentence_transformers` is semantic and needs the `embeddings` extra; `none` is lexical-only. `ollama` or `sentence_transformers` that cannot be used falls back to lexical-only. The default stays `hash` because `ollama` without a running server means no dense channel at all |
| `LHA_MEMORY_EMBEDDING_MODEL` | string | unset | the embedding model; unset: `nomic-embed-text` for `ollama`, `BAAI/bge-m3` for `sentence_transformers`. On Postgres a model narrower than 1024 is zero-padded to the `vector(1024)` column; a wider one runs lexical-only |
| `LHA_MEMORY_RERANK` | `none` \| `cross_encoder` | `none` | second-stage rerank; `cross_encoder` needs the `embeddings` extra |
| `LHA_MEMORY_CONSOLIDATE_EVERY` | int | `5` | consolidate episodes into facts every N recorded cycles; `0` disables |
| `LHA_MEMORY_CONSOLIDATION` | `extractive` \| `model` | `extractive` | `extractive` is deterministic and free; `model` asks the metered lead model |
| `LHA_MEMORY_INDEX_REPO_FILES` | bool | `true` | index the checkout's tracked text files for retrieval |
| `LHA_MEMORY_MAX_REPO_FILES` | int | `400` | cap on indexed files |

See [12-memory.md](12-memory.md).

### Governor

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_BUDGET_USD_CEILING` | float | `10.0` | spend ceiling in USD per mission (local runs; durable missions started by the CLI) |
| `LHA_MAX_CYCLES` | int | `1000` | cycle ceiling for local runs; `lha mission-start` passes it as `MissionInput.max_cycles` unless `--max-cycles` is given (read in the CLI process, not the worker) |
| `LHA_MAX_TURNS_PER_CYCLE` | int | `8` | model turns per cycle before the acting phase ends |
| `LHA_STALL_LIMIT` | int | `5` | local runs stop when one item fails this many times in a row; unused by the durable workflow |
| `LHA_MAX_REPLANS` | int | `20` | splits of blocked items allowed per mission (counted as items with status `split`); `0` disables the replanner |
| `LHA_MAX_SPLIT_DEPTH` | int | `2` | how deeply splits may nest: an item whose id already has this many dots (`03.1.2`) is not split again |
| `LHA_APPROVAL_TIMEOUT_S` | int | `86400` | durable missions: how long the workflow waits for a human to approve or reject an irreversible action before rejecting it; read by `lha mission-start`, whose `--approval-timeout-hours` overrides it per mission (`MissionInput.approval_timeout_seconds`). Local runs use `LHA_CONSOLE_APPROVAL_TIMEOUT_S` instead |

Durable sub-agent activities (`run_subagent`) build their own governor from
`LHA_BUDGET_USD_CEILING` and `LHA_MAX_CYCLES`. See [10-cost-and-budget.md](10-cost-and-budget.md).

### Execution sandbox

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_SANDBOX` | `docker` \| `e2b` \| `local` | `docker` | where tool calls and checks run; `--sandbox` overrides it for local commands |
| `LHA_ALLOW_UNSAFE_LOCAL` | bool | `false` | required for `local`, which runs commands on the host with no isolation |
| `LHA_SANDBOX_IMAGE` | string | `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` | Docker image the sandbox runs; it must contain the tools the checks and witnesses call ([`sandbox/Dockerfile`](../sandbox/Dockerfile) builds a Go + uv + Node/pnpm image) |
| `LHA_SANDBOX_MEMORY` | Docker memory size | `2g` | the Docker sandbox's memory limit (swap included); a Go build of a large dependency is killed at the default |
| `LHA_SANDBOX_CPUS` | float (> 0) | `2.0` | CPUs the Docker sandbox may use |
| `LHA_SANDBOX_TMP_SIZE` | Docker size | `1g` | size of the sandbox's `/tmp` tmpfs, which holds toolchain caches (Go modules and build cache, uv); it counts against `LHA_SANDBOX_MEMORY` |
| `LHA_SANDBOX_EGRESS` | comma-separated hosts | `""` | hosts the Docker sandbox may reach through the per-session egress proxy, for example `proxy.golang.org,sum.golang.org,storage.googleapis.com,pypi.org,files.pythonhosted.org`; `.example.org` allows the domain and subdomains, `host:port` another port; IP addresses are rejected. Empty: no network |
| `LHA_WEB_ALLOW_HOSTS` | comma-separated hosts | `""` | hosts the web tools may read; see [Web tools](#web-tools) |
| `LHA_TRUSTED_CHECKS` | JSON object | `""` | operator-defined checks run outside the sandbox, as `{"name": ["argv", ...]}`; items reference them as `trusted:<name>` witnesses. Malformed JSON or a non-list entry is a configuration error when a run starts |
| `LHA_HARNESS_PATHS` | comma-separated globs | `""` | workspace-relative paths the agent may not modify, on top of the test files always protected, for example `Makefile,e2e/**,.github/**` |
| `LHA_FLAKY_RETRIES` | int (0 to 5) | `1` | re-runs of a failing, not timed-out gating check on the same work tree; a check that then passes is quarantined (non-gating for the rest of the mission, with a committed `check_quarantined` event). `0` turns re-runs and quarantine off. See [07-verification.md](07-verification.md#flaky-check-quarantine) |

`LHA_SANDBOX_IMAGE` and `LHA_SANDBOX_EGRESS` apply to `docker` only. `e2b` authenticates through
the E2B SDK's own configuration (not an `LHA_*` variable). See
[09-safety-model.md](09-safety-model.md) and, for witnesses, trusted checks and protected paths,
[07-verification.md](07-verification.md).

Example for a Go project whose end-to-end suite needs Docker on the host:

```bash
export LHA_SANDBOX_IMAGE=lha-sandbox:latest
export LHA_SANDBOX_EGRESS=proxy.golang.org,sum.golang.org,storage.googleapis.com
export LHA_TRUSTED_CHECKS='{"e2e": ["make", "e2e"]}'
export LHA_HARNESS_PATHS='Makefile,e2e/**'
```

### Web tools

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_WEB_ALLOW_HOSTS` | comma-separated hosts | `""` | hosts `fetch_url` may reach; empty means the web tools are not registered. `--allow-host` adds to it per run. Not the sandbox's allow-list (`LHA_SANDBOX_EGRESS`) |
| `LHA_WEB_ALLOW_PORTS` | comma-separated ints | empty | ports allowed beyond 80/443; a non-integer entry refuses the run |
| `LHA_WEB_CREDENTIALS` | secret (JSON object) | unset | `{"<placeholder>": {"value": "<secret>", "hosts": ["<allow-listed host>", ...]}}`; brokered into `fetch_url` headers for the bound hosts only |
| `LHA_WEB_SEARCH_PROVIDER` | `tavily` \| `exa` | unset | registers `web_search` (with a key and a non-empty allow-list) |
| `LHA_WEB_SEARCH_API_KEY` | secret | unset | the search provider's API key |
| `LHA_WEB_SEARCH_ENDPOINT` | string | provider default | overrides `https://api.tavily.com/search` / `https://api.exa.ai/search` |
| `LHA_WEB_TIMEOUT_S` | float (> 0) | `30.0` | per-request timeout of the web tools |
| `LHA_WEB_MAX_RESPONSE_BYTES` | int (> 0) | `2000000` | response body cap of the web tools |
| `LHA_PRIVATE_DATA` | bool | `false` | declares that the workspace holds secrets or customer data; with web tools enabled the run is refused (Rule of Two) |

A run with a non-empty allow-list and either `LHA_SANDBOX=local` or `LHA_PRIVATE_DATA=true` is
refused before it starts. An invalid `LHA_WEB_CREDENTIALS` (bad JSON, a binding to a host
outside the allow-list), `LHA_WEB_ALLOW_PORTS` or `LHA_WEB_SEARCH_ENDPOINT` is refused the same
way. See [09-safety-model.md](09-safety-model.md#web-tools).

The web settings above other than `LHA_WEB_ALLOW_HOSTS` are read by the Python implementation
only; `go/internal/config` does not define them (see [Go port coverage](#go-port-coverage)).

### Multi-agent coordination

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_MAX_PARALLEL_IMPLEMENTERS` | int | `3` | `lha orchestrate` only (a durable mission takes `lha mission-start --max-parallel N` instead): the most checklist items one parallel wave runs at once, each by its own implementer in its own git worktree (items need disjoint write-sets assigned by the Planner); `1` (or less) disables parallel waves, so the Lead works every item serially |

See [11-multi-agent-organization.md](11-multi-agent-organization.md).

### Observability

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_OTEL_EXPORTER_OTLP_ENDPOINT` | URL | unset | OTLP/HTTP collector base URL (`/v1/traces` is appended). When unset, the standard `OTEL_EXPORTER_OTLP_ENDPOINT` is used |
| `OTEL_EXPORTER_OTLP_HEADERS` | string | unset | standard OpenTelemetry variable, read by the exporter: headers for the collector (for example an API key) |
| `OTEL_SDK_DISABLED` | bool | `false` | standard OpenTelemetry kill switch: `true` installs no exporter |
| `LHA_OTEL_SERVICE_NAME` | string | `lha` | the traces' `service.name` |
| `LHA_OTEL_EXPORT_TIMEOUT_S` | int | `5` | seconds one export may take before its spans are dropped |
| `LHA_LANGFUSE_HOST` | string | unset | Langfuse URL; with both keys set, spans also go to `<host>/api/public/otel` |
| `LHA_LANGFUSE_PUBLIC_KEY` | string | unset | Langfuse public key |
| `LHA_LANGFUSE_SECRET_KEY` | secret | unset | Langfuse secret key |

Export needs the `observability` extra; without it these settings only produce a warning at
start. See [16-observability.md](16-observability.md).

### Human gates and sleeping

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_CONSOLE_APPROVAL_TIMEOUT_S` | int >= 1 | `3600` | local runs with `--approve-interactive`: how long the terminal prompt waits before rejecting (durable missions use `LHA_APPROVAL_TIMEOUT_S` / `mission-start --approval-timeout-hours`) |
| `LHA_GATE_ESCALATION_SECONDS` | JSON list of ints | `[900, 2700, 14400, 43200]` | reminder offsets after a gate opens (the escalation ladder); offsets at or past the gate's timeout are skipped |
| `LHA_DEADLOCK_GATE_DEFAULT` | `abort` \| `impossible` | `abort` | the deadlock gate's decision on timeout (`retry` is refused) |
| `LHA_IMPOSSIBLE_AFTER_FAILURES` | int >= 1 | `3` | the deadlock gate recommends `impossible` after this many consecutive failed cycles on one item |
| `LHA_CYCLE_PAUSE_SECONDS` | int >= 0 | `0` | durable missions: pause between cycles (status `SLEEPING`) |
| `LHA_GATE_WEBHOOK_URL` | secret | unset | if set, every gate event (opened, reminder, resolved, defaulted) is POSTed there as JSON; off by default |
| `LHA_GATE_WEBHOOK_TIMEOUT_SECONDS` | float, 0 < x <= 60 | `5.0` | timeout of each webhook POST; a failed or slow POST never changes a gate's outcome |

`mission-start` reads `LHA_GATE_ESCALATION_SECONDS`, `LHA_DEADLOCK_GATE_DEFAULT`,
`LHA_IMPOSSIBLE_AFTER_FAILURES` and `LHA_CYCLE_PAUSE_SECONDS` and puts them in the workflow input,
so they are fixed per mission. `LHA_GATE_WEBHOOK_URL` and its timeout are read by the worker (the
`notify_gate` activity) and by local runs. See
[09-safety-model.md](09-safety-model.md#human-gates-on-tool-calls) and
[08-durable-execution.md](08-durable-execution.md#human-gates).

## Go port coverage

[`go/internal/config/config.go`](../go/internal/config/config.go) defines every Python setting,
with the same names, defaults and validation (`lha config` prints the same output). The Go CLI
uses the model, governor, sandbox, web-tool, trusted-check, harness and human-gate settings
(`LHA_CONSOLE_APPROVAL_TIMEOUT_S`, `LHA_GATE_ESCALATION_SECONDS`, `LHA_GATE_WEBHOOK_URL`,
`LHA_GATE_WEBHOOK_TIMEOUT_SECONDS`), the mission store (`LHA_SQLITE_PATH`, `LHA_POSTGRES_DSN`,
`LHA_POSTGRES_FALLBACK_TO_SQLITE`) and the `LHA_MEMORY_*` settings. Two memory values name
Python-only extras: `LHA_MEMORY_EMBEDDER=sentence_transformers` runs lexical-only retrieval and
`LHA_MEMORY_RERANK=cross_encoder` keeps fusion order in Go, as Python does when the `embeddings`
extra is not installed (`LHA_MEMORY_EMBEDDER=ollama` is the Go choice for real embeddings).
Settings of features that are Python-only are read but have no effect in a Go run: Temporal,
`LHA_FALLBACK_MODELS`, `LHA_FALLBACK_MAX_ROUNDS`,
`LHA_MODEL_PROBE_TIMEOUT_S`, `LHA_MAX_PARALLEL_IMPLEMENTERS`, `LHA_FLAKY_RETRIES`, the
`LHA_CLAUDE_CODE_*` settings and the `LHA_OTEL_*` settings (`LHA_LEAD_ENGINE=claude_code` and
`LHA_MODEL_BACKEND=claude_code` are refused). See
[04-choosing-an-implementation.md](04-choosing-an-implementation.md).

## Secrets

Seven settings are `SecretStr`: `LHA_OPENAI_API_KEY`, `LHA_ANTHROPIC_API_KEY`,
`LHA_POSTGRES_DSN`, `LHA_LANGFUSE_SECRET_KEY`, `LHA_WEB_CREDENTIALS`, `LHA_WEB_SEARCH_API_KEY` and
`LHA_GATE_WEBHOOK_URL` (chat webhook URLs embed their credential). Their `repr` never shows the
value; code unwraps them with
`get_secret_value()` only where the value is sent. `Settings.redacted()` masks every field whose
value is a `SecretStr`, so a new secret field is masked as long as it is declared `SecretStr`.

`lha config` prints `redacted()`:

```console
$ cd python && uv run lha config
model_backend = stub
model_name = stub-1
...
anthropic_api_key = ***      # set
postgres_dsn = None          # unset
...
```

Sandboxed child processes get a minimal allow-listed environment
([`execution/proc.py`](../python/src/lha/execution/proc.py)), not the host environment, so these
keys do not reach agent-run commands. Keep `.env` out of version control (`.gitignore` covers it).

## Other files and variables

| Variable / file | Read by | Meaning |
|---|---|---|
| `LHA_IT_POSTGRES_DSN` | `tests/integration/conftest.py` | admin DSN for a Postgres with `vector` available; each test creates and drops its own database. Unset: Postgres tests are skipped |
| `LHA_IT_DOCKER` | `tests/integration/conftest.py` | `1` runs the Docker sandbox tests against the local daemon (they pull `python:3.12-slim`, `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` and `python:3.12-alpine`, and the egress tests need internet access). Otherwise skipped |
| `LHA_IT_SANDBOX_IMAGE` | `tests/integration/test_large_mission_e2e.py` | the polyglot sandbox image built from `sandbox/Dockerfile` (default `lha-sandbox:dev`) |
| `LHA_RECORD_HISTORY` | `tests/durability/test_replay.py` | `1` rewrites the committed replay histories `mission_three_items.json`, `mission_approval_ladder.json`, `mission_row_gate_retry.json` and `mission_cancel_mid_cycle.json` in `tests/durability/histories/`; select one with `-k` so the others keep replaying |
| `LHA_APPDB_PASSWORD` | `docker-compose.yml` | `appdb` password (default `lha`) |
| `LANGFUSE_NEXTAUTH_SECRET`, `LANGFUSE_SALT` | `docker-compose.yml` | required by the Langfuse service; compose refuses to start without them |

The test variables are read with `os.environ` directly and are not `Settings` fields. See
[20-testing.md](20-testing.md).

## Settings the Dockerfile sets

[`python/Dockerfile`](../python/Dockerfile) sets `LHA_WORKSPACE_ROOT=/home/lha/workspaces`,
`LHA_OBJECT_STORE_ROOT=/home/lha/objects` and `GIT_TERMINAL_PROMPT=0`, and runs `lha worker` as
uid 10001.
