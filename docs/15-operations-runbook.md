# Operations runbook

How to observe, steer and recover a running mission, as the code behaves today. Items marked
**planned** are designed (and sometimes present as library code) but not wired into any command
or workflow. Setup is in [14-running-on-temporal.md](14-running-on-temporal.md).

## Mission statuses

The durable `MissionWorkflow` exposes its status through the `status_v1` query
(`lha mission-status <id>`):

| Status | Meaning | What to do |
|---|---|---|
| `RUNNING` | a cycle is being dispatched or is running | nothing |
| `SLEEPING` | on a durable timer by design: scheduled start, pause between cycles, or a snooze | nothing; `lha mission-snooze <id> --seconds 0` wakes it |
| `DEGRADED_PARK` | a cycle failed 5 attempts with retryable errors; the workflow sleeps and probes health | read the `park_reason` query; fix the dependency |
| `WAITING_ON_HUMAN` | a gate is open: an irreversible action waiting for approval, or the deadlock gate | `lha mission-status <id>` shows it; answer with `lha mission-approve` |
| `DONE` | every item verified done | nothing |
| `IMPOSSIBLE` | deadlocked with no gate, or declared impossible at the deadlock gate | inspect blocked items; start a new mission |
| `ABORTED` | budget exhausted, `max_cycles` reached, "abort" at the deadlock gate, or a non-retryable failure | see below |

The `missions` table in Postgres is not written by the workflow; the status lives in the workflow.

Local runs (`run-local`, `mission`, `orchestrate`) have no status query. They print a summary and
exit: `stopped_reason` is `complete`, `deadlocked: <reason>`, `governor: <reason>`,
`loop on item <id>` or `max_cycles`.

## Observe a mission

- **Committed truth**: `git -C <workdir> log --oneline`, `<workdir>/.lha/progress.md`,
  `<workdir>/.lha/checklist.json`. Each cycle is one commit: `lha: complete <id> (...)`,
  `lha: attempt <id> (...)` or `lha: block <id> (...)`.
