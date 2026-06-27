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
| `DEGRADED_PARK` | a cycle failed 5 attempts with retryable errors; the workflow sleeps and probes health | read the `park_reason` query; fix the dependency |
| `WAITING_ON_HUMAN` | a human gate is open: an irreversible action awaits approval, or the mission is deadlocked | run `lha mission-status` to see the question, then `lha mission-approve` with `approve`/`reject` or `retry`/`abort` |
| `DONE` | every item verified done | nothing |
| `IMPOSSIBLE` | deadlocked: items remain, none actionable | inspect blocked items; start a new mission |
| `ABORTED` | budget exhausted, `max_cycles` reached, or a non-retryable failure | see below |

`SLEEPING` appears in the schema comment and in `durable/signals.py` but is never set. The
`missions` table in Postgres is not written by the workflow; the status lives in the workflow.

Local runs (`run-local`, `mission`, `orchestrate`) have no status query. They print a summary and
exit: `stopped_reason` is `complete`, `deadlocked: <reason>`, `governor: <reason>`,
`loop on item <id>` or `max_cycles`.

## Observe a mission

- **Committed truth**: `git -C <workdir> log --oneline`, `<workdir>/.lha/progress.md`,
  `<workdir>/.lha/checklist.json`. Each cycle is one commit: `lha: complete <id> (...)`,
  `lha: attempt <id> (...)`, `lha: block <id> (...)` or `lha: split <id> (...)` (the item was
  blocked and replaced by children `<id>.1`, `<id>.2`, ...).
