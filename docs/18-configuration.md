# Configuration

All runtime configuration is one `Settings` object in
[`python/src/lha/config.py`](../python/src/lha/config.py) (pydantic-settings). Application code
does not read `os.environ`; it calls `get_settings()`. The Go port reads the same names with the
same defaults in [`go/internal/config/config.go`](../go/internal/config/config.go).

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
dropped.

Invalid values fail at startup with a pydantic validation error: an unknown `LHA_MODEL_BACKEND` or
`LHA_SANDBOX`, a non-numeric number, or a boolean other than `1/0`, `true/false`, `yes/no`,
`on/off` (and `t/f`, `y/n`).

## Settings

### Model

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_MODEL_BACKEND` | `stub` \| `ollama` \| `openai_compat` \| `claude` | `stub` | which backend `build_provider` constructs |
| `LHA_MODEL_NAME` | string | `stub-1` | model id sent to the backend |
| `LHA_OLLAMA_BASE_URL` | string | `http://localhost:11434` | Ollama server; `/v1` is appended |
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
| `LHA_SQLITE_PATH` | string | `.lha/lha.sqlite3` | the SQLite mission store (WAL mode), relative to the working directory; if it would fall inside a mission's checkout it is moved to `<checkout>/.git/lha/` |
| `LHA_POSTGRES_FALLBACK_TO_SQLITE` | bool | `true` | if `LHA_POSTGRES_DSN` is set but unusable (unreachable, not migrated, `psycopg` missing): `true` warns and uses SQLite, `false` fails the run |
| `LHA_WORKSPACE_ROOT` | string | `.lha/workspaces` | declared but not read by any code; each command's `--workdir` default is hard-coded |
| `LHA_OBJECT_STORE_ROOT` | string | `.lha/objects` | ClaimCheck blob directory (resolved to an absolute path); client, workers and replay must share it |

