# CLI reference

The `lha` command is defined in [`python/src/lha/cli/main.py`](../python/src/lha/cli/main.py)
(Typer). Run it from `python/` with `uv run lha <command>`, or install the package and run `lha`.
`lha` with no arguments prints help. Every command also reads the `LHA_*` settings described in
[18-configuration.md](18-configuration.md).

The Go implementation mirrors this surface (same command names, options and settings) as it is
ported. Today `go/cmd/lha` is empty and no Go binary can be built; see [23-roadmap.md](23-roadmap.md).

## Commands

| Command | Purpose | Needs |
|---|---|---|
| [`version`](#lha-version) | print the version | nothing |
| [`config`](#lha-config) | print resolved settings, secrets masked | nothing |
| [`db migrate`](#lha-db-migrate) | apply SQL migrations | Postgres, `postgres` extra |
| [`run-local`](#lha-run-local) | run a given checklist locally | a sandbox |
| [`mission`](#lha-mission) | plan a task, then run it locally | a sandbox |
| [`orchestrate`](#lha-orchestrate) | plan, then run the multi-agent org locally | a sandbox |
| [`worker`](#lha-worker) | serve durable missions | Temporal |
| [`mission-start`](#lha-mission-start) | plan and start a durable mission | Temporal, a worker |
| [`mission-status`](#lha-mission-status) | query status and cycle count | Temporal |
| [`mission-approve`](#lha-mission-approve) | send a gate decision | Temporal |
| [`mission-abort`](#lha-mission-abort) | cancel a durable mission | Temporal |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success; for the local mission commands, every item verified done |
| `1` | a local mission ended without completing (deadlocked, stopped by the governor, loop, `max_cycles`); or an unhandled error (Python traceback) |
| `2` | usage error, or a handled operator error printed as `error: ...` on stderr (bad `--check`, unknown `--sandbox`, unsafe `local` sandbox, missing optional module, missing `LHA_POSTGRES_DSN`) |
| `3` | the budget governor refused the planning call (`mission`, `orchestrate`, `mission-start`) |

Once a local run is under way, a governor refusal (before a cycle or before a single model call)
stops it normally: the summary is printed with `stopped_reason` `governor: ...` and the exit code
is `1`. Only a refusal during planning, before anything has run, exits `3`. The Temporal commands
(`worker`, `mission-status`, `mission-approve`, `mission-abort`) do not translate errors: an
unreachable server or unknown workflow id ends in a traceback with exit `1`.

## Options shared by the mission commands

`run-local`, `mission`, `orchestrate` and `mission-start` share these:

| Option | Default | Meaning |
|---|---|---|
| `--check TEXT` | none | a gating verification command, shell-quoted (split with `shlex`); repeatable; added to the default checks |
| `--no-default-checks` | off | drop the default checks; requires at least one non-empty `--check` |
| `--sandbox TEXT` | `LHA_SANDBOX`, else `docker` | `docker`, `e2b` or `local` (not on `mission-start`) |
| `--unsafe-local` | off | allow the `local` sandbox (no isolation) (not on `mission-start`) |

The default checks are `uv run ruff check .`, `uv run ty check` and `uv run pytest -q`, run inside
the sandbox. An item is never marked done without at least one passing gating check. The default
Docker image, `python:3.12-slim`, does not include `uv`; with the Docker sandbox, pass
`--no-default-checks` and `--check` commands that the image can run.

Choosing `local` without `--unsafe-local` (or `LHA_ALLOW_UNSAFE_LOCAL=true`) fails before any
model call or workspace write. `e2b` requires the `e2b-code-interpreter` package and an E2B
account; `docker` requires the `sandbox` extra and a Docker daemon.

## `lha version`

Prints `lha <version>` (currently `lha 0.1.0`). No options.

## `lha config`

Prints every setting as `name = value`, one per line, in declaration order. `SecretStr` settings
(`openai_api_key`, `anthropic_api_key`, `postgres_dsn`, `langfuse_secret_key`) print `***` when
set and `None` when unset. No options.

## `lha db migrate`

Applies pending migrations to the database at `LHA_POSTGRES_DSN`.

| Option | Default | Meaning |
|---|---|---|
| `--migrations-dir TEXT` | `db/migrations` | directory of `*.sql` files, relative to the current directory |

The default is relative to the repository root. From `python/`, pass
`--migrations-dir ../db/migrations`. Prints `migrations applied: [...]` with the versions applied by
this run (`[]` when up to date). Exits `2` if `LHA_POSTGRES_DSN` is unset or `psycopg` is missing
(`uv sync --extra postgres`). Details in [19-wire-contract.md](19-wire-contract.md#postgres-schema).

## `lha run-local`

Runs a mission from an explicit checklist, in this process, without Temporal.

| Option | Default | Meaning |
|---|---|---|
| `--title TEXT` | required | mission title |
| `--item TEXT` | required, repeatable | one checklist item; ids are `01`, `02`, ... in order |
| `--workdir TEXT` | `.lha/workspaces/local` | workspace; initialized as a git repo |
| `--description TEXT` | `""` | mission description |

Plus the shared options. It initializes the anchor, then runs cycles until the checklist is
complete, deadlocked, refused by the governor, a loop is detected, or `LHA_MAX_CYCLES` is reached.
It prints:

```
mission <mission_id>: <stopped_reason>
items <done>/<total>  cycles <n>  cost $<known spend>
head <sha or (none)>
```

## `lha mission`

Plans `--task` into a checklist with the configured model, then runs it as `run-local` does. The
planner and the lead share one budget.

| Option | Default | Meaning |
|---|---|---|
| `--task TEXT` | required | mission description; the Planner decomposes it |
| `--title TEXT` | `mission` | mission title |
| `--workdir TEXT` | `.lha/workspaces/mission` | workspace |

If the planner's reply cannot be parsed, the plan falls back to one item built from the
description.

## `lha orchestrate`

Plans, then runs the multi-agent organization locally: per item, read-only researchers, the Lead
Engineer loop, reflection on failure, and an independent reviewer that can reopen an item (see
[11-multi-agent-organization.md](11-multi-agent-organization.md)). With the `claude` backend each
role uses its tier's model ([13-models.md](13-models.md#per-role-routing-lha-orchestrate)).

| Option | Default | Meaning |
|---|---|---|
| `--task TEXT` | required | mission description |
| `--title TEXT` | `mission` | mission title |
| `--workdir TEXT` | `.lha/workspaces/org` | workspace |

Output and exit codes as for `run-local`.

## `lha worker`

Connects to `LHA_TEMPORAL_ADDRESS` / `LHA_TEMPORAL_NAMESPACE` and serves `MissionWorkflow` and
`SubAgentWorkflow` on `LHA_TASK_QUEUE` until interrupted. No options. The model, sandbox and
budget used by durable missions come from this process's settings. See
[14-running-on-temporal.md](14-running-on-temporal.md).

## `lha mission-start`

Plans the task, initializes the anchor at `--workdir`, and starts `MissionWorkflow` with workflow
id `mission:<mission_id>`. Prints `started mission <mission_id> (workflow id:
mission:<mission_id>)` and returns without waiting.

| Option | Default | Meaning |
|---|---|---|
| `--task TEXT` | required | mission description |
| `--title TEXT` | `mission` | mission title |
| `--workdir TEXT` | `.lha/workspaces/durable` | workspace; passed to the worker as given, so use an absolute path |

Plus `--check` and `--no-default-checks`. The check commands travel in the workflow input; the
defaults are resolved by this command, not by the worker.

## `lha mission-status`

```
lha mission-status MISSION_ID
```

Queries `status_v1` and `cycles_done` on workflow `mission:MISSION_ID` and prints
`status=<status> cycles=<n>`. Works while running and after the workflow has closed; a worker
must be running to answer the queries.

## `lha mission-approve`

```
lha mission-approve MISSION_ID [--decision TEXT]
```

| Option | Default | Meaning |
|---|---|---|
| `--decision TEXT` | `approve` | the decision string to signal |

Sends signal `human_decision_v1` and prints `sent decision '<decision>' to mission <id>`. The only
gate in `MissionWorkflow` is the deadlock gate, whose options are `retry` and `abort`; other
values, including the default `approve`, are recorded as rejected and ignored. Missions started by
`mission-start` never open that gate. See [15-operations-runbook.md](15-operations-runbook.md).

## `lha mission-abort`

```
lha mission-abort MISSION_ID
```

Requests cancellation of workflow `mission:MISSION_ID` and prints `cancelled mission <id>`. The
workflow closes as Cancelled; the interrupted cycle is not committed.