- **Workflow**: the Temporal UI (<http://localhost:8080>) shows every activity, attempt, failure
  and timer. `lha mission-status` returns status, cycle count and, when a gate is open, the
  question it is waiting on.
- **Spend**: `<workdir>/.git/lha/spend.ndjson` holds one row per cycle attempt (`usd`, `unknown`,
  `calls`). Each `run_agent_cycle` result in the history carries `spent_usd` for that cycle.
- **Langfuse**: no LHA code sends data to Langfuse (see [16-observability.md](16-observability.md)).

## How a mission ends

| Outcome | Status | Cause |
|---|---|---|
| `completed` | `DONE` | every checklist item is `done` (split items count through their children) |
| `deadlocked` | `IMPOSSIBLE` | nothing actionable (blocked items, unsatisfiable dependencies) and no `retry` from a human within the deadlock gate (24 h by default) |
| `budget_exhausted` | `ABORTED` | the governor refused a model call (`BudgetExceeded`, non-retryable) |
| `max_cycles` | `ABORTED` | `cycles_done >= max_cycles` (`--max-cycles`, default `LHA_MAX_CYCLES`, 1000) |
| workflow failure | `ABORTED` | an activity raised a non-retryable `MissionConfigError`: empty check list, bad model config, sandbox refused, malformed `LHA_TRUSTED_CHECKS` or `LHA_SANDBOX_EGRESS` |

An unpriced `openai_compat` model without `LHA_ALLOW_UNPRICED_MODELS=true` is refused by the
governor on the first call, so the mission ends as `budget_exhausted`. Invalid `MissionInput`
values (an empty `check_commands`, `cycles_before_can < 1`, `max_cycles < 0`) fail the workflow
before the first cycle.

An item becomes `blocked` after 3 consecutive failed verifications (the `AgentLoop` default; not
configurable through settings). The replanner then tries to split it into smaller items
(at most `LHA_MAX_REPLANS` splits per mission, nested at most `LHA_MAX_SPLIT_DEPTH` levels).
Blocked items are skipped so independent items continue; when only blocked items remain the
mission is deadlocked and the deadlock gate opens.

There is no command to resume a mission whose workflow has closed. `lha mission-start` plans (or
imports) and initializes a new anchor.

## Common actions

**Approve or reject an irreversible action.** When the agent tries a gated command (for example
`git push`, a publish, an upload), the durable cycle queues it and the workflow waits as
`WAITING_ON_HUMAN`. `lha mission-status <id>` prints the tool, its exact arguments and the reason.
Check what would be sent (for example `git -C <workdir> log origin/main..HEAD`), then run
`lha mission-approve <id> --decision approve` or `--decision reject`. No cycles run while the
gate is open. An approved call is allowed once, in the next cycles, only with the same arguments;
if the agent changes them, it is asked again. A rejected call is not asked about again. With no
answer within `--approval-timeout-hours` (default `LHA_APPROVAL_TIMEOUT_S`, 24h) the action is rejected. Several queued
actions are asked one at a time.

**Resolve the deadlock gate.** `lha mission-start` opens it for `--deadlock-gate-hours` (default
24; `0` turns it off). While open, `lha mission-approve <id> --decision retry` runs
`unblock_items` (every `blocked` item back to retryable, committed as
`lha: unblock ... (human retry)`) and the mission continues; `abort`, or the timeout, ends it as
`deadlocked`. Before retrying, read `last_failure` of the blocked items in
`<workdir>/.lha/checklist.json`: if the check cannot pass as configured (a missing tool in the
sandbox image, a host not in `LHA_SANDBOX_EGRESS`, an undefined `trusted:` check), fix the worker
settings and restart the worker first, or the items block again.

**Steer.** Send `steer_v1` with a note (`temporal workflow signal --workflow-id mission:<id>
--name steer_v1 --input '"..."'`). Following cycles include it in the prompt.

**Stop.** `lha mission-abort <id>` cancels the workflow (see
[14-running-on-temporal.md](14-running-on-temporal.md#5-gates-and-abort)).

**Change the budget ceiling.** A CLI-started mission uses the worker's `LHA_BUDGET_USD_CEILING`,
read once per worker process and applied per cycle attempt. Change it and restart the worker; the
next attempt uses the new ceiling, seeded with the spend already in `.git/lha/spend.ndjson`. This
only helps before the mission ends: once a call is refused the outcome is `budget_exhausted` and
the workflow closes.

**Change the model or sandbox.** Same as the budget: worker settings (`LHA_MODEL_*`,
`LHA_SANDBOX`, `LHA_SANDBOX_IMAGE`), applied after a restart.

**Allow an egress domain.** Two settings, both read by the worker:

- `LHA_SANDBOX_EGRESS`: hosts the sandbox's commands may reach through the egress proxy (package
  registries). A host alone allows ports 80 and 443; `.example.org` allows the domain and its
  subdomains; `host:port` allows another port.
- `LHA_WEB_ALLOW_HOSTS`: hosts the lead's `fetch_url` may read. Empty means no `fetch_url`.

For reference material that does not change, prefer `lha vendor` once over opening the network
to every cycle.

**Egress incidents.** A check that fails with a download error (`403` from the proxy, `CONNECT
tunnel failed`, a name that does not resolve) usually means a missing host. While the session is
open, `docker logs lha-egress-proxy-<id>` shows one allowed/denied line per request; the
containers and networks are labelled `lha.egress`. Add the host and restart the worker. If a
worker died without closing its session, remove leftovers with
`docker ps -a --filter label=lha.egress` and `docker network ls --filter label=lha.egress`. If
the agent reached something it should not have, remove the host from the allow-list; the proxy
never allows IP literals or hosts that resolve to private, loopback or link-local addresses.

**Protect files the gate depends on.** Set `LHA_HARNESS_PATHS` (for example
`Makefile,e2e/**,.github/**`) on the worker so edits to them fail the `harness_integrity` check
and are reverted. Do this for anything a `trusted:` check runs.

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
- `ops/lifecycle.py` (`should_declare_impossible`, `MissionOutcome`) and the
  `AutoPolicyGate` class in `hitl/gate.py` are library code with no caller (`CallbackGate`
  backs the local `--approve-interactive` prompt). **Planned**: a "declare impossible?" gate,
  gates with an escalation ladder, and persisting gates to the `hitl_gates` table. Today the
  durable gates are the action-approval gate (default `reject`) and the deadlock gate (default
  `abort`), both with a timeout.

## Safe deploys during an in-flight mission

Workflow code must replay recorded histories deterministically, or a running mission breaks when
a new worker picks it up. The guard:

- [`durable/replay_test_harness.py`](../python/src/lha/durable/replay_test_harness.py)
  replays every `*.json` history in a directory against the current `MissionWorkflow` and
  `SubAgentWorkflow`, using the same ClaimCheck data converter as the worker.
- [`tests/durability/test_replay.py`](../python/tests/durability/test_replay.py) replays the
  committed history `tests/durability/histories/mission_three_items.json`, and checks that a
  tampered history fails. CI runs it on every push and pull request.

Before deploying a workflow change:

1. Run `uv run pytest -q tests/durability/test_replay.py`.
2. To check against a real mission, export its history (Temporal UI download, or
   `temporal workflow show --workflow-id mission:<id> --output json > h/mission.json`) and call
   `replay_histories("h", object_store_root=...)` with the object store the worker used.
3. If replay fails, do not deploy that change to workers serving in-flight missions.

**Planned**: worker Build IDs / worker versioning. `build_worker` sets no Build ID, so every worker
on the queue can pick up any mission; mixed old and new workers are not separated.

When a change is intentionally incompatible, re-record the committed history with
`LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py`.

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
