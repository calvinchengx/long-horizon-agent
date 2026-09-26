# Choosing an implementation

LHA has two implementations of the same system: Python in [`python/`](../python/) and Go in
[`go/`](../go/). Python is the reference implementation and the only complete one.
The Go CLI runs missions locally in the local or Docker sandbox: single-agent (`lha run-local`,
`lha mission`) and the multi-agent organization (`lha orchestrate`, including `--resume`), with
the built-in turn loop or the `claude_code` lead engine, the mission store, the persistent cost
ledger and tiered memory. Everything durable (the Temporal worker and `mission-*` commands, and
with them the durable organization rounds) and E2B are Python-only.

## What the two share

The design goal is that the two are interchangeable at every boundary an operator or another
process can observe:

| Surface | Shared definition |
|---|---|
| CLI | The `lha` commands and flags (see [CLI](17-cli.md)). The Go CLI has `version`, `config`, `run-local`, `mission`, `orchestrate`, `decisions`, `vendor`, `missions`, `costs`, `gates` and `db migrate`. |
| Settings | The `LHA_*` environment variables and `.env` file, with the same names and defaults ([`python/src/lha/config.py`](../python/src/lha/config.py), [`go/internal/config/`](../go/internal/config/)) |
| Mission anchor | The `.lha/` files and their JSON shapes ([the mission anchor](06-mission-anchor.md)) |
| Mission store | The SQLite file (same tables, migration ids and JSON columns) and the Postgres schema in [`db/migrations/`](../db/migrations/), with the same `schema_migrations` bookkeeping |
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
writes an anchor that Python reads back identically, and the reverse. The mission store has the
same kind of tests ([`go/internal/persistence/crossimpl_test.go`](../go/internal/persistence/crossimpl_test.go)):
the same writes leave byte-identical rows in a SQLite file, each implementation reads (and
replays ledger writes into) the other's file, and on Postgres each sees the other's `lha db
migrate` as already applied.

