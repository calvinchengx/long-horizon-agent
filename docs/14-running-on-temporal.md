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
Only `temporal` (and its database) is required by the durable path. The workflow itself never
touches a database; the activities write mission rows, the cost ledger and tiered memory to
`appdb` only when the workers have `LHA_POSTGRES_DSN` set, and otherwise to a local SQLite file
(see [18-configuration.md](18-configuration.md)). Workers send traces to Langfuse only when
the `LHA_LANGFUSE_*` settings are set and the `observability` extra is installed (see
[16-observability.md](16-observability.md)). None of these
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
| Activities | `run_agent_cycle`, `check_mission_health`, `unblock_items`, `read_mission_snapshot`, `notify_gate`, `declare_impossible`, `run_subagent` |

It runs until interrupted. It has no options; everything comes from `LHA_*` settings of the worker
process. The model backend, sandbox (`LHA_SANDBOX`, default `docker`), sandbox image and egress
allow-list, `LHA_WEB_ALLOW_HOSTS`, `LHA_TRUSTED_CHECKS`, `LHA_HARNESS_PATHS`, replanning limits,
budget ceiling and `max_turns_per_cycle` used by a mission are the **worker's**, not the settings
of the process that started the mission. Trusted checks run on the worker's host. Settings are read once per process, so a change takes effect after a worker
restart. If the server is unreachable, the command exits with a Python traceback.

[`python/Dockerfile`](../python/Dockerfile) builds a worker image whose default command is
`lha worker`. Do not mount the host Docker socket into it; point `DOCKER_HOST` at a separate
daemon or use `LHA_SANDBOX=e2b`.

## 3. Start a mission

```bash
cd python
uv run lha mission-start --task "Add a slugify() helper with tests" \
  --workdir .lha/workspaces/slugify
# started mission mission_3f9a1c0b2d4e (workflow id: mission:mission_3f9a1c0b2d4e)

# or from your own checklist, with vendored references
uv run lha mission-start --checklist roadmap.md --reference reference/api.md.txt \
  --workdir /srv/missions/emulator --deadlock-gate-hours 48
```

`lha mission-start`, in order:

