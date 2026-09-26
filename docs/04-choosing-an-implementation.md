# Choosing an implementation

LHA has two implementations of the same system: Python in [`python/`](../python/) and Go in
[`go/`](../go/). Python is the reference implementation and the only complete one.
The Go CLI runs single-agent missions locally (`lha run-local`, `lha mission`) in the local or
Docker sandbox, and durably on Temporal (`lha worker`, `lha mission-start` and the other
`mission-*` commands) for single-agent missions. The multi-agent organization (`lha orchestrate`,
and `mission-start --research / --review / --max-parallel`), persistence and memory, the
`claude_code` engine and E2B are Python-only.

## What the two share

The design goal is that the two are interchangeable at every boundary an operator or another
process can observe:

| Surface | Shared definition |
|---|---|
| CLI | The `lha` commands and flags (see [CLI](17-cli.md)). The Go CLI has `version`, `config`, `run-local`, `mission`, `decisions`, `worker`, `mission-start`, `mission-status`, `mission-approve`, `mission-snooze` and `mission-abort`. |
| Settings | The `LHA_*` environment variables and `.env` file, with the same names and defaults ([`python/src/lha/config.py`](../python/src/lha/config.py), [`go/internal/config/`](../go/internal/config/)) |
| Mission anchor | The `.lha/` files and their JSON shapes ([the mission anchor](06-mission-anchor.md)) |
| Postgres schema | [`db/migrations/`](../db/migrations/) |
| Temporal contract | Workflow, activity, signal and query names, the task queue, and the JSON payload shapes ([wire contract](19-wire-contract.md)) |
| Behaviour | The language-neutral cases in [`spec/`](../spec/), which both test suites load |

The Temporal names the Python worker registers are:

| Kind | Names |
|---|---|
| Workflows | `MissionWorkflow`, `SubAgentWorkflow` |
| Activities | `run_agent_cycle`, `check_mission_health`, `notify_gate`, `declare_impossible`, `unblock_items`, `read_mission_snapshot`, `record_mission_status`, `run_subagent`, `plan_round`, `run_implementer`, `integrate_branch`, `review_cycle` |
| Signals | `human_decision_v1`, `steer_v1`, `snooze_v1` |
| Queries | `status_v1`, `gate_v1`, `gate_log_v1`, `cycles_done`, `last_item`, `park_reason`, `resume_at`, `open_question`, `rejected_decisions` |
| Task queue / workflow id | `LHA_TASK_QUEUE` (default `lha-mission`) / `mission:<mission_id>` |

Payloads are the dataclasses in [`python/src/lha/durable/types.py`](../python/src/lha/durable/types.py),
serialized by Temporal's default JSON converter plus a claim-check codec that moves large
payloads to the object store. The Go worker ([`go/internal/durable/`](../go/internal/durable/))
registers the same workflows and activities under the same names; its `run_subagent` refuses with
`MissionConfigError` and it has no organization activities (see below).

## Wire compatibility

"Wire-compatible" means that a Go process and a Python process can operate on the same
artifacts: read and write the same `.lha/` anchor, the same database, and the same Temporal task
queue. The Go state package has cross-implementation tests
([`go/internal/state/crossimpl_test.go`](../go/internal/state/crossimpl_test.go)) in which Go
writes an anchor that Python reads back identically, and the reverse.

`spec/` pins behaviour that must match exactly: which commands need human approval, which URLs
may be fetched, redaction, checklist transitions, check naming, the decision-log hash chain,
shared-path ownership, protected harness files and model pricing. Python is the reference: a
behaviour change is made in Python, the cases are regenerated with
`cd python && uv run python scripts/export_spec.py`, and Go is then made to pass them. See
[`spec/README.md`](../spec/README.md).

## Status of the Go port

The port proceeds in phases: first the single-agent spine (safety, sandboxes, git anchor,
verifier, model backends, agent loop, CLI), then Temporal, then the multi-agent organization,
memory and Postgres. Current state of [`go/internal/`](../go/internal/):