`spec/` pins behaviour that must match exactly: which commands need human approval, which URLs
may be fetched, redaction, checklist transitions, check naming, the decision-log hash chain,
shared-path ownership, protected harness files, model pricing, and memory retrieval (hash
embedder vectors, BM25 scores, fusion order and the memory block recalled for a fixture). Python is the reference: a
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
| `spec` | conformance harness | Committed: runs every file in `spec/`, including `agent/prompts.json`, `agent/org.json`, `coordination/shared_paths.json` and `memory/` |
| `model` | `lha.model` (stub, OpenAI-compatible/Ollama, Claude, Claude Code, failover, retry, pricing) | Committed, including the `claude_code` backend (`claude -p`, the same argv, result parsing, errors and reported cost), `LHA_FALLBACK_MODELS` chains (same `backend:model[@in/out]` format, errors and per-member pricing) and the model health probe (`ProbeModel`) |
| `safety` | `lha.safety` (command classifier, egress policy with credential broker, Rule of Two) | Committed, including the `>\|` redirection and `fec0::/10` fixes |
| `obs` | `lha.obs` (events, redaction, OpenTelemetry) | Committed, including OTLP/HTTP trace export (`obs/tracing`: the same settings, span names and redacted attributes) |
| `state` | `lha.state` (git ops, mission anchor, schema migrations, hash-chained decision log) | Committed, including reading, verifying and appending the chained `.lha/decisions.ndjson` (verified on every snapshot read and before every checkpoint), decisions queued mid-cycle (`RecordDecision`), mission references, the ownership map (`.lha/ownership.json`: read, staged, written at initialization), anchor-only commits (`CommitAnchorUpdate`, for lease decisions) and `ReadEvents`; `state/vendor` is `lha vendor` (same layout and `MANIFEST.json` bytes) |
| `checklistimport` | `lha.state.checklist_import` | Committed (`.json` and `.md` checklists) |
| `verify` | `lha.verify` (verifier, harness integrity, flaky quarantine, witnesses, trusted runner) | Committed, including operator-protected paths (`LHA_HARNESS_PATHS`); no mutation testing or trust bootstrap |
| `governor` | `lha.governor` (cost ledger, budget governor, metering) | Committed, including `RunExternal` (a `claude_code` session metered by its reported cost) and the `CostHook` every recorded call, external ones included, is handed to (python `on_record`) |
| `agent` | `lha.agent` (prompts, loop, compaction, local runner) | Committed: the built-in turn loop with verification, harness integrity, rollback of failed attempts, replanning and checkpoints; `run_mission_local` / `plan_and_run_local` with the mission row, the persistent cost ledger and tiered memory in the lead prompt (recalled before the first turn, recorded after the checkpoint, for the turn loop and the `claude_code` engine alike); the `claude_code` lead engine and its MCP bridge (`agent/mcpbridge`: the same MCP config, tools and results as `lha.agent.mcp_bridge`) |
| `agents` | `lha.agents.planner`, `replanner`, `roles`, `router`, `reviewer` (verdict parsing), `reflection` | Committed: Planner with file ownership, Replanner, the role chart, per-role model routing, review parsing, reflection |
| `agents/org` | `lha.agents.orchestrator`, `waves`, `integrator`, `reviewer`, `team`, `subagent`, `specialists` | Committed: the sub-agent loop, research fan-out, the Reviewer, implementer waves in `.git/lha-worktrees`, the `BranchIntegrator` and the `Orchestrator` with `--resume`. Its run services (`DefaultServices`, python `open_run_services`) write the mission row, every role's calls to the persistent ledger and the terminal gate's events, and give the Lead tiered memory. Not the durable (Temporal) rounds |
| `coordination` | `lha.coordination` (ownership, enforcement, leases, ticket, blackboard) | Committed: the ownership map and `OwnershipGuard` (same refusal messages), the git-layer check, the `LeaseBroker` (sharing Python's `.git/lha-cycle.lock` flock) and `request_lease`, tickets, the blackboard |
| `cmd/lha` | `lha.cli.main` | Committed: `version`, `config`, `run-local`, `mission`, `orchestrate`, `decisions`, `vendor`, `missions`, `costs`, `gates`, `db migrate` (and the hidden `egress-proxy`). The durable commands say they are not yet available and exit 2. `go/cmd/lha/wiring.go` links the execution layer into the runner (python: `lha.agent.assembly`) |
| `execution` | `lha.execution` (sandboxes, egress proxy, dispatcher, tools including the web tools) | Committed: the `local` and `docker` sandboxes (image, egress allow-list proxy), path containment, the allow-list dispatcher with the Rule of Two and human gates, and every lead tool. No E2B sandbox ([see below](#e2b-is-not-supported-in-go)) |
| `hitl` | `lha.hitl.approvals` (`TerminalApprover`, `console_gate`), `lha.hitl.escalation`, `lha.hitl.notify` | Committed: the console y/N gate of `--approve-interactive` with the escalation ladder and the gate webhook; prompts, events and webhook bodies are byte-identical to Python's; every gate event is written to `hitl_gates`. No `DeferredApprovalGate` (it belongs to the durable workflow) |
| `persistence` | `lha.persistence` (store, SQLite, Postgres, tracking, services, `db migrate`) | Committed: one `Store` interface with a pure-Go SQLite backend and a pgx Postgres backend on Python's schemas, the fallback from Postgres to SQLite, monotonic terminal statuses, the idempotent cost ledger, `hitl_gates`, `MissionTracker`, `LedgerSink` on the `CostMeter`, and run services. No object store (it has its own port) |
| `memory`, `ops` | `lha.memory`, `lha.ops.degradation` | Committed: episodic recall, BM25 + dense retrieval fused with RRF, skills, extractive and model consolidation, the hash and Ollama embedders (padded for pgvector), pgvector on Postgres, stored vectors on SQLite, and the degradation to BM25 + `git grep`. `sentence_transformers` and the `cross_encoder` reranker are Python-only extras: Go falls back exactly as Python does without the extra. No `VoyageEmbedder` (a Python library class no setting selects) |
| durable worker | `lha.durable` | Not started in this tree (in progress on a separate branch) |

What the Go CLI can do today:

- `cd go && go build -o lha ./cmd/lha` builds it. `lha run-local` and `lha mission` run a mission
  to completion, deadlock, budget refusal or loop detection in the `local` sandbox (with
  `LHA_ALLOW_UNSAFE_LOCAL=true` / `--unsafe-local`) or the `docker` sandbox (`LHA_SANDBOX_IMAGE`,
  `LHA_SANDBOX_EGRESS`), with the stub, Ollama, OpenAI-compatible, Claude or Claude Code
  (`claude_code`) backend, and with the built-in turn loop or the `claude_code` lead engine
  (`LHA_LEAD_ENGINE=claude_code`, both `LHA_CLAUDE_CODE_TOOLS` modes).
- The lead gets the same tools as in Python (file IO, `run_command`, `record_decision`, and
  `fetch_url` / `web_search` under `LHA_WEB_ALLOW_HOSTS` / `--allow-host`). An unsafe local sandbox
  and a Rule-of-Two run are refused before the workspace is touched, with Python's messages and
  exit code 2.
- Irreversible commands are refused, or asked on the terminal with `--approve-interactive`
  (reminders at `LHA_GATE_ESCALATION_SECONDS`, rejection after `LHA_CONSOLE_APPROVAL_TIMEOUT_S`,
  the optional `LHA_GATE_WEBHOOK_URL`); the answers are committed as `tool_approval` and
  `gate_reminder` events.
- `lha vendor` snapshots reference pages with the same egress rules, DNS pinning (each hop is
  resolved once and only a vetted address is dialled), output, exit codes and `MANIFEST.json`.
- `LHA_FALLBACK_MODELS` builds a failover chain, and OTLP trace export
  (`LHA_OTEL_EXPORTER_OTLP_ENDPOINT` or Langfuse) emits the same spans as Python
  ([observability](16-observability.md)).
- `lha orchestrate` plans (or takes `--checklist`) and runs the organization: research fan-out,
  the Lead, reflection, the Reviewer, parallel implementer waves with leases and the integrator.
  A mission started by either implementation can be continued by the other with `--resume`.
- For the same inputs a Go run and a Python run leave the same checkpoint commits, the same
  `.lha/` files and the same exit code; `go/cmd/lha/e2e_test.go`,
  `go/cmd/lha/claude_code_test.go` and `go/cmd/lha/orchestrate_test.go` check this by running
  both side by side, including a mission interrupted in one implementation and resumed in the
  other (event payload keys are written in a different order; the JSON is otherwise equal).
- `run-local`, `mission` and `orchestrate` write the mission row (RUNNING, then DONE /
  IMPOSSIBLE / ABORTED) and every metered model call (every org role's, and a `claude_code`
  session's reported cost) to the mission store (the per-user SQLite file, or Postgres with
  `LHA_POSTGRES_DSN`), and the lead gets the same tiered memory block as in Python. `lha
  missions`, `lha costs` and `lha gates` read the store with Python's output; `lha db migrate`
  applies `db/migrations/`. Either implementation reads what the other wrote, and the parity
  tests compare the stores the two leave (ledger, mission row, memory tiers) as well.

What remains Python-only:

- The Temporal worker and the `worker`, `mission-start`, `mission-status`, `mission-approve`,
  `mission-abort` and `mission-snooze` commands, and with them the durable multi-agent
  organization rounds (a Go port of the worker is in progress on a separate branch).
- The E2B sandbox (E2B has no Go SDK; see below).
- The `sentence_transformers` embedder and the `cross_encoder` reranker (Python extras): the Go
  memory plane falls back to lexical retrieval, and keeps fusion order, as Python does when the
  extra is not installed; and the library-only `VoyageEmbedder`.
- The Docker sandbox's egress proxy container runs the Python proxy source by default in both
  implementations; the Go proxy (`lha egress-proxy`) is used only when the sandbox is given a
  proxy command and an image containing a Linux `lha` binary, which no setting selects yet.

## E2B is not supported in Go

`LHA_SANDBOX=e2b` (or `--sandbox e2b`) is refused by the Go CLI with exit 2 and a message
pointing to `docker` or the Python implementation. E2B publishes SDKs for Python and JavaScript
only. A Go adapter would have to speak E2B's REST control plane and the in-VM `envd` API
(Connect-RPC process streaming and file transfer) directly, and re-implement the Python adapter's
two-way workspace sync, whose copy-back is a security boundary (only requested regular files and
symlinks, no `.git`/`.lha`, no writes through host symlinks, a 256 MiB cap, fail closed on any
error or missing exit code). Without an SDK that contract could only be tested against a fake of
our own making, not the real service, so the Go implementation does not offer it.

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
| Run missions on Temporal | Python |
| Run a mission locally from one static binary, with persistence and memory | Go (`lha run-local`, `lha mission`) or Python |
| Run the multi-agent flow locally (`lha orchestrate`) | Go or Python |
| Use `sentence_transformers` embeddings or a cross-encoder reranker | Python |
| Contribute to the port or check conformance | Go packages plus `spec/` |

See [installation](03-installation.md) for setup of either.
