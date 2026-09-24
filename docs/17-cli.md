# CLI reference

The `lha` command is defined in [`python/src/lha/cli/main.py`](../python/src/lha/cli/main.py)
(Typer). Run it from `python/` with `uv run lha <command>`, or install the package and run `lha`.
`lha` with no arguments prints help. Every command also reads the `LHA_*` settings described in
[18-configuration.md](18-configuration.md).

The Go implementation mirrors this surface (same command names, options and settings) as it is
ported. Today there is no `go/cmd/lha` and no Go binary can be built; see [23-roadmap.md](23-roadmap.md).

## Commands

| Command | Purpose | Needs |
|---|---|---|
| [`version`](#lha-version) | print the version | nothing |
| [`config`](#lha-config) | print resolved settings, secrets masked | nothing |
| [`db migrate`](#lha-db-migrate) | apply SQL migrations | Postgres, `postgres` extra |
| [`vendor`](#lha-vendor) | snapshot reference pages into the workspace | network access to the URLs |
| [`run-local`](#lha-run-local) | run a given or imported checklist locally | a sandbox |
| [`mission`](#lha-mission) | plan a task (or import a checklist), then run it locally | a sandbox |
| [`orchestrate`](#lha-orchestrate) | plan, then run the multi-agent org locally | a sandbox |
| [`decisions`](#lha-decisions) | print or verify a mission's decision log | a mission workspace |
| [`missions`](#lha-missions) | list persisted missions with status and recorded spend | the mission store |
| [`costs`](#lha-costs) | print a mission's persisted cost ledger | the mission store |
| [`gates`](#lha-gates) | list recorded human gates: question, options, reminders, decision, who and when | the mission store |
| [`worker`](#lha-worker) | serve durable missions | Temporal |
| [`mission-start`](#lha-mission-start) | plan (or import) and start a durable mission | Temporal, a worker |
| [`mission-status`](#lha-mission-status) | query status, cycles, open gate, sleep and recent gate events | Temporal |
| [`mission-approve`](#lha-mission-approve) | answer the open gate (a queued irreversible action, or the deadlock gate) | Temporal |
| [`mission-snooze`](#lha-mission-snooze) | sleep a mission before its next cycle, or wake it | Temporal |
| [`mission-abort`](#lha-mission-abort) | cancel a durable mission | Temporal |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success; for the local mission commands, every item verified done |
| `1` | a local mission ended without completing (deadlocked, stopped by the governor, loop, `max_cycles`, decision log failed verification); `decisions` found a broken chain; `costs` found no ledger rows; or an unhandled error (Python traceback) |
| `2` | usage error, or a handled operator error printed as `error: ...` on stderr (bad `--check`, unknown `--sandbox`, unsafe `local` sandbox, missing optional module, missing `LHA_POSTGRES_DSN`, an unusable Postgres store with `LHA_POSTGRES_FALLBACK_TO_SQLITE=false`, a Rule-of-Two violation or invalid web settings, an invalid `--checklist` file, neither or both of `--item`/`--checklist`, an unknown `--decision` or one the open gate does not offer, a refused `vendor` URL) |
| `3` | the budget governor refused the planning call (`mission`, `orchestrate`, `mission-start`) |

Once a local run is under way, a governor refusal (before a cycle or before a single model call)
stops it normally: the summary is printed with `stopped_reason` `governor: ...` and the exit code
is `1`. Only a refusal during planning, before anything has run, exits `3`. The Temporal commands
(`worker`, `mission-status`, `mission-approve`, `mission-snooze`, `mission-abort`) do not
translate errors: an unreachable server or unknown workflow id ends in a traceback with exit `1`.
`mission-approve` exits `2` when the decision is unknown or not offered by the open gate.

## Options shared by the mission commands

`run-local`, `mission`, `orchestrate` and `mission-start` share these:

| Option | Default | Meaning |
|---|---|---|
| `--check TEXT` | none | a gating verification command, shell-quoted (split with `shlex`); repeatable; added to the default checks |
| `--no-default-checks` | off | drop the default checks; requires at least one non-empty `--check` |
| `--sandbox TEXT` | `LHA_SANDBOX`, else `docker` | `docker`, `e2b` or `local` (not on `mission-start`) |
| `--unsafe-local` | off | allow the `local` sandbox (no isolation) (not on `mission-start`) |
| `--reference TEXT` | none | a workspace-relative path of vendored reference material, recited to the agent every cycle; repeatable (see [`vendor`](#lha-vendor)) |
| `--approve-interactive` | off | ask on the terminal before an irreversible command (not on `mission-start`; see below) |
| `--allow-host TEXT` | none | add a host to this run's web allow-list, on top of `LHA_WEB_ALLOW_HOSTS`; repeatable. A non-empty allow-list registers the web tools (`fetch_url`, and `web_search` when configured) (not on `mission-start`, where the worker's settings apply) |

`run-local`, `mission` and `mission-start` also take `--checklist FILE`: seed the mission with a
`.json` checklist or a `.md` roadmap instead of planning (format in
[06-mission-anchor.md](06-mission-anchor.md#importing-a-checklist)). The file's title,
description and references are used unless given on the command line; `--reference` paths are
merged with the file's.

Without `--approve-interactive`, a command the classifier flags (`git push`, publishing, uploads,
...) is refused in a local run. With it, the run prints the tool, the exact argv, the classifier's
reason and the timeout, and asks `Allow this exact call? [y/N]`. Only `y`/`yes` approves; any other
answer, end of input, or no answer within `LHA_CONSOLE_APPROVAL_TIMEOUT_S` (default 3600) rejects,
with reminders at `LHA_GATE_ESCALATION_SECONDS` first. When stdin is not a TTY the call is rejected
without asking. Each answer is committed to the anchor as a `tool_approval` event.

Before any planning call or workspace write, the local commands check the run's web settings and
the Rule of Two ([09-safety-model.md](09-safety-model.md#5-rule-of-two)): web tools together with
the `local` sandbox or `LHA_PRIVATE_DATA=true` exit `2`.

The default checks are `uv run ruff check .`, `uv run ty check` and `uv run pytest -q`, run inside
the sandbox. An item is never marked done without at least one passing gating check, and an
item's witnesses must pass too. The default Docker image
(`ghcr.io/astral-sh/uv:python3.12-bookworm-slim`) has `uv` but no ruff, ty or pytest, and no
network unless `LHA_SANDBOX_EGRESS` allows the package index; choose checks the image can run, or
set `LHA_SANDBOX_IMAGE` to an image that has them.

Choosing `local` without `--unsafe-local` (or `LHA_ALLOW_UNSAFE_LOCAL=true`) fails before any
model call or workspace write. `e2b` requires the `e2b-code-interpreter` package and an E2B
account; `docker` requires the `sandbox` extra and a Docker daemon.

## `lha version`

Prints `lha <version>` (currently `lha 0.1.0`). No options.

## `lha config`

Prints every setting as `name = value`, one per line, in declaration order. `SecretStr` settings
(`openai_api_key`, `anthropic_api_key`, `postgres_dsn`, `langfuse_secret_key`,
`web_credentials`, `web_search_api_key`, `gate_webhook_url`) print `***` when set and `None` when
unset. The last line, `mission store = ...`, is where `lha missions` and `lha costs` read:
`sqlite <absolute path>` (the resolved `LHA_SQLITE_PATH` or the per-user default), or
`postgres (LHA_POSTGRES_DSN)` with its SQLite fallback. No options.

## `lha db migrate`

Applies pending migrations to the database at `LHA_POSTGRES_DSN`.

| Option | Default | Meaning |
|---|---|---|
| `--migrations-dir TEXT` | `db/migrations`, else `../db/migrations` | directory of `*.sql` files |

Without the option, the command uses `db/migrations` in the current directory if it exists,
otherwise `../db/migrations`, so it works from the repository root and from `python/`; if neither
exists it exits `2`. Prints `migrations applied: [...]` with the versions applied by
this run (`[]` when up to date). Exits `2` if `LHA_POSTGRES_DSN` is unset or `psycopg` is missing
(`uv sync --extra postgres`). Details in [19-wire-contract.md](19-wire-contract.md#postgres-schema).

## `lha vendor`

```
lha vendor URL... [--into DIR]
```

Downloads each URL into `DIR` (default `reference`, relative to the current directory) and
writes `DIR/MANIFEST.json` with each file's URL, path, SHA-256, size, content type and fetch time
(a re-vendored URL replaces its entry). Files are stored under `<host>/<path>`; HTML pages also
get a `.txt` rendering. It prints `<url> -> <path> (<bytes> bytes, sha256 <prefix>)` per file,
then the manifest path.

Fetching uses the egress policy: http(s) only, no credentials in the URL, public addresses only,
at most 5 redirects, each of which must stay on one of the hosts in the given URLs, and at most
10,000,000 bytes per page. A refused or failed URL exits `2`. Run it inside the mission workspace
(or point `--into` there), then pass the paths to a mission with `--reference`. See
[06-mission-anchor.md](06-mission-anchor.md#references).

## `lha run-local`

Runs a mission from an explicit checklist, in this process, without Temporal.

| Option | Default | Meaning |
|---|---|---|
| `--title TEXT` | the checklist file's title, else `mission` | mission title |
| `--item TEXT` | repeatable | one checklist item; ids are `01`, `02`, ... in order |
| `--checklist FILE` | none | import the checklist from a file instead of `--item` |
| `--workdir TEXT` | `.lha/workspaces/local` | workspace; initialized as a git repo |
| `--description TEXT` | `""` | mission description |

Give exactly one of `--item` (one or more) and `--checklist`.

Plus the shared options. It initializes the anchor, then runs cycles until the checklist is
complete, deadlocked, refused by the governor, a loop is detected, the decision log fails
verification, or `LHA_MAX_CYCLES` is reached.
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
| `--task TEXT` | `""` | mission description; the Planner decomposes it |
| `--title TEXT` | `mission` (or the checklist file's title) | mission title |
| `--checklist FILE` | none | import the checklist instead of planning |
| `--workdir TEXT` | `.lha/workspaces/mission` | workspace |

One of `--task` and `--checklist` is required. With `--checklist`, the Planner is not called and
`--task`, if given, becomes the mission description. If the planner's reply cannot be parsed, the
plan falls back to one item built from the description.

## `lha orchestrate`

Plans, then runs the multi-agent organization locally: per item, read-only researchers, the Lead
Engineer loop, reflection on failure, and an independent reviewer that can reopen an item. Items
the Planner gave disjoint file write-sets run in parallel waves: one implementer per item in its
own git worktree, then an integrator merges the verified branches (up to
`LHA_MAX_PARALLEL_IMPLEMENTERS` items per wave; see
[11-multi-agent-organization.md](11-multi-agent-organization.md)). With the `claude` backend each
role uses its tier's model ([13-models.md](13-models.md#per-role-routing-lha-orchestrate)).

| Option | Default | Meaning |
|---|---|---|
| `--task TEXT` | required | mission description |
| `--title TEXT` | `mission` | mission title |
| `--workdir TEXT` | `.lha/workspaces/org` | workspace |

`orchestrate` has no `--checklist` option; it always plans. Output and exit codes as for
`run-local`.

## `lha decisions`

Prints the design decisions committed in a mission workspace's `.lha/decisions.ndjson`, read from
`HEAD`, or verifies their hash chain ([mission anchor](06-mission-anchor.md#decisionsndjson)).
Needs no model, sandbox or server.

| Option | Default | Meaning |
|---|---|---|
| `--workdir TEXT` | `.` | the mission workspace (the git repository holding `.lha/`) |
| `--verify` | off | verify the chain instead of printing the records |
| `--limit INTEGER` | `0` | print only the newest N records (`0`: all) |

Without `--verify`, the chain is verified first. Then each record prints as
`<n>. [<cycle_id>] <decision>`, followed by `why:`, and `rejected:` and `affects:` lines when
those fields are set. `<n>` counts from the oldest record. `(no decisions recorded)` is printed
for an empty log.

With `--verify`, it prints `decision chain OK: <n> record(s), <m> chained`. If the log has
legacy (pre-chain) records, a second line gives how many, and whether a chained record seals
them. If the chain fails, it prints `decision chain BROKEN: <problem>` (for example
`line 3: hash mismatch`).

Exit codes: `0` when the chain verifies; `1` when it does not (with or without `--verify`; without
it the message goes to stderr as `error: ...`); `2` when `--workdir` has no `.lha/` directory.

## `lha missions`

```
lha missions [--limit N]
```

Lists the missions in the mission store, most recently updated first, one per line:
`<mission_id>  <status>  <known spend> [(+<n> unknown-cost)]  calls <n>  head <sha prefix>  updated <time>  <title>`.
Prints `no missions recorded` when the store is empty.

| Option | Default | Meaning |
|---|---|---|
| `--limit INTEGER` (>= 1) | `20` | how many missions |

The store is Postgres when `LHA_POSTGRES_DSN` is set, otherwise the SQLite file at
`LHA_SQLITE_PATH`; unset, that is a per-user file every process shares: `$XDG_DATA_HOME/lha/lha.sqlite3`, else `~/Library/Application Support/lha/lha.sqlite3` on macOS or `~/.local/share/lha/lha.sqlite3` on Linux, so `mission-start`, the worker and this command
meet in the same store wherever they run from (`lha config` prints the resolved location as
`mission store = ...`). A relative `LHA_SQLITE_PATH` resolves against the current directory and
logs a warning. A run whose SQLite path would fall inside its own workspace keeps its database in
`<workdir>/.git/lha/` instead, which this command does not read unless `LHA_SQLITE_PATH` points
there. If Postgres is configured but unusable, it warns on stderr and reads SQLite (or exits `2`
with `LHA_POSTGRES_FALLBACK_TO_SQLITE=false`).

The status is the mission row's, written by the run paths; for a durable mission the workflow
also writes `SLEEPING`, `DEGRADED_PARK`, an open gate's `WAITING_ON_HUMAN` and the final
outcome. A row that reached `DONE`, `IMPOSSIBLE` or `ABORTED` keeps it (a late write from a
cycle that was still finishing cannot turn it back to `RUNNING`). Those writes are best effort,
so [`mission-status`](#lha-mission-status) is the live source.

## `lha costs`

```
lha costs MISSION_ID [--limit N]
```

Prints the newest `--limit` rows of the mission's cost ledger (time, cycle id, role, model, input
and output tokens, USD or `unknown`), then a total line: calls, known USD, unknown-cost calls and
tokens. Exits `1` with `error: no cost ledger rows for mission <id>` when the ledger is empty.

| Option | Default | Meaning |
|---|---|---|
| `--limit INTEGER` (>= 0) | `50` | how many of the most recent calls to list (`0`: totals only) |

It reads the same store as [`missions`](#lha-missions). See
[10-cost-and-budget.md](10-cost-and-budget.md).

## `lha gates`

```
lha gates [MISSION_ID] [--limit N]
```

Lists the human gates recorded in the mission store's `hitl_gates` table, most recently opened
first; with `MISSION_ID`, only that mission's. Each gate prints three or four lines:

```
<opened at>  <mission_id>  <gate_id>  <kind>  <status>  reminders <n>  <decision> by <who> at <time>
  question: <the question, secrets redacted>
  options: approve | reject
  request: <tool> <argv or arguments>
```

An open gate shows `open, default <action> at <deadline>` instead of the decision. `kind` is
`tool_call` (an irreversible action) or `deadlock`; `status` is `OPEN`, `ESCALATED` (open, with at
least one reminder sent), `RESOLVED` (a human answered) or `DEFAULTED` (the default applied).
Who: `human (human_decision signal)` for a durable decision (the signal carries no identity),
`default (timeout)`, `terminal:<login>` for a local `--approve-interactive` answer, `timeout`,
`end of input` or `non-interactive (stdin is not a TTY)`. Prints `no gates recorded` when there
are none.

| Option | Default | Meaning |
|---|---|---|
| `--limit INTEGER` (>= 1) | `50` | how many gates |

Durable missions record each gate event from the `notify_gate` activity; local runs record the
terminal approver's. One row per gate: an event repeated by a retried activity changes nothing,
reminders only raise the count of an open gate, and a closed gate stays closed. The anchor's
`gate_*` events and the workflow history remain the complete record. It reads the same store as
[`missions`](#lha-missions).

## `lha worker`

Connects to `LHA_TEMPORAL_ADDRESS` / `LHA_TEMPORAL_NAMESPACE` and serves `MissionWorkflow` and
`SubAgentWorkflow` on `LHA_TASK_QUEUE` until interrupted. No options. The model, sandbox, egress,
trusted checks, protected paths, replanning limits and budget used by durable missions come from
this process's settings. See
[14-running-on-temporal.md](14-running-on-temporal.md).

## `lha mission-start`

Plans the task (or imports `--checklist`), initializes the anchor at `--workdir`, and starts
`MissionWorkflow` with workflow id `mission:<mission_id>`. Prints `started mission <mission_id>
(workflow id: mission:<mission_id>)` and returns without waiting.

| Option | Default | Meaning |
|---|---|---|
| `--task TEXT` | `""` | mission description; the Planner decomposes it (give `--task` or `--checklist`) |
| `--title TEXT` | `mission` (or the checklist file's title) | mission title |
| `--checklist FILE` | none | import a `.json` checklist or `.md` roadmap instead of planning |
| `--reference TEXT` | none | vendored reference material recited every cycle (repeatable) |
| `--workdir TEXT` | `.lha/workspaces/durable` | workspace; resolved to an absolute path here before it is sent to the worker |
| `--max-cycles INTEGER` | `LHA_MAX_CYCLES` | cycle ceiling |
| `--deadlock-gate-hours FLOAT` | `24.0` | on deadlock, wait this long for `retry` / `abort` / `impossible`; `0` ends the mission `IMPOSSIBLE` at once |
| `--deadlock-default TEXT` | `LHA_DEADLOCK_GATE_DEFAULT` (`abort`) | the deadlock gate's decision on timeout: `abort` or `impossible`; anything else exits `2` |
| `--approval-timeout-hours FLOAT` | `LHA_APPROVAL_TIMEOUT_S` (24 h) | how long a queued irreversible action waits for approval before it is rejected |
| `--cycle-pause-seconds INTEGER` (>= 0) | `LHA_CYCLE_PAUSE_SECONDS` (0) | durable pause between cycles (status `SLEEPING`) |
| `--start-in-seconds INTEGER` (>= 0) | `0` | sleep (status `SLEEPING`) before the first cycle |

Plus `--check` and `--no-default-checks`. The check commands and the gate / sleep settings travel
in the workflow input (with `LHA_GATE_ESCALATION_SECONDS` and `LHA_IMPOSSIBLE_AFTER_FAILURES`);
they are resolved by this command, not by the worker. Before starting the workflow it writes the
mission row (`RUNNING`, or `SLEEPING` with `--start-in-seconds`; `ABORTED` if the start fails)
and the Planner's spend to the mission store. Irreversible actions are
approved with [`mission-approve`](#lha-mission-approve).

## `lha mission-status`

```
lha mission-status MISSION_ID
```

Queries workflow `mission:MISSION_ID` and prints `status=<status> cycles=<n>`, then, when they
apply: the open gate (`gate_v1`: kind, id, question, options, default on timeout, opened /
deadline, reminders sent and the next one, a recommendation, and for a queued action its tool,
arguments, reason and fingerprint), `sleeping until <time>` (`resume_at`), and the last 8 lines of
`gate_log_v1`. For a workflow whose worker does not answer `gate_v1` it prints `waiting on:
<question>` from `open_question` instead. Works while running and after the workflow has closed;
a worker must be running to answer the queries.

## `lha mission-approve`

```
lha mission-approve MISSION_ID --decision TEXT
```

| Option | Default | Meaning |
|---|---|---|
| `--decision TEXT` | required | `approve` or `reject` (a queued irreversible action); `retry`, `abort` or `impossible` (the deadlock gate) |

It queries the open gate first and checks the decision against that gate's options, so a decision
the gate does not offer (for example `approve` at the deadlock gate) is refused with exit `2`; the
gate is printed on stderr and nothing is sent. With no gate open, any of the five decisions is
sent and held until the next gate. The value is lower-cased; a word outside those five exits `2`
before contacting Temporal. On success it sends signal `human_decision_v1` and prints
`sent decision '<decision>' to mission <id>`. See
[14-running-on-temporal.md](14-running-on-temporal.md#5-gates-sleep-and-abort) and
[15-operations-runbook.md](15-operations-runbook.md).

## `lha mission-snooze`

```
lha mission-snooze MISSION_ID --seconds INT
```

Signals `snooze_v1`: the mission sleeps (status `SLEEPING`, a durable timer) for `--seconds`
before its next cycle; `--seconds 0` wakes a sleeping mission. A cycle already running finishes
first. Prints `mission <id>: snoozed <n>s` or `mission <id>: woken`.

## `lha mission-abort`

```
lha mission-abort MISSION_ID
```

Requests cancellation of workflow `mission:MISSION_ID` and prints `cancelled mission <id>`. The
workflow waits for a cycle in flight to acknowledge the cancellation, then writes `ABORTED` to
the mission row and closes as Cancelled; the interrupted cycle is not committed.