- **Workflow**: the Temporal UI (<http://localhost:8080>) shows every activity, attempt, failure
  and timer. `lha mission-status` returns status, cycle count, the open gate (with a queued
  action and its reason), the sleep deadline and the last gate events.
- **Gate events**: `<workdir>/.lha/events.ndjson` holds `tool_approval` (every answer to a flagged
  command: `pending`, `approve`, `reject`, with who decided and whether it was a default),
  `gate_opened`, `gate_reminder`, `gate_resolved`, `gate_defaulted` and `mission_impossible`
  events. Set `LHA_GATE_WEBHOOK_URL` on the workers to receive the gate events as JSON POSTs.
- **Spend**: `<workdir>/.git/lha/spend.ndjson` holds one row per cycle attempt (`usd`, `unknown`,
  `calls`). Each `run_agent_cycle` result in the history carries `spent_usd` for that cycle.
- **Langfuse**: no LHA code sends data to Langfuse (see [16-observability.md](16-observability.md)).

## How a mission ends

| Outcome | Status | Cause |
|---|---|---|
| `completed` | `DONE` | every checklist item is `done` |
| `deadlocked` | `IMPOSSIBLE` | nothing actionable (blocked items, unsatisfiable dependencies) and no deadlock gate |
| `impossible` | `IMPOSSIBLE` | "impossible" at the deadlock gate (human or default); final checkpoint `lha: mission declared impossible` |
| `aborted` | `ABORTED` | "abort" at the deadlock gate (human or the default) |
| `budget_exhausted` | `ABORTED` | the governor refused a model call (`BudgetExceeded`, non-retryable) |
| `max_cycles` | `ABORTED` | `cycles_done >= max_cycles` (1000 for CLI-started missions) |
| workflow failure | `ABORTED` | an activity raised a non-retryable `MissionConfigError`: empty check list, bad model config, sandbox refused |

An unpriced `openai_compat` model without `LHA_ALLOW_UNPRICED_MODELS=true` is refused by the
governor on the first call, so the mission ends as `budget_exhausted`. Invalid `MissionInput`
values (an empty `check_commands`, `cycles_before_can < 1`, `max_cycles < 0`) fail the workflow
before the first cycle.

An item becomes `blocked` after 3 consecutive failed verifications (the `AgentLoop` default; not
configurable through settings). Blocked items are skipped so independent items continue; when
only blocked items remain the mission is deadlocked.

There is no command to resume a mission whose workflow has closed. `lha mission-start` plans and
initializes a new anchor.

## Common actions

**Approve or reject an irreversible action.** `lha mission-status <id>` shows the queued action
(tool, arguments, reason, fingerprint, deadline). `lha mission-approve <id> --decision approve`
allows that exact action once in a later cycle; `reject`, or no answer before the approval timeout
(`mission-start --approval-timeout-hours`, default 24), rejects it and it is never asked about
again. In a local run with `--approve-interactive` the same question is asked on the terminal
(`y/N`, default reject; rejected without asking when stdin is not a TTY).

**Resolve the deadlock gate.** The gate opens when the mission was started with
`--deadlock-gate-hours` > 0 (the default is 24). While open,
`lha mission-approve <id> --decision retry` runs `unblock_items` (every `blocked` item back to
retryable, committed as `lha: unblock ... (human retry)`) and the mission continues; `abort` ends
it `ABORTED`; `impossible` writes a final checkpoint and ends it `IMPOSSIBLE`. On timeout the
`--deadlock-default` applies (`abort`, or `impossible`). `mission-approve` refuses a decision the
open gate does not offer.

**Snooze.** `lha mission-snooze <id> --seconds N` parks the mission (status `SLEEPING`) before its
next cycle; `--seconds 0` wakes it. `--cycle-pause-seconds` and `--start-in-seconds` at
`mission-start` do the same on a schedule.

**Steer.** Send `steer_v1` with a note (`temporal workflow signal --workflow-id mission:<id>
--name steer_v1 --input '"..."'`). Following cycles include it in the prompt.

**Stop.** `lha mission-abort <id>` cancels the workflow (see
[14-running-on-temporal.md](14-running-on-temporal.md#5-gates-sleep-and-abort)).

**Change the budget ceiling.** A CLI-started mission uses the worker's `LHA_BUDGET_USD_CEILING`,
read once per worker process and applied per cycle attempt. Change it and restart the worker; the
next attempt uses the new ceiling, seeded with the spend already in `.git/lha/spend.ndjson`. This
only helps before the mission ends: once a call is refused the outcome is `budget_exhausted` and
the workflow closes.

**Change the model or sandbox.** Same as the budget: worker settings, applied after a restart.

**Allow an egress domain.** There is no setting for the egress allow-list. The web tools
(`fetch_url`, `web_search`) deny every request unless code constructs them with an
`EgressPolicy`, and the Docker sandbox runs with `network_mode="none"`. **Planned**: an
operator-configurable allow-list.

## Degradation modes

[`ops/degradation.py`](../python/src/lha/ops/degradation.py) classifies dependencies:

| Class | Dependencies | Decision when DOWN |
|---|---|---|
| critical | `git`, `model`, `sandbox` | park |
| optional | `pgvector`, `langfuse`, `egress_proxy` | continue, listed as degraded |

What actually happens, by failure:

| Failure | Behaviour |
|---|---|
| Model API transient error (429, 5xx, timeout) | the provider retries 3 times with backoff; then the cycle attempt fails and Temporal retries it (5 attempts); then the mission parks |
| Model API 400/401/403 | not retried by the provider; the attempt fails and Temporal retries it like any other error; after 5 attempts the mission parks |
| Model misconfigured (missing key or base URL, unpriced Claude model) | `MissionConfigError`, non-retryable: the workflow fails |
| Sandbox cannot be opened (Docker down) | configuration-type errors (`UnsafeSandboxError`, `ValueError`) fail the workflow; other errors are retried, then park |
| Worker process dies | see [Recovery](#recovery) |

While parked, the workflow sleeps 60 s, doubling to at most 3600 s, and after each sleep runs
`check_mission_health` (one attempt, 2-minute timeout). The probe checks:

- `git`: the workdir is a repository with at least one commit;
- `model`: `build_provider` succeeds (configuration only; no request is sent to the model);
- `sandbox`: a sandbox session can be opened and closed.

The mission resumes when no critical dependency is DOWN. Because the model probe does not contact
the provider, an API outage passes the probe; the mission then resumes, and if the outage persists
it fails 5 more attempts and parks again with the delay reset to 60 s. The probe never reports
the optional dependencies, so the optional rows of the table have no runtime effect.

**Not implemented** (claimed by earlier docs): falling back to lexical search when pgvector is
down (no runtime path uses pgvector), buffering spans when Langfuse is down (nothing is sent), a
`fallback_model` setting (see `FailoverModel` in [13-models.md](13-models.md), library-only), and
alerts on park.

## Stuck items and gates

- A failing item is retried each cycle, blocked after 3 consecutive failures, and the mission
  moves on. The local runners also stop when one item fails `LHA_STALL_LIMIT` consecutive times
  (`loop on item <id>`); with the default of 5 the item is blocked first, so this fires only when
  the limit is 3 or less. The durable workflow has no such detector.
- Every gate has a timeout, a default and an escalation ladder: reminders at
  `LHA_GATE_ESCALATION_SECONDS` (default 15 min, 45 min, 4 h and 12 h after it opens, skipping
  offsets past the timeout), each committed as a `gate_reminder` event, shown by `mission-status`
  and POSTed to the optional webhook; then the default. Tool-call gates default to **reject**; the
  deadlock gate to `abort` (or `impossible`).
- The deadlock gate recommends "impossible" when `ops.lifecycle.should_declare_impossible` says
  the blocked item failed `LHA_IMPOSSIBLE_AFTER_FAILURES` (3) cycles in a row.
- `MissionOutcome` in `ops/lifecycle.py` and the `AutoPolicyGate` / `CallbackGate` classes in
  `hitl/gate.py` have no caller in a run path. **Planned**: persisting gates to the `hitl_gates`
  table.

## Safe deploys during an in-flight mission

Workflow code must replay recorded histories deterministically, or a running mission breaks when
a new worker picks it up. The guard:

- [`durable/replay_test_harness.py`](../python/src/lha/durable/replay_test_harness.py)
  replays every `*.json` history in a directory against the current `MissionWorkflow` and
  `SubAgentWorkflow`, using the same ClaimCheck data converter as the worker.
- [`tests/durability/test_replay.py`](../python/tests/durability/test_replay.py) replays the
  committed histories in `tests/durability/histories/` (a three-item mission, a deadlock gate and
  an approval gate recorded with the pre-ladder code, and an approval on the escalation ladder),
  and checks that a tampered history fails. CI runs it on every push and pull request.
- New workflow behaviour is guarded by `workflow.patched(...)` (see
  [08-durable-execution.md](08-durable-execution.md#replay-safety-net)), so a mission started by
  an older worker keeps its old command sequence after a deploy.

Before deploying a workflow change:

1. Run `uv run pytest -q tests/durability/test_replay.py`.
2. To check against a real mission, export its history (Temporal UI download, or
   `temporal workflow show --workflow-id mission:<id> --output json > h/mission.json`) and call
   `replay_histories("h", object_store_root=...)` with the object store the worker used.
3. If replay fails, do not deploy that change to workers serving in-flight missions.

**Planned**: worker Build IDs / worker versioning. `build_worker` sets no Build ID, so every worker
on the queue can pick up any mission; mixed old and new workers are not separated.

When a change is intentionally incompatible, re-record one history with
`LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py -k <test>` and keep the older
histories replaying.

## Recovery

**Worker or host crash.** Restart `lha worker` (any worker polling `LHA_TASK_QUEUE` works). Temporal
replays the history: completed cycles are not re-run; the in-flight attempt times out on its
heartbeat (2 minutes) and is retried from a clean checkout. A commit made just before the crash
is detected and not repeated. The worker needs the same `LHA_OBJECT_STORE_ROOT` contents as
before, or offloaded payloads cannot be decoded.

**Stale workdir lock.** The lock is an `flock` held by a live process; it is released when the
process dies. An attempt waits up to 300 s for it, then fails with `WorkdirBusyError` (retryable).

**Temporal server restart.** The server keeps state in `temporal-db`; workflows and timers resume
when it is back. Workers reconnect.

**Postgres schema.** `lha db migrate` applies pending `db/migrations/*.sql` (see
[17-cli.md](17-cli.md#lha-db-migrate)). The durable path does not need Postgres.

**Re-embedding memory.** **Planned.** Every `semantic_memory` row records `embedding_model` and
`embedding_version`, but there is no re-embed command and no runtime path writes the table.

**Orphaned sub-agent branches.** `durable/reconcile.py` (`reconcile_in_flight`: adopt a ticket
branch with commits, re-spawn one that is missing or empty) and `durable/saga.py` (LIFO
compensations) are tested library code; `MissionWorkflow` calls neither. **Planned.**