1. With `--task`, plans the task into a checklist with the configured model (one metered call
   against `LHA_BUDGET_USD_CEILING` in this process). With `--checklist FILE`, imports the file
   instead ([importing a checklist](06-mission-anchor.md#importing-a-checklist)); one of the two
   is required.
2. Initializes the git mission anchor at `--workdir`, including any `--reference` paths (see
   [06-mission-anchor.md](06-mission-anchor.md)).
3. Writes the mission row (status `RUNNING`, or `SLEEPING` with `--start-in-seconds`) and the
   Planner's spend to the mission store; if the workflow then cannot be started, the row is set
   to `ABORTED`.
4. Starts `MissionWorkflow` with id `mission:<mission_id>` on `LHA_TASK_QUEUE`, passing a
   `MissionInput` with the mission id, the workdir, the check commands and the values below.

It prints the id and returns; it does not wait for the mission.

| `MissionInput` field | CLI option | Else setting (default) |
|---|---|---|
| `max_cycles` | `--max-cycles` | `LHA_MAX_CYCLES` (1000) |
| `deadlock_gate_seconds` | `--deadlock-gate-hours` (x 3600) | 24 hours; `0` = no gate |
| `deadlock_gate_default` | `--deadlock-default abort\|impossible` | `LHA_DEADLOCK_GATE_DEFAULT` (`abort`) |
| `approval_timeout_seconds` | `--approval-timeout-hours` (x 3600) | `LHA_APPROVAL_TIMEOUT_S` (86400) |
| `cycle_pause_seconds` | `--cycle-pause-seconds` | `LHA_CYCLE_PAUSE_SECONDS` (0) |
| `resume_at` | `--start-in-seconds N` (now + N) | 0 (start now) |
| `gate_escalation_seconds` | none | `LHA_GATE_ESCALATION_SECONDS` (`[900, 2700, 14400, 43200]`) |
| `impossible_after_failures` | none | `LHA_IMPOSSIBLE_AFTER_FAILURES` (3) |

These values are read by the process that runs `mission-start` and travel in the workflow input,
so they are fixed per mission. `LHA_GATE_WEBHOOK_URL` is different: the worker's `notify_gate`
activity reads it, so set it on the workers.

The workflow's other inputs keep their defaults: `cycles_before_can=200`, `budget_usd=None` (the
worker's ceiling), `park_initial_seconds=60`, `park_max_seconds=3600`. There is no `--sandbox` or
`--allow-host` option: the worker's `LHA_SANDBOX` and `LHA_WEB_ALLOW_HOSTS` apply.

`--workdir` is resolved to an absolute path in the CLI process before it is sent, so a relative
path means relative to where you ran `mission-start`. The worker must be able to reach the same
filesystem path.

## 4. Watch it

```bash
uv run lha mission-status mission_3f9a1c0b2d4e
# status=RUNNING cycles=3

uv run lha mission-status mission_3f9a1c0b2d4e
# status=WAITING_ON_HUMAN cycles=3
# gate: tool_call approval-5d41402abc4b
#   question: Mission mission_3f9a1c0b2d4e wants to run an irreversible action: ...
#   options: approve | reject  (default on timeout: reject)
#   opened: 2026-09-24T10:00:00+00:00  deadline: 2026-09-25T10:00:00+00:00
#   reminders sent: 1  next reminder: 2026-09-24T10:45:00+00:00
#   pending action: run_command {'argv': ['git', 'push', 'origin', 'main']}
#   reason: git push (outward-facing / rewrites history)
#   fingerprint: 5d41402abc4b2a76b9719d911017c592
# recent gate events:
#   2026-09-24T10:00:00+00:00 tool_call gate approval-5d41402abc4b opened (default reject)
#   2026-09-24T10:15:00+00:00 tool_call gate approval-5d41402abc4b: reminder 1 (escalation)
```

`mission-status` queries `status_v1` and `cycles_done`, then `gate_v1` (the open gate, with the
pending action and its reason), `resume_at` (printed as "sleeping until ..." when set) and
`gate_log_v1` (the last 8 gate / sleep events). For a workflow served by an older worker that
does not answer `gate_v1`, it falls back to `open_question` (printed as `waiting on: ...`).
Queries work while the workflow runs and after it has closed, but a worker must be polling the
task queue to answer them. The workflow also answers `last_item`, `park_reason` and
`rejected_decisions`, which the CLI does not expose; use the Temporal UI or
`temporal workflow query`.

Statuses the workflow sets:

| Status | When |
|---|---|
| `RUNNING` | dispatching or running a cycle |
| `SLEEPING` | on a durable timer by design: a scheduled start (`--start-in-seconds`), the pause between cycles (`--cycle-pause-seconds`) or `lha mission-snooze` |
| `DEGRADED_PARK` | a cycle exhausted its retries; sleeping and probing health |
| `WAITING_ON_HUMAN` | a gate is open: an irreversible action waiting for approval, or the deadlock gate (`deadlock_gate_seconds > 0`) |
| `DONE` | every item verified done |
| `IMPOSSIBLE` | deadlocked with no deadlock gate, or declared impossible at the deadlock gate |
| `ABORTED` | budget exhausted, `max_cycles` reached, "abort" at the deadlock gate, or a non-retryable failure |

These are the workflow's `status_v1` values. The mission row that `lha missions` lists follows
them: the cycle activity writes what a cycle sees (`RUNNING`, also for a deadlocked
checklist, `DONE`, `WAITING_ON_HUMAN` for a queued approval, `ABORTED` on budget), and the
workflow writes the rest through the `record_mission_status` activity: `SLEEPING`,
`DEGRADED_PARK`, `WAITING_ON_HUMAN` when a gate opens, and the final status of every ending
(`IMPOSSIBLE` for a deadlock, a deadlock-gate decision, `max_cycles`, a failure,
`mission-abort`). A row in `DONE`, `IMPOSSIBLE` or `ABORTED` never goes back to a non-terminal
status. Those writes are best effort (30 s, three attempts); if the store is down the mission
goes on and the row lags until the next write. Gate events are written to `hitl_gates` by the
`notify_gate` activity (`lha gates`).

The Temporal UI at <http://localhost:8080> shows each workflow's event history: every activity
with its input, result, attempts and failures, the timers of a park, and each Continue-As-New.
Large payloads appear as ClaimCheck pointers (see [19-wire-contract.md](19-wire-contract.md)).
The committed work is in git: `git -C <workdir> log --oneline` and `<workdir>/.lha/progress.md`.

## 5. Gates, sleep and abort

```bash
uv run lha mission-approve mission_3f9a1c0b2d4e --decision approve      # a queued action
uv run lha mission-approve mission_3f9a1c0b2d4e --decision impossible   # the deadlock gate
uv run lha mission-snooze  mission_3f9a1c0b2d4e --seconds 3600          # 0 wakes it
uv run lha mission-abort   mission_3f9a1c0b2d4e
```

`MissionWorkflow` opens two kinds of gate
([08-durable-execution.md](08-durable-execution.md#human-gates)).

**Irreversible actions.** When the agent runs a command the classifier flags (for example
`git push`), it is refused for now and queued. After the cycle the workflow opens a `tool_call`
gate: status `WAITING_ON_HUMAN`, the action and its reason in `mission-status`. `approve` allows
that exact action once in a later cycle; `reject`, or no answer within the approval timeout
(`--approval-timeout-hours`, default 24), rejects it for good.

**Deadlock gate.** With `--deadlock-gate-hours` > 0 (default 24), a deadlock opens a gate offering
`retry` (unblock the blocked items and continue), `abort` (status `ABORTED`) and `impossible` (a
final `lha: mission declared impossible` checkpoint, status `IMPOSSIBLE`). The default on timeout
is `--deadlock-default` (`abort` unless set to `impossible`). When the blocked item failed
`LHA_IMPOSSIBLE_AFTER_FAILURES` (3) cycles in a row the gate recommends `impossible`. With
`--deadlock-gate-hours 0`, a deadlock ends the mission immediately with status `IMPOSSIBLE`.

`mission-approve` sends the `human_decision_v1` signal. It checks `--decision` against the open
gate first: a decision the gate does not offer (for example `approve` at the deadlock gate) is
refused with exit 2 and nothing is sent. With no gate open, any of `approve`, `reject`, `retry`,
`abort`, `impossible` is sent and held until the next gate; a held decision the gate does not offer
is discarded (recorded in `rejected_decisions`) and the gate keeps waiting.

**Escalation ladder.** Every gate sends reminders at `LHA_GATE_ESCALATION_SECONDS` after it opens
(default 15 minutes, 45 minutes, 4 hours and 12 hours; offsets past the gate's timeout are
skipped), then applies its default at the timeout. Each step is committed to the mission anchor
as a `gate_opened` / `gate_reminder` / `gate_resolved` / `gate_defaulted` event, listed by
`mission-status`, and POSTed as JSON to `LHA_GATE_WEBHOOK_URL` if the workers have it set.

**Sleeping.** `mission-snooze --seconds N` parks the mission on a durable timer (status
`SLEEPING`) before its next cycle; `--seconds 0` wakes it. A cycle already running finishes first.

`mission-abort` requests cancellation of the workflow. Temporal delivers it to a running
`run_agent_cycle` with the response to a heartbeat (the activity heartbeats every 5 s; the SDK
sends one to the server at most every 60 s by default); a cancelled cycle never parks the mission.
The workflow waits until the cycle has acknowledged the cancellation (or finished anyway: that
does not undo the abort), then sets its status to `ABORTED`, writes `ABORTED` to the mission row
(`record_mission_status`) and closes as *Cancelled*. Because the cycle's last row write lands
first, and the store never moves a row out of `ABORTED`, `lha missions` ends at `ABORTED`. Work
of the interrupted cycle is not committed; uncommitted files may remain in the working tree.
Workflows started before this behaviour (`workflow.patched("lha-cycle-wait-cancel-v1")`) keep the
old one: they write `ABORTED` without waiting, and only the store's rule protects the row.

The `steer_v1` signal appends an operator note (at most 2000 characters; the last 20 are kept)
that every following cycle's prompt includes. No CLI command sends it; use `temporal workflow
signal --name steer_v1`.

## How a cycle runs

`MissionWorkflow` loops: sleep while a scheduled start, pause or snooze is pending (`SLEEPING`),
run one `run_agent_cycle` activity, absorb its result, open any human gates, stop on a terminal
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