Every run path persists the mission row (status transitions), every metered model call (the cost
ledger), episodic events, semantic memory and skills to the mission store: SQLite when
`LHA_POSTGRES_DSN` is unset, Postgres (after `lha db migrate`) when it is set. The mission's
checklist and progress still live in git (the anchor). `lha missions` and `lha costs` read the
store. See [10-cost-and-budget.md](10-cost-and-budget.md#the-persistent-ledger).

`missions.status` as the run paths write it:

- local runs (`run-local`, `mission`, `orchestrate`): `RUNNING` at the start; at the end `DONE`
  (complete), `IMPOSSIBLE` (deadlocked) or `ABORTED` (budget, max cycles, loop, or an error);
- `mission-start`: `RUNNING`, with `workflow_id`;
- each `run_agent_cycle` activity: `RUNNING` while it works, then from the committed checklist
  `DONE` (complete), `WAITING_ON_HUMAN` (the cycle queued an irreversible action for approval),
  `IMPOSSIBLE` (deadlocked) or `RUNNING` (including an item split by the replanner); `ABORTED`
  when the budget refuses a call. States only the workflow knows are not written:
  `DEGRADED_PARK`, and the end of a mission by the cycle ceiling or `lha mission-abort` (the row
  keeps the last cycle's status). `lha mission-status` queries the live status from Temporal.

### Memory

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_MEMORY_ENABLED` | bool | `true` | tiered memory in the lead's prompt (all run paths) |
| `LHA_MEMORY_PROMPT_BUDGET_CHARS` | int | `4000` | cap on the memory block per cycle (characters, ~4 per token) |
| `LHA_MEMORY_EPISODIC_K` | int | `4` | past outcomes of the active item recalled per cycle |
| `LHA_MEMORY_SEMANTIC_K` | int | `4` | facts / progress / decisions / repo chunks recalled per cycle |
| `LHA_MEMORY_SKILLS_K` | int | `2` | verified skills recalled per cycle |
| `LHA_MEMORY_EMBEDDER` | `hash` \| `sentence_transformers` \| `none` | `hash` | dense channel; `sentence_transformers` needs the `embeddings` extra (lexical-only without it), `none` is lexical-only |
| `LHA_MEMORY_EMBEDDING_MODEL` | string | `BAAI/bge-m3` | model for `sentence_transformers` (1024-wide to use pgvector) |
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
| `LHA_APPROVAL_TIMEOUT_S` | int | `86400` | how long a durable mission waits for a human to approve or reject an irreversible action before rejecting it; `lha mission-start --approval-timeout-hours` overrides it per mission (`MissionInput.approval_timeout_seconds`) |

Durable sub-agent activities (`run_subagent`) build their own governor from
`LHA_BUDGET_USD_CEILING` and `LHA_MAX_CYCLES`. See [10-cost-and-budget.md](10-cost-and-budget.md).

### Execution sandbox

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_SANDBOX` | `docker` \| `e2b` \| `local` | `docker` | where tool calls and checks run; `--sandbox` overrides it for local commands |
| `LHA_ALLOW_UNSAFE_LOCAL` | bool | `false` | required for `local`, which runs commands on the host with no isolation |
| `LHA_SANDBOX_IMAGE` | string | `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` | Docker image the sandbox runs; it must contain the tools the checks and witnesses call ([`sandbox/Dockerfile`](../sandbox/Dockerfile) builds a Go + uv + Node/pnpm image) |
| `LHA_SANDBOX_EGRESS` | comma-separated hosts | `""` | hosts the Docker sandbox may reach through the per-session egress proxy, for example `proxy.golang.org,sum.golang.org,pypi.org,files.pythonhosted.org`; `.example.org` allows the domain and subdomains, `host:port` another port; IP addresses are rejected. Empty: no network |
| `LHA_WEB_ALLOW_HOSTS` | comma-separated hosts | `""` | hosts the web tools may read; see [Web tools](#web-tools) |
| `LHA_TRUSTED_CHECKS` | JSON object | `""` | operator-defined checks run outside the sandbox, as `{"name": ["argv", ...]}`; items reference them as `trusted:<name>` witnesses. Malformed JSON or a non-list entry is a configuration error when a run starts |
| `LHA_HARNESS_PATHS` | comma-separated globs | `""` | workspace-relative paths the agent may not modify, on top of the test files always protected, for example `Makefile,e2e/**,.github/**` |

`LHA_SANDBOX_IMAGE` and `LHA_SANDBOX_EGRESS` apply to `docker` only. `e2b` authenticates through
the E2B SDK's own configuration (not an `LHA_*` variable). See
[09-safety-model.md](09-safety-model.md) and, for witnesses, trusted checks and protected paths,
[07-verification.md](07-verification.md).

Example for a Go project whose end-to-end suite needs Docker on the host:

```bash
export LHA_SANDBOX_IMAGE=lha-sandbox:latest
export LHA_SANDBOX_EGRESS=proxy.golang.org,sum.golang.org
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

The web settings above other than `LHA_WEB_ALLOW_HOSTS`, and `LHA_FALLBACK_MODELS`,
`LHA_FALLBACK_MAX_ROUNDS` and `LHA_MODEL_PROBE_TIMEOUT_S`, are read by the Python implementation
only; `go/internal/config` does not define them.

### Observability

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_LANGFUSE_HOST` | string | unset | Langfuse URL for `build_langfuse()` |
| `LHA_LANGFUSE_PUBLIC_KEY` | string | unset | Langfuse public key |
| `LHA_LANGFUSE_SECRET_KEY` | secret | unset | Langfuse secret key |

`build_langfuse()` has no caller; setting these has no effect on a run today. See
[16-observability.md](16-observability.md).

## Secrets

Six settings are `SecretStr`: `LHA_OPENAI_API_KEY`, `LHA_ANTHROPIC_API_KEY`, `LHA_POSTGRES_DSN`,
`LHA_LANGFUSE_SECRET_KEY`, `LHA_WEB_CREDENTIALS` and `LHA_WEB_SEARCH_API_KEY`. Their `repr` never
shows the value; code unwraps them with
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
| `LHA_RECORD_HISTORY` | `tests/durability/test_replay.py` | `1` rewrites the committed replay history `tests/durability/histories/mission_three_items.json` |
| `LHA_PYDIFF` | `go/internal/safety/zz_pydiff_test.go` | directory of Python egress dumps for the Go differential tests; unset: skipped |
| `LHA_APPDB_PASSWORD` | `docker-compose.yml` | `appdb` password (default `lha`) |
| `LANGFUSE_NEXTAUTH_SECRET`, `LANGFUSE_SALT` | `docker-compose.yml` | required by the Langfuse service; compose refuses to start without them |

The test variables are read with `os.environ` directly and are not `Settings` fields. See
[20-testing.md](20-testing.md).

## Settings the Dockerfile sets

[`python/Dockerfile`](../python/Dockerfile) sets `LHA_WORKSPACE_ROOT=/home/lha/workspaces`,
`LHA_OBJECT_STORE_ROOT=/home/lha/objects` and `GIT_TERMINAL_PROMPT=0`, and runs `lha worker` as
uid 10001.
