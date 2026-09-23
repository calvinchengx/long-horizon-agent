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
| `LHA_POSTGRES_DSN` | secret | unset | Postgres DSN; used only by `lha db migrate` |
| `LHA_WORKSPACE_ROOT` | string | `.lha/workspaces` | declared but not read by any code; each command's `--workdir` default is hard-coded |
| `LHA_OBJECT_STORE_ROOT` | string | `.lha/objects` | ClaimCheck blob directory (resolved to an absolute path); client, workers and replay must share it |

The config comment says local SQLite is used when `LHA_POSTGRES_DSN` is unset; there is no SQLite
code. Without Postgres, state lives in git (the anchor) and the filesystem object store.

### Governor

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_BUDGET_USD_CEILING` | float | `10.0` | spend ceiling in USD per mission (local runs; durable missions started by the CLI) |
| `LHA_MAX_CYCLES` | int | `1000` | cycle ceiling for local runs; not passed to durable missions (they use `MissionInput.max_cycles`, default 1000) |
| `LHA_MAX_TURNS_PER_CYCLE` | int | `8` | model turns per cycle before the acting phase ends |
| `LHA_STALL_LIMIT` | int | `5` | local runs stop when one item fails this many times in a row; unused by the durable workflow |

Durable sub-agent activities (`run_subagent`) build their own governor from
`LHA_BUDGET_USD_CEILING` and `LHA_MAX_CYCLES`. See [10-cost-and-budget.md](10-cost-and-budget.md).

### Execution sandbox

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_SANDBOX` | `docker` \| `e2b` \| `local` | `docker` | where tool calls and checks run; `--sandbox` overrides it for local commands |
| `LHA_ALLOW_UNSAFE_LOCAL` | bool | `false` | required for `local`, which runs commands on the host with no isolation |

`e2b` authenticates through the E2B SDK's own configuration (not an `LHA_*` variable). See
[09-safety-model.md](09-safety-model.md).

### Observability

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_LANGFUSE_HOST` | string | unset | Langfuse URL for `build_langfuse()` |
| `LHA_LANGFUSE_PUBLIC_KEY` | string | unset | Langfuse public key |
| `LHA_LANGFUSE_SECRET_KEY` | secret | unset | Langfuse secret key |

`build_langfuse()` has no caller; setting these has no effect on a run today. See
[16-observability.md](16-observability.md).

### Human gates and sleeping

| Variable | Type | Default | Meaning |
|---|---|---|---|
| `LHA_APPROVAL_TIMEOUT_SECONDS` | int >= 1 | `3600` | local runs with `--approve-interactive`: how long the terminal prompt waits before rejecting (durable missions use `mission-start --approval-timeout-hours`) |
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

## Secrets

These settings are `SecretStr`: `LHA_OPENAI_API_KEY`, `LHA_ANTHROPIC_API_KEY`, `LHA_POSTGRES_DSN`,
`LHA_LANGFUSE_SECRET_KEY` and `LHA_GATE_WEBHOOK_URL` (chat webhook URLs embed their credential). Their `repr` never shows the value; code unwraps them with
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
| `LHA_IT_DOCKER` | `tests/integration/conftest.py` | `1` runs the Docker sandbox tests against the local daemon (pulls `python:3.12-slim`). Otherwise skipped |
| `LHA_RECORD_HISTORY` | `tests/durability/test_replay.py` | `1` rewrites the committed replay histories `mission_three_items.json` and `mission_approval_ladder.json` in `tests/durability/histories/`; select one with `-k` so the others keep replaying |
| `LHA_PYDIFF` | `go/internal/safety/zz_pydiff_test.go` | directory of Python egress dumps for the Go differential tests; unset: skipped |
| `LHA_APPDB_PASSWORD` | `docker-compose.yml` | `appdb` password (default `lha`) |
| `LANGFUSE_NEXTAUTH_SECRET`, `LANGFUSE_SALT` | `docker-compose.yml` | required by the Langfuse service; compose refuses to start without them |

The test variables are read with `os.environ` directly and are not `Settings` fields. See
[20-testing.md](20-testing.md).

## Settings the Dockerfile sets

[`python/Dockerfile`](../python/Dockerfile) sets `LHA_WORKSPACE_ROOT=/home/lha/workspaces`,
`LHA_OBJECT_STORE_ROOT=/home/lha/objects` and `GIT_TERMINAL_PROMPT=0`, and runs `lha worker` as
uid 10001.
