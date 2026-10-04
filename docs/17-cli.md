# CLI reference

The `lha` command is defined in [`python/src/lha/cli/main.py`](../python/src/lha/cli/main.py)
(Typer). Run it from `python/` with `uv run lha <command>`, or install the package and run `lha`.
`lha` with no arguments prints help. Every command also reads the `LHA_*` settings described in
[18-configuration.md](18-configuration.md), and before it runs installs the OTLP trace exporter
when one is configured ([16-observability.md](16-observability.md)).

The Go implementation mirrors this surface: the same command names, options, settings, output
and exit codes. `go/cmd/lha` implements every command: `version`, `config`, `run-local`,
`mission`, `orchestrate`, `decisions`, `vendor`, `missions`, `costs`, `gates`, `db migrate` (Go
needs no extra for `db migrate`: the Postgres driver is built in), `objects prune`,
`memory reembed`, `worker`, `mission-start`
(including the organization options `--research`, `--review` and `--max-parallel`),
`mission-status`, `mission-approve`, `mission-snooze`, `mission-steer`, `mission-edit` and `mission-abort`. It installs the trace
exporter at start like Python; see [04-choosing-an-implementation.md](04-choosing-an-implementation.md).

## Commands

| Command | Purpose | Needs |
|---|---|---|
| [`version`](#lha-version) | print the version | nothing |
| [`config`](#lha-config) | print resolved settings, secrets masked (`--fingerprints` adds a hash of each secret) | nothing |
| [`db migrate`](#lha-db-migrate) | apply SQL migrations | Postgres, `postgres` extra |
| [`objects prune`](#lha-objects-prune) | delete old payloads from the ClaimCheck object store | nothing |
| [`memory reembed`](#lha-memory-reembed) | re-embed stored memory with the configured embedder | the mission store, an embedder |
| [`vendor`](#lha-vendor) | snapshot reference pages into the workspace | network access to the URLs |
| [`workspace init`](#lha-workspace-init) | build a multi-repo workspace: a repository holding each `--repo` as a git submodule | git access to the repositories |
| [`run-local`](#lha-run-local) | run a given or imported checklist locally | a sandbox |
| [`mission`](#lha-mission) | plan a task (or import a checklist), then run it locally | a sandbox |
| [`orchestrate`](#lha-orchestrate) | plan, then run the multi-agent org locally | a sandbox |
| [`decisions`](#lha-decisions) | print or verify a mission's decision log | a mission workspace |
| [`missions`](#lha-missions) | list persisted missions with status and recorded spend | the mission store |
| [`costs`](#lha-costs) | print a mission's persisted cost ledger | the mission store |
| [`gates`](#lha-gates) | list recorded human gates: question, options, reminders, decision, who and when | the mission store |
| [`mission-report`](#lha-mission-report) | one page about a mission: items, verdicts, reviews, gates, spend, commits | a mission workspace and/or the mission store |
| [`labels export`](#lha-labels-export) | export a mission's gate answers, tool approvals, verifier and review verdicts as JSON Lines labels | a mission workspace and/or the mission store |
| [`eval check`, `eval run`](#lha-eval) | validate gold evaluation sets, and score a judge against them | gold `.jsonl` files |
| [`worker`](#lha-worker) | serve durable missions | Temporal |
| [`serve`](#lha-serve) | serve the mission UI's API on loopback | the mission store; Temporal for durable missions' live state and controls; the `serve` extra (Python) |
| [`mission-start`](#lha-mission-start) | plan (or import) and start a durable mission | Temporal, a worker |
| [`mission-status`](#lha-mission-status) | query status, cycles, open gate, sleep and recent gate events | Temporal |
| [`mission-approve`](#lha-mission-approve) | answer the open gate (a queued irreversible action, or the deadlock gate) | Temporal |
| [`mission-snooze`](#lha-mission-snooze) | sleep a mission before its next cycle, or wake it | Temporal |
| [`mission-steer`](#lha-mission-steer) | add an operator note that every following cycle's prompt includes | Temporal |
| [`mission-edit`](#lha-mission-edit) | add, remove, edit, reopen, block or unblock checklist items of a mission in flight | Temporal, or a mission workspace with `--workdir` |
| [`mission-abort`](#lha-mission-abort) | cancel a durable mission | Temporal |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success; for the local mission commands, every item verified done |
| `1` | a local mission ended without completing (deadlocked, stopped by the governor, loop, `max_cycles`, decision log failed verification, model unavailable); `mission`'s Planner call failed because the model stayed unavailable (`error: model unavailable: ...`); `decisions` found a broken chain; `costs` found no ledger rows; `mission-edit --workdir` refused the batch or found a cycle running; or an unhandled error (Python traceback) |
| `2` | usage error, or a handled operator error printed as `error: ...` on stderr (bad `--check`, unknown `--sandbox`, unsafe `local` sandbox, missing optional module, missing `LHA_POSTGRES_DSN`, an unusable Postgres store with `LHA_POSTGRES_FALLBACK_TO_SQLITE=false`, a Rule-of-Two violation or invalid web settings, an invalid `--checklist` file, neither or both of `--item`/`--checklist`, an unknown `--decision` or one the open gate does not offer, a refused `vendor` URL, a `mission-edit` call with nothing to do, more than 50 edits, a malformed `ID=VALUE` or an unreadable `--edits` file) |
| `3` | the budget governor refused the planning call (`mission`, `orchestrate`, `mission-start`) |

Once a local run is under way, a governor refusal (before a cycle or before a single model call)
stops it normally: the summary is printed with `stopped_reason` `governor: ...` and the exit code
is `1`. Only a refusal during planning, before anything has run, exits `3`. The Temporal commands
(`worker`, `mission-status`, `mission-approve`, `mission-snooze`, `mission-steer`, `mission-edit`, `mission-abort`) do not
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

`run-local`, `mission`, `orchestrate` and `mission-start` also take `--checklist FILE`: seed the mission with a
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
model call or workspace write. `e2b` (Python only; the Go `lha` refuses it) requires the
`e2b` extra (`uv sync --extra e2b`), an E2B account and a template with `python3`, and copies the
workspace into the microVM and back around every command; `docker` requires the `sandbox` extra
and a Docker daemon.

## `lha version`

Prints `lha <version>` (currently `lha 0.1.0`). No options.

## `lha config`

Prints every setting as `name = value`, one per line, in declaration order. `SecretStr` settings
(`openai_api_key`, `anthropic_api_key`, `postgres_dsn`, `langfuse_secret_key`,
`voyage_api_key`, `web_credentials`, `web_search_api_key`, `system_one_api_key`,
`gate_webhook_url`) print `***` when set and `None` when
unset. The last line, `mission store = ...`, is where `lha missions` and `lha costs` read:
`sqlite <absolute path>` (the resolved `LHA_SQLITE_PATH` or the per-user default), or
`postgres (LHA_POSTGRES_DSN)` with its SQLite fallback. `--fingerprints` adds
`<name> fingerprint = <12 hex digits>` (the SHA-256 of the value) for each secret that is set,
so a rotated key can be checked without either value being shown
([15-operations-runbook.md](15-operations-runbook.md#common-actions)).

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
The Go `lha db migrate` applies the same files with the same `schema_migrations` bookkeeping and
advisory lock, so either implementation sees what the other applied.

## `lha objects prune`

```
lha objects prune --older-than-days N [--dry-run]
```

Deletes the objects in the ClaimCheck store at `LHA_OBJECT_STORE_ROOT` not modified for `N` days
(`N` >= 1; only files named by a sha256 key are considered). Prints
`<count> objects deleted (<MiB> MiB), <kept> kept`, or `... to delete ...` with `--dry-run`.
Durable missions offload every payload over 32 KiB there and journal only its key, so the store
grows with every cycle. An object a live workflow history still refers to must not be deleted:
choose `N` longer than your longest mission plus the Temporal namespace's history retention
([runbook](15-operations-runbook.md#memory-and-disk-over-a-long-mission)). With
`LHA_OBJECT_RETENTION_DAYS=N` set, `lha worker` runs the same deletion as it starts.

## `lha memory reembed`

```
lha memory reembed [MISSION_ID] [--dry-run]
```

Re-embeds stored memory rows that the dense channel cannot see (no vector, or a vector from
another embedder model or version) with the configured embedder, for `MISSION_ID` or every
mission. Prints one `<mission>  <n> rows` line per mission, then `<total> rows re-embedded with
<model> (<version>)`, or `nothing to re-embed for <model> (<version>)`. `--dry-run` only counts
(`... rows to re-embed with ...`). Exits `2` when memory is disabled (`LHA_MEMORY_ENABLED=false`)
or there is no embedder to use (`LHA_MEMORY_EMBEDDER=none`, or the embedder is unreachable).
Missions also do this on their own, 64 rows per cycle ([12-memory.md](12-memory.md#re-embedding)).

## `lha workspace init`

```
lha workspace init DIRECTORY --repo [NAME=]URL[@REF] [--repo ...]
```

Creates `DIRECTORY` as a git repository (or extends one) holding each `--repo` as a git submodule,
a **member**, and commits them as one workspace commit, `lha: workspace members <names>`. `URL`
is anything `git submodule add` accepts, including a local path; `NAME` defaults to the URL's
last path component without `.git`; `@REF` checks the member out at that branch, tag or commit.
Prints `<name> <- <url>[@ref] (<member HEAD, 12 chars>)` per member added, `<name>: already a
member, kept` on stderr for one it has, and `workspace <dir>: <n> members`. Exit `2` for a spec
without a URL, a name that repeats, an unusable name, or a directory that already anchors a
mission (members are added before the mission starts); exit `1` when git cannot add a member.
Point a mission at it with `--workdir`; see
[24-large-missions.md](24-large-missions.md#optional-one-mission-over-several-repositories) for
what a mission on a workspace does, and what it does not (`--max-parallel` is refused there).

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
10,000,000 bytes per page. Each hop's host is resolved once and only the checked addresses are
dialled (no DNS rebinding between the check and the connection). A refused or failed URL exits
`2`. The Go `lha vendor` writes the same files and `MANIFEST.json`. Run it inside the mission workspace
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
| `--task TEXT` | `""` | mission description (required unless `--resume` or `--checklist`) |
| `--title TEXT` | the checklist's, else `mission` | mission title |
| `--checklist FILE` | none | run this checklist instead of planning (no ownership map, so no parallel waves) |
| `--workdir TEXT` | `.lha/workspaces/org` | workspace |
| `--resume` | off | continue the mission already anchored in `--workdir` instead of planning a new one |
| `--research INTEGER` (0-4) | `2` | read-only researchers per item (`0`: none) |
| `--review/--no-review` | `--review` | the independent reviewer after every verified item; with `--no-review` the [pre-review screen](07-verification.md#pre-review-screen) still runs and forces the review when it finds weakened tests |

Without `--resume` or `--checklist` it plans. `--checklist` cannot be combined with `--resume`
(exit `2`): a resumed mission keeps its committed checklist. A workdir that
already holds a mission (`.lha/mission.json` committed) is refused without `--resume` (exit `2`),
so a second invocation never replaces a mission's checklist; `--resume` on a workdir without one
is refused too. With `--resume` there is no planning, and `--task`, `--title` and `--reference`
are ignored: the committed spec, checklist, ownership map, decisions and events are used, and
uncommitted residue from the interrupted run is discarded
([Resuming](11-multi-agent-organization.md#resuming-lha-orchestrate)). Output and exit codes as
for `run-local`.

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
`mission store = ...`). A relative `LHA_SQLITE_PATH` resolves against the current directory;
Python logs a warning, Go does not. A run whose SQLite path would fall inside its own workspace keeps its database in
`<workdir>/.git/lha/` instead, which this command does not read unless `LHA_SQLITE_PATH` points
there. If Postgres is configured but unusable, it warns on stderr and reads SQLite (or exits `2`
with `LHA_POSTGRES_FALLBACK_TO_SQLITE=false`).

The status is the mission row's, written by the run paths; for a durable mission the workflow
also writes `SLEEPING`, `DEGRADED_PARK`, an open gate's `WAITING_ON_HUMAN` and the final
outcome. A row that reached `DONE`, `IMPOSSIBLE` or `ABORTED` keeps it (a late write from a
cycle that was still finishing cannot turn it back to `RUNNING`). Those writes are best effort,
so [`mission-status`](#lha-mission-status) is the live source.

`missions`, `costs` and `gates` print the same lines in both implementations, and each reads a
store the other wrote (`go/cmd/lha/store_cmds_test.go` compares their output on one store).

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

## `lha eval`

```
lha eval check FILES...
lha eval run FILES... [--judge recorded|screen]
```

Gold evaluation sets ([25-system-one.md](25-system-one.md#gold-evaluation-sets),
[`eval/gold/`](../eval/gold/README.md)): `lha labels export` rows with a `gold` judgment
(`label`, `by`, `note`) and `tags`. `check` reads every file (exit `2` at the first line that is
not a schema-1 label row with a usable `gold`, or when a gold label is outside its source's
vocabulary or a judgment appears twice, each error on stderr) and prints
`<n> gold rows (<n> gate, <n> tool_approval, <n> verifier, <n> review) in <k> file(s)`. `run`
scores a judge and prints one block per source present:

```
judge: recorded
verifier: 52 rows, 52 judged, 40 agree (0.77); failed: precision 1.00, recall 0.59 (tp 17, fp 0, fn 12, tn 23)
  mission_c61985f91887 c3 02: judged passed, gold failed [conftest-injection,bypass,pair-1] (pair 1, no review: ...)
review: 21 rows, 21 judged, 15 agree (0.71); block: precision 1.00, recall 0.71 (tp 10, fp 0, fn 4, tn 7)
total: 73 rows, 73 judged, 55 agree (0.75)
```

`judged` counts the rows the judge answered (`<source>: <n> rows, 0 judged (the judge abstains)`
otherwise); precision and recall are of the source's refusing label (`reject`, `failed`,
`block`), `n/a` when undefined. `--judge recorded` (default) scores the label the mission
recorded; `--judge screen` re-runs the pre-review screen on each review row's `diff` and
abstains elsewhere. An unknown judge exits `2`. Both implementations print the same report.

## `lha mission-report`

```
lha mission-report [MISSION_ID] [--workdir DIR]
```

One page about a mission, after the fact or while it runs, from the anchor at `--workdir` and the
mission store: the mission and its definition of done; the store's status, the head commit and
the commit count; every item with its status, attempts, witnesses and (while not done) the first
line of its last failure, with the checklist's own verdict (complete, or deadlocked and why); the
cycles' verdicts, the review verdicts, how many screened diffs the pre-review screen flagged and
how many reflections were written; the mission's gates as [`lha gates`](#lha-gates) prints them;
and the [`lha costs`](#lha-costs) total. `MISSION_ID` defaults to the mission the anchor's latest
`orchestrate` event names; without an anchor it is required and only the store is read. A
`run-local` or `mission` workspace names no mission, so its store sections say so. Both
implementations render the same bytes from the same inputs (`spec/state/report.json`).

```
# Mission: Fabric emulator
Build the spine.
Definition of done: all witnesses pass
mission m1  status RUNNING  head abcdef123456  commits 7

## Items (1/3 done, deadlocked: ...)
01  done        attempts  1  do thing one
      witnesses: pytest:tests/test_a.py::test_x
02  blocked     attempts  3  do thing two
      last failure: exit 1
...
```

## `lha labels export`

```
lha labels export [MISSION_ID] [--workdir DIR] [--out FILE] [--diffs]
```

Writes the mission's own judgments as JSON Lines, one object per line, for fitting the System One
thresholds that are still to be measured ([25-system-one.md](25-system-one.md#labels)). Four
sources, in this order: every `tool_approval`, `cycle` (with a verdict) and `review` event in the
anchor's committed `.lha/events.ndjson` at `--workdir`, then the mission's closed gates
(`RESOLVED` or `DEFAULTED`) from the mission store, oldest opening first. A `tool_approval` whose
fingerprint a gate row also carries is the same decision recorded twice, so only the gate row is
kept. Every object has the same keys:

```json
{"at":"2026-01-01T01:05:00+00:00","by":"terminal:calvin","cycle_id":"","input":{"kind":"tool_call","options":["approve","reject"],"question":"Allow `git push`?","request":{"fingerprint":"fp1","tool":"run_command"},"risk":"high"},"item_id":"","label":"approve","mission_id":"m1","schema":1,"source":"gate"}
```

| Key | Meaning |
|---|---|
| `schema` | `1`; bumped when a row's shape changes |
| `source` | `gate` (a human or the default answered a gate), `tool_approval` (the dispatcher's record of a gated call), `verifier` (a cycle's verdict), `review` (the reviewer's verdict on a verified diff) |
| `label` | the judgment: `approve` / `reject`, `passed` / `failed` (or another verifier verdict), `approve` / `block` / `unparsed` |
| `by` | who judged: the gate's `resolved_by`, `default` when the default applied, `verifier`, `reviewer` |
| `mission_id`, `cycle_id`, `item_id`, `at` | where the judgment comes from; empty when the source does not record it (an event has no timestamp, a gate no cycle) |
| `input` | what was judged, secrets redacted: the gate's question, options, risk and request; the approval's tool, arguments, reason and fingerprint; the cycle's status, tool calls, rolled-back files, split and per-check `name`, `passed`, `gating`, `exit_code` (no timings); the review's commit range, blocking issues and advisory notes, plus `diff` with `--diffs` |

| Option | Default | Meaning |
|---|---|---|
| `MISSION_ID` | the one the anchor's latest `orchestrate` event names | whose gates to read from the store; required when there is no anchor, in which case only the gates are exported |
| `--workdir DIR` | `.` | the mission workspace; without a `.lha/` directory only the store is read |
| `--out FILE` | `-` (stdout) | where to write |
| `--diffs` | off | add each reviewed `git diff base..head` (harness files excluded) to the review rows, cut to 20,000 characters |

Prints `<n> labels (<n> gate, <n> tool_approval, <n> verifier, <n> review)` on stderr, so stdout is
only the JSON Lines. Keys are sorted and separators compact, so both implementations write the
same bytes (`spec/systemone/labels.json`). A workspace whose events name no mission (a `run-local`
or `mission` run, which record no `orchestrate` event) exports its events with an empty
`mission_id` and no gates unless `MISSION_ID` is given.

## `lha worker`

Connects to `LHA_TEMPORAL_ADDRESS` / `LHA_TEMPORAL_NAMESPACE` and serves `MissionWorkflow` and
`SubAgentWorkflow`, with every activity they use (including the organization's `plan_round`,
`run_implementer`, `integrate_branch` and `review_cycle`), on `LHA_TASK_QUEUE` until
interrupted. No options. With `LHA_WORKER_DEPLOYMENT` and `LHA_WORKER_BUILD_ID` it polls as that
build of a Temporal Worker Deployment
([versioned deploys](14-running-on-temporal.md#versioned-deploys-worker-build-ids)); a half-set
pair exits `2`, and a failed `LHA_WORKER_PROMOTE` exits `1`. The model, sandbox, egress,
trusted checks, protected paths, replanning limits and budget used by durable missions come from
this process's settings. See
[14-running-on-temporal.md](14-running-on-temporal.md).

## `lha serve`

Serves the UI API ([`spec/serve/openapi.json`](../spec/serve/openapi.json)) on loopback until
interrupted, and prints the start-up URL first:

```text
lha serve: http://127.0.0.1:8765/?token=...
```

Opening that URL in a browser shows the mission UI ([`ui/`](../ui/README.md)): every mission,
and for each its live state, open gate, checklist, timeline, gates and spend, with the controls
for a durable mission. It sets the token cookie the UI's reads use; any other client sends the
token as the `X-LHA-Token` header, and every write needs the header. The server refuses a `Host` that is not
its own loopback address.

| Option | Default | Meaning |
|---|---|---|
| `--host` | `127.0.0.1` | `127.0.0.1` or `localhost`; anything else exits `2` |
| `--port` | `8765` | `0` picks a free port |

`LHA_SERVE_TOKEN` fixes the token (otherwise it is random). The server reads the mission store
(`lha config` shows which), each mission's anchor at the `workdir` its run recorded, and, for a
durable mission, its workflow's queries; the controls send the workflow's signals, as the
`mission-*` commands do. Both implementations' servers pass the same cases
([`spec/serve/README.md`](../spec/serve/README.md)), and either one shows missions the other's runs
record. The Python server needs the `serve` extra (`uv sync --extra serve`).

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
| `--research INTEGER` (0 to 4) | `0` | read-only researcher child workflows per item before each round |
| `--review` / `--no-review` | off | an independent reviewer after every verified item; a blocking review reopens it |
| `--max-parallel INTEGER` (0 to 8) | `0` | parallel implementer waves of up to N items with disjoint Planner-assigned files, each in its own git worktree; below 2 never |

With `--max-parallel 2` or more the Planner also assigns file ownership (`plan_mission`), which
is written to `.lha/ownership.json`. An imported `--checklist` has no file ownership, so it runs
serially (the command says so on stderr). The three organization options are off by default, so
a mission runs the Lead loop alone unless it opts in
([Durable execution](08-durable-execution.md#the-multi-agent-organization-opt-in)).

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
arguments, reason and fingerprint), `sleeping until <time>` (`resume_at`), the steering notes,
`checklist edits pending: <n> (applied before the next cycle)` (`pending_edits`, when a
[`mission-edit`](#lha-mission-edit) batch is waiting), and the last 8 lines of `gate_log_v1`. For a workflow whose worker does not answer `gate_v1` it prints `waiting on:
<question>` from `open_question` instead. Works while running and after the workflow has closed;
a worker must be running to answer the queries.

## `lha mission-approve`

```
lha mission-approve MISSION_ID --decision TEXT
```

| Option | Default | Meaning |
|---|---|---|
| `--decision TEXT` | required | `approve` or `reject` (a queued irreversible action); `retry`, `abort` or `impossible` (the deadlock gate) |
| `--as TEXT` | unset | who decides (at most 200 characters); recorded as the gate's `resolved_by`, `<who> (human_decision signal)`, in [`lha gates`](#lha-gates) and the gate log |

It queries the open gate first and checks the decision against that gate's options, so a decision
the gate does not offer (for example `approve` at the deadlock gate) is refused with exit `2`; the
gate is printed on stderr and nothing is sent. With no gate open, any of the five decisions is
sent and held until the next gate. The value is lower-cased; a word outside those five exits `2`
before contacting Temporal. On success it sends signal `human_decision_v1` (or `human_decision_v2`
with `--as`) and prints `sent decision '<decision>' to mission <id>` (`... as <who>`). See
[14-running-on-temporal.md](14-running-on-temporal.md#5-gates-sleep-and-abort) and
[15-operations-runbook.md](15-operations-runbook.md).

## `lha mission-snooze`

```
lha mission-snooze MISSION_ID --seconds INT
```

Signals `snooze_v1`: the mission sleeps (status `SLEEPING`, a durable timer) for `--seconds`
before its next cycle; `--seconds 0` wakes a sleeping mission. A cycle already running finishes
first. Prints `mission <id>: snoozed <n>s` or `mission <id>: woken`.

## `lha mission-steer`

```
lha mission-steer MISSION_ID --note TEXT
```

Signals `steer_v1` with the note (leading and trailing whitespace stripped). Every following
cycle's prompt, for the lead and for the organization's implementers, shows the mission's notes
under "Operator steering (most recent last)"; the workflow keeps the last 20. A note that is
empty or longer than 2000 characters is refused before anything is sent (exit 2). Prints
`mission <id>: steering note added (<n> chars; the last 20 notes are kept)`.
[`mission-status`](#lha-mission-status) lists the notes (`steering notes (<n>, latest last):`,
the last three, each on one line cut to 120 characters).

## `lha mission-edit`

```
lha mission-edit MISSION_ID [OPTIONS]
lha mission-edit --workdir DIR [OPTIONS]
```

| Option | Default | Meaning |
|---|---|---|
| `--add TEXT` | none | add a `todo` item with this description (repeatable); witnesses the roadmap way, `"Do X (witness: cmd:make test, go:TestX)"` |
| `--remove ID` | none | remove an open item (repeatable); a `done` or `split` item is refused |
| `--reopen ID` | none | a `done` item back to `todo`, its verification forgotten (repeatable) |
| `--block ID` | none | park an open item as `blocked` (its `last_failure` says who) (repeatable) |
| `--unblock ID` | none | a `blocked` item back to `todo` (repeatable) |
| `--describe ID=TEXT` | none | replace an open item's description (repeatable); a witness suffix adds to its witnesses |
| `--depends ID=DEP[,DEP]` | none | replace an open item's dependencies; `ID=` clears them (repeatable) |
| `--edits FILE` | unset | a JSON list of edit objects, or `{"edits": [...]}`, for anything the options cannot say (an explicit `id`, `after`, `witnesses`, `notes`, `allow_harness_edits`); applied before the options |
| `--as TEXT` | unset | who edits (at most 200 characters); recorded in the anchor's `checklist_edit` event and commit |
| `--workdir DIR` | unset | edit the anchor of a local mission (`lha mission`, `lha orchestrate`) directly, between runs, instead of signalling a durable one |

One call is one batch. The edit objects are `{"op": "add", "description", "id"?, "witnesses"?,
"depends_on"?, "after"?, "allow_harness_edits"?, "notes"?}`, `{"op": "remove" | "reopen" |
"block" | "unblock", "id"}` and `{"op": "edit", "id", "description"?, "witnesses"?,
"depends_on"?, "notes"?, "allow_harness_edits"?}`; the options become edits in the order
`--edits` file, `--describe`, `--depends`, `--reopen`, `--unblock`, `--block`, `--remove`,
`--add`. An added item without an `id` gets one past the largest numeric id the checklist has
ever had (`04` after `01`..`03`, and still `04` when `03` was removed), so the anchor's history
never names two items the same. The batch is validated as a whole: unknown items or fields,
a `done` item edited or removed, dependencies that would dangle, an invalid witness, or a
checklist left empty refuse the whole batch and change nothing. Statuses the ops do not name are
untouched: the item a cycle is working stays `in_progress`.

With `MISSION_ID` it sends signal `checklist_edit_v1` (`{"edits": [...], "by": ...}`) and prints
`mission <id>: <n> checklist edit(s) queued (applied before the next cycle; 'lha mission-status'
shows the outcome)`. The workflow applies the batch with the `edit_checklist` activity before its
next cycle starts, at once while the mission sleeps (the sleep goes on), or at the deadlock gate's
`retry` before the blocked items are reset (so removing or reopening items is how an operator
resolves a deadlock the blocked items alone cannot); never while a cycle runs, and not while a
gate is open ([`mission-status`](#lha-mission-status) shows `checklist edits pending`). Each
applied batch is one anchor-only commit, `lha: checklist edited by <who>`, with a
`checklist_edit` event; the gate log then shows `checklist edited by <who>: added 04; removed 02`
or `checklist edit refused: <why>`. A refused batch is dropped; later batches still apply. A
mission holds at most 20 unapplied batches.

With `--workdir DIR` the same batch is applied to that anchor at once (the checkout is reset to
`HEAD` first, as before a cycle) and the summary is printed; a refusal exits `1` with the reason,
as does a cycle holding the workdir lock. Exit `2` before anything is sent or changed: no edit
given, more than 50, a malformed `ID=VALUE`, an unreadable `--edits` file, both or neither of
`MISSION_ID` and `--workdir`, `--as` over 200 characters.

## `lha mission-abort`

```
lha mission-abort MISSION_ID
```

Requests cancellation of workflow `mission:MISSION_ID` and prints `cancelled mission <id>`. The
workflow waits for a cycle in flight (or, in a mission started with `--research`, `--review` or
`--max-parallel`, every activity of the round in flight: implementers, integrations, reviews) to
acknowledge the cancellation, then writes `ABORTED` to the mission row and closes as Cancelled;
the interrupted cycle is not committed, and an interrupted wave's branches are not integrated.
