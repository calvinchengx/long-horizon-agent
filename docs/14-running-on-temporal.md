# Running on Temporal

The local commands (`lha run-local`, `lha mission`, `lha orchestrate`) run a mission in one
process. The durable path runs the same agent loop inside a Temporal activity, so a mission
survives worker crashes and restarts. This page covers the durable path with the Python
implementation; the Go port does not have a Temporal worker yet (see [23-roadmap.md](23-roadmap.md)).
Background on the design is in [08-durable-execution.md](08-durable-execution.md).

## 1. Start the stack

```bash
# from the repository root; compose reads ./.env
printf 'LANGFUSE_NEXTAUTH_SECRET=%s\nLANGFUSE_SALT=%s\n' "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" >> .env
docker compose up -d
```

[`docker-compose.yml`](../docker-compose.yml) starts these services, every port bound to
`127.0.0.1`:

| Service | Image | Port | Purpose |
|---|---|---|---|
| `temporal` | `temporalio/auto-setup:1.27` | 7233 | Temporal server (namespace `default` is auto-created) |
| `temporal-db` | `postgres:16` | none | Temporal's own database |
| `temporal-ui` | `temporalio/ui:2.34.0` | 8080 | web UI |
| `appdb` | `pgvector/pgvector:pg16` | 5432 | LHA app database; runs `db/migrations/*.sql` on first start of an empty volume |
| `langfuse`, `langfuse-db` | `langfuse/langfuse:2`, `postgres:16` | 3000 | Langfuse server |

`docker compose up` refuses to start until `LANGFUSE_NEXTAUTH_SECRET` and `LANGFUSE_SALT` are set.
Only `temporal` (and its database) is required by the durable path; the mission workflow does not
read or write `appdb` or Langfuse (see [16-observability.md](16-observability.md)). None of these
services has authentication fit for a network: anyone who can reach port 7233 can start workflows
and send gate decisions.

## 2. Run a worker

```bash
cd python
uv run lha worker
```

`lha worker` connects to `LHA_TEMPORAL_ADDRESS` in namespace `LHA_TEMPORAL_NAMESPACE`, installs the
ClaimCheck data converter over `LHA_OBJECT_STORE_ROOT`, and polls `LHA_TASK_QUEUE`. It hosts:

| Kind | Names |
|---|---|
| Workflows | `MissionWorkflow`, `SubAgentWorkflow` |
| Activities | `run_agent_cycle`, `check_mission_health`, `unblock_items`, `read_mission_snapshot`, `run_subagent` |

It runs until interrupted. It has no options; everything comes from `LHA_*` settings of the worker
process. The model backend, sandbox (`LHA_SANDBOX`, default `docker`), budget ceiling and
`max_turns_per_cycle` used by a mission are the **worker's**, not the settings of the process that
started the mission. Settings are read once per process, so a change takes effect after a worker
restart. If the server is unreachable, the command exits with a Python traceback.

[`python/Dockerfile`](../python/Dockerfile) builds a worker image whose default command is
`lha worker`. Do not mount the host Docker socket into it; point `DOCKER_HOST` at a separate
daemon or use `LHA_SANDBOX=e2b`.

## 3. Start a mission

```bash
cd python
uv run lha mission-start --task "Add a slugify() helper with tests" \
  --workdir "$PWD/.lha/workspaces/slugify"
# started mission mission_3f9a1c0b2d4e (workflow id: mission:mission_3f9a1c0b2d4e)
```

`lha mission-start`, in order:

1. Plans the task into a checklist with the configured model (one metered call against
   `LHA_BUDGET_USD_CEILING` in this process).
2. Initializes the git mission anchor at `--workdir` (see [06-mission-anchor.md](06-mission-anchor.md)).
3. Starts `MissionWorkflow` with id `mission:<mission_id>` on `LHA_TASK_QUEUE`, passing
   `MissionInput(mission_id, workdir, check_commands)`.

It prints the id and returns; it does not wait for the mission. The workflow's other inputs keep
their defaults: `max_cycles=1000`, `cycles_before_can=200`, `budget_usd=None` (the worker's
ceiling), `park_initial_seconds=60`, `park_max_seconds=3600`, `deadlock_gate_seconds=0`.
`LHA_MAX_CYCLES` is not passed. There is no `--sandbox` option: the worker's `LHA_SANDBOX` applies.

`--workdir` is passed to the worker as given. A relative path is resolved against the worker's
current directory, so use an absolute path unless the worker runs in the same directory. The
worker must be able to reach the same filesystem path.

## 4. Watch it

```bash
uv run lha mission-status mission_3f9a1c0b2d4e
# status=RUNNING cycles=3
```

