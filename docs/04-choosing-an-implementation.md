# Choosing an implementation

LHA has two implementations of the same system: Python in [`python/`](../python/) and Go in
[`go/`](../go/). Python is the reference implementation and the only complete one.
**Use Python for everything today.** The Go port has no CLI and no Temporal worker yet.

## What the two share

The design goal is that the two are interchangeable at every boundary an operator or another
process can observe:

| Surface | Shared definition |
|---|---|
| CLI | The `lha` commands and flags (see [CLI](17-cli.md)). Only Python has a CLI today. |
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
payloads to the object store.

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
| `model` | `lha.model` (stub, OpenAI-compatible/Ollama, Claude, failover, retry, pricing) | Committed. `BuildProvider` does not build a fallback chain from settings, and there is no health probe |
| `safety` | `lha.safety` (command classifier, egress policy with credential broker, Rule of Two) | Committed, including the `>\|` redirection and `fec0::/10` fixes |
| `obs` | `lha.obs` (events, redaction) | Committed |
| `state` | `lha.state` (git ops, mission anchor, schema migrations, hash-chained decision log) | Committed, including reading, verifying and appending the chained `.lha/decisions.ndjson` (verified on every snapshot read and before every checkpoint), decisions queued mid-cycle (`RecordDecision`), mission references, and carrying `.lha/ownership.json` along; no `vendor` |
| `checklistimport` | `lha.state.checklist_import` | Committed (`.json` and `.md` checklists) |
| `verify` | `lha.verify` (verifier, harness integrity, flaky quarantine, witnesses, trusted runner) | Committed, including operator-protected paths (`LHA_HARNESS_PATHS`); no mutation testing or trust bootstrap |
| `governor` | `lha.governor` (cost ledger, budget governor, metering) | Committed |
| `agent` | `lha.agent` (prompts, loop, compaction, local runner) | Committed: the built-in turn loop with verification, harness integrity, rollback of failed attempts, replanning and checkpoints; `run_mission_local` / `plan_and_run_local`. No `claude_code` lead engine and no tiered memory |
| `agents` | `lha.agents.planner`, `lha.agents.replanner` | Committed (Planner with file ownership, Replanner); not the orchestrator or the other roles |
| `cmd/lha` | `lha.cli.main` | Committed: `version`, `config`, `run-local`, `mission`, `decisions`. The other commands say they are not yet available and exit 2. Runs need the execution layer, which is linked in `go/cmd/lha/wiring.go` |
| `execution` (sandboxes, egress proxy, tools including the web tools), `hitl` (approvals, escalation, webhook), `memory`, `persistence`, durable worker | | Not started |

Consequences today:

- `cd go && go build -o lha ./cmd/lha` builds the Go CLI. Until the execution layer (sandboxes
  and tools) is linked, `run-local` and `mission` stop with `error: execution layer not linked`
  (exit 2), so use the Python implementation to run missions.
- There is no Go Temporal worker and no Go persistence: Go runs do not write the mission store.
- The Go packages can be tested with `cd go && go test ./...`.

## Known limitation: mixed Python/Go workers on one mission

The wire contract lets both implementations serve the same task queue, but sharing one
*running* mission between them is not currently possible when the history contains timers.

Temporal replays a workflow by matching the commands the code issues against the recorded
history, and those commands carry sequence-numbered IDs. Python's Temporal SDK numbers activity
IDs and timer IDs with separate counters; the Go SDK uses one counter shared by both. A history
recorded by one SDK therefore does not replay under the other once it contains a timer:
`MissionWorkflow` creates timers when it parks (`workflow.sleep` in `DEGRADED_PARK`), while it
is `SLEEPING` (scheduled start, cycle pause, snooze) and while it waits on a human gate with a
timeout and escalation reminders. This was verified experimentally.

The design for missions that mix Python and Go workers is still open. Until it is settled, run
each mission's workflow on workers of one implementation.

## Recommendation

| If you want to | Use |
|---|---|
| Run missions locally or on Temporal | Python |
| Run the multi-agent flow (`lha orchestrate`) | Python |
| Contribute to the port or check conformance | Go packages plus `spec/` |

See [installation](03-installation.md) for setup of either.