| Package | Mirrors | State |
|---|---|---|
| `contracts` | `lha.contracts` | Committed, including item `witnesses`, the `split` status and `Checklist.Split`, `MissionSpec.References` and `Check.Where` |
| `config` | `lha.config` | Committed: every Python setting, with the same names, defaults and validation; `lha config` output is byte-identical |
| `spec` | conformance harness | Committed: runs every file in `spec/`, including `agent/prompts.json` and `coordination/shared_paths.json` |
| `model` | `lha.model` (stub, OpenAI-compatible/Ollama, Claude, failover, retry, pricing, health probe) | Committed, including the health probe a parked durable mission uses (`ProbeModel`). `BuildProvider` does not build a fallback chain from settings |
| `safety` | `lha.safety` (command classifier, egress policy with credential broker, Rule of Two) | Committed, including the `>\|` redirection and `fec0::/10` fixes |
| `obs` | `lha.obs` (events, redaction) | Committed |
| `state` | `lha.state` (git ops, mission anchor, schema migrations, hash-chained decision log) | Committed, including reading, verifying and appending the chained `.lha/decisions.ndjson` (verified on every snapshot read and before every checkpoint), decisions queued mid-cycle (`RecordDecision`), mission references, and carrying `.lha/ownership.json` along; no `vendor` |
| `checklistimport` | `lha.state.checklist_import` | Committed (`.json` and `.md` checklists) |
| `verify` | `lha.verify` (verifier, harness integrity, flaky quarantine, witnesses, trusted runner) | Committed, including operator-protected paths (`LHA_HARNESS_PATHS`); no mutation testing or trust bootstrap |
| `governor` | `lha.governor` (cost ledger, budget governor, metering) | Committed |
| `agent` | `lha.agent` (prompts, loop, compaction, local runner) | Committed: the built-in turn loop with verification, harness integrity, rollback of failed attempts, replanning and checkpoints; `run_mission_local` / `plan_and_run_local`. No `claude_code` lead engine and no tiered memory |
| `agents` | `lha.agents.planner`, `lha.agents.replanner` | Committed (Planner with file ownership, Replanner); not the orchestrator or the other roles |
| `cmd/lha` | `lha.cli.main` | Committed: `version`, `config`, `run-local`, `mission`, `decisions`, the durable `worker`, `mission-start`, `mission-status`, `mission-approve`, `mission-snooze`, `mission-abort` (and the hidden `egress-proxy`). The other commands say they are not yet available and exit 2. `go/cmd/lha/wiring.go` links the execution layer into the runner and the worker's activities (python: `lha.agent.assembly`) |
| `durable` | `lha.durable` (workflows, activities, types, signals, codec, worker, replay harness) | Committed for single-agent missions: `MissionWorkflow` (cycle loop, Continue-As-New, parking with health probes, SLEEPING, the deadlock and tool-approval gates with the escalation ladder, cancellation that waits for the cycle), `SubAgentWorkflow`, the activities, the ClaimCheck codec and the worker guard. Not the organization round or a working `run_subagent`. The mission store writes go through a no-op hook until the Go store is connected |
| `ops` | `lha.ops` (degradation, lifecycle) | Committed |
| `execution` | `lha.execution` (sandboxes, egress proxy, dispatcher, tools including the web tools) | Committed: the `local` and `docker` sandboxes (image, egress allow-list proxy), path containment, the allow-list dispatcher with the Rule of Two and human gates, and every lead tool. No E2B sandbox |
| `hitl` | `lha.hitl.approvals` (`TerminalApprover`, `console_gate`), `lha.hitl.escalation`, `lha.hitl.notify` | Committed: the console y/N gate of `--approve-interactive` with the escalation ladder and the gate webhook; prompts, events and webhook bodies are byte-identical to Python's. The durable `DeferredApprovalGate` is in `durable` |
| `memory`, `persistence`, orchestrator | | Not started |

What the Go CLI can do today:

- `cd go && go build -o lha ./cmd/lha` builds it. `lha run-local` and `lha mission` run a mission
  to completion, deadlock, budget refusal or loop detection in the `local` sandbox (with
  `LHA_ALLOW_UNSAFE_LOCAL=true` / `--unsafe-local`) or the `docker` sandbox (`LHA_SANDBOX_IMAGE`,
  `LHA_SANDBOX_EGRESS`), with the stub, Ollama, OpenAI-compatible or Claude backend.
- The lead gets the same tools as in Python (file IO, `run_command`, `record_decision`, and
  `fetch_url` / `web_search` under `LHA_WEB_ALLOW_HOSTS` / `--allow-host`). An unsafe local sandbox
  and a Rule-of-Two run are refused before the workspace is touched, with Python's messages and
  exit code 2.