`mission-status` sends two queries, `status_v1` and `cycles_done`. Queries work while the workflow
runs and after it has closed, but a worker must be polling the task queue to answer them. The
workflow also answers `last_item`, `park_reason` and `rejected_decisions`, which the CLI does not
expose; use the Temporal UI or `temporal workflow query`.

Statuses the workflow sets:

| Status | When |
|---|---|
| `RUNNING` | dispatching or running a cycle |
| `DEGRADED_PARK` | a cycle exhausted its retries; sleeping and probing health |
| `WAITING_ON_HUMAN` | a deadlock gate is open (only when `deadlock_gate_seconds > 0`) |
| `DONE` | every item verified done |
| `IMPOSSIBLE` | deadlocked: items remain and none is actionable |
| `ABORTED` | budget exhausted, `max_cycles` reached, or a non-retryable failure |

`SLEEPING` is defined in [`durable/signals.py`](../python/src/lha/durable/signals.py) and in the
`missions` table comment but no code path sets it.

The Temporal UI at <http://localhost:8080> shows each workflow's event history: every activity
with its input, result, attempts and failures, the timers of a park, and each Continue-As-New.
Large payloads appear as ClaimCheck pointers (see [19-wire-contract.md](19-wire-contract.md)).
The committed work is in git: `git -C <workdir> log --oneline` and `<workdir>/.lha/progress.md`.

## 5. Gates and abort

```bash
uv run lha mission-approve mission_3f9a1c0b2d4e --decision retry
uv run lha mission-abort   mission_3f9a1c0b2d4e
```

`mission-approve` sends the `human_decision_v1` signal with `--decision` (default `approve`). The
workflow stores it until a gate consumes it. The only gate in `MissionWorkflow` is the deadlock
gate, which offers `retry` and `abort` (default `abort` on timeout). A decision that matches no
offered option (compared case-insensitively) is discarded, recorded in `rejected_decisions`, and
the gate keeps waiting. Because `mission-start` leaves `deadlock_gate_seconds` at `0`, a mission
started from the CLI never opens this gate: a deadlock ends the mission immediately with status
`IMPOSSIBLE`. The help text's `approve | reject | abort` does not match the gate's options.

`mission-abort` requests cancellation of the workflow. Temporal delivers it to a running
`run_agent_cycle` at its next heartbeat (every 5 s). The workflow does not catch it: it closes as
*Cancelled*, and its `status` query keeps the last value it set (typically `RUNNING`). Work of the
interrupted cycle is not committed; uncommitted files may remain in the working tree.

The `steer_v1` signal appends an operator note (at most 2000 characters; the last 20 are kept)
that every following cycle's prompt includes. No CLI command sends it; use `temporal workflow
signal --name steer_v1`.

## How a cycle runs

`MissionWorkflow` loops: run one `run_agent_cycle` activity, absorb its result, stop on a terminal
outcome, and Continue-As-New every 200 cycles or when Temporal suggests it.

| Setting | Value |
|---|---|
| start-to-close timeout | 1 hour |
| heartbeat timeout | 2 minutes (the activity heartbeats every 5 s) |
| retry policy | 5 attempts, 1 s initial, x2, 30 s maximum |
| non-retryable error types | `BudgetExceeded`, `MissionConfigError` |

Each attempt takes an exclusive `flock` on `<workdir>/.git/lha-cycle.lock` (waiting up to 300 s),
resets the checkout to `HEAD` (`reset --hard`, `clean -ffdx`), and checks whether `HEAD` already
has a `cycle` event for this cycle id. If it does, the attempt returns that result instead of
working another item.

## What happens when a worker crashes

- **Completed cycles are not re-run.** Their results are in the workflow history; on replay the
  new worker reads them back.
- **The in-flight attempt is lost.** Temporal notices the missing heartbeat within 2 minutes and
  schedules a retry on any worker polling the queue. The retry discards the partial edits (reset to
  `HEAD`), so nothing half-done is committed. Model calls of the lost attempt are paid again.
- **A commit that landed before the crash is not repeated.** The retry finds the cycle's event at
  `HEAD` and returns it (the exactly-once check above).
- **Spend of the lost attempt still counts.** Every attempt appends its spend to
  `<workdir>/.git/lha/spend.ndjson` (outside the worktree, so the reset keeps it); the next attempt
  seeds its budget ledger from that file.

These properties are tested in
[`tests/durability/test_durable_spine.py`](../python/tests/durability/test_durable_spine.py)
(`test_crash_after_commit_is_idempotent`, `test_crash_before_commit_discards_residue`).

If a cycle fails 5 times with retryable errors (an outage), the mission parks: status
`DEGRADED_PARK`, a durable timer of 60 s doubling to at most 3600 s, and a `check_mission_health`
probe after each sleep. See [15-operations-runbook.md](15-operations-runbook.md) for what the
probe checks and what ends a mission.