- Irreversible commands are refused, or asked on the terminal with `--approve-interactive`
  (reminders at `LHA_GATE_ESCALATION_SECONDS`, rejection after `LHA_CONSOLE_APPROVAL_TIMEOUT_S`,
  the optional `LHA_GATE_WEBHOOK_URL`); the answers are committed as `tool_approval` and
  `gate_reminder` events.
- For the same inputs a Go run and a Python run leave the same checkpoint commits, the same
  `.lha/` files and the same exit code; `go/cmd/lha/e2e_test.go` checks this by running both side
  by side (event payload keys are written in a different order; the JSON is otherwise equal).

What the Go worker runs:

- `lha worker` serves `MissionWorkflow` for single-agent missions on `LHA_TASK_QUEUE`: the cycle
  loop on the real agent loop, parking on a degraded model, checkout or sandbox with health
  probes, `SLEEPING` (scheduled start, pause between cycles, snooze), the tool-approval and
  deadlock gates with the escalation ladder and the gate webhook, Continue-As-New, and
  cancellation that waits for the cycle in flight. The same workdir lock, spend journal and
  exactly-once checkpoint check as Python make a retried attempt safe.
- `lha mission-start` plans (or imports) a checklist and starts a mission; `mission-status`,
  `mission-approve`, `mission-snooze` and `mission-abort` work on missions served by either
  implementation, with the same output and exit codes as Python.

What remains Python-only:

- The durable multi-agent organization (`mission-start --research / --review / --max-parallel`
  are refused by the Go CLI, and a mission that sets them fails on a Go worker), `run_subagent`,
  and the `missions` command.
- `lha orchestrate` (the multi-agent organization), `vendor` and `db`.
- Persistence and memory: Go runs do not write the mission store or the persistent cost ledger
  (so `lha costs` has nothing from them) and the lead has no tiered memory. The Go worker's
  mission-row, `hitl_gates` and cost-ledger writes go through a hook
  (`durable.DefaultStoreOpener`) that is a no-op until the Go mission store lands.
- The `claude_code` model backend and lead engine, the E2B sandbox, re-running failing checks
  (`LHA_FLAKY_RETRIES`), fallback model chains (`LHA_FALLBACK_MODELS`) and OTLP trace export.
- The Docker sandbox's egress proxy container runs the Python proxy source by default in both
  implementations; the Go proxy (`lha egress-proxy`) is used only when the sandbox is given a
  proxy command and an image containing a Linux `lha` binary, which no setting selects yet.

## One task queue per implementation

A mission started by either CLI can be queried, signalled and aborted by either CLI, but its
workflow must be served by workers of ONE implementation, from its first workflow task to its
last.

Temporal replays a workflow by matching the commands the code issues against the recorded
history, and those commands carry sequence-numbered IDs. Python's Temporal SDK numbers activity
IDs and timer IDs with separate counters; the Go SDK uses one counter shared by both. A history
recorded by one SDK therefore does not replay under the other once it contains a timer:
`MissionWorkflow` creates timers when it parks (`DEGRADED_PARK`), while it is `SLEEPING`
(scheduled start, cycle pause, snooze) and while it waits on a human gate with a timeout and
escalation reminders.

So each implementation's workers poll their own task queue, and `lha worker` enforces it (fail
closed): it marks its identity (`lha-py:<pid>@<host>` or `lha-go:<pid>@<host>`), asks Temporal
who polls `LHA_TASK_QUEUE` (`DescribeTaskQueue`) before it starts, and exits 2 when a poller of
the other implementation is listed, suggesting another `LHA_TASK_QUEUE`. Start a mission on the
queue of the implementation that should run it (`LHA_TASK_QUEUE=... lha mission-start`). Temporal
keeps listing a poller for a few minutes after it stops, so moving a queue from one
implementation to the other means waiting that long or using a new queue name. Details:
[wire contract](19-wire-contract.md#cross-language-workers).

## Recommendation

| If you want to | Use |
|---|---|
| Run single-agent missions on Temporal | Go (`lha worker`, `lha mission-start`) or Python |
| Run durable missions with the multi-agent organization, persistence or memory | Python |
| Run a single-agent mission locally from one static binary | Go (`lha run-local`, `lha mission`) or Python |
| Run the multi-agent flow (`lha orchestrate`) | Python |
| Contribute to the port or check conformance | Go packages plus `spec/` |

See [installation](03-installation.md) for setup of either.
