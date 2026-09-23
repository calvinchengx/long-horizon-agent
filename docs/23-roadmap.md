# Roadmap

LHA is built lean-baseline-first: a durable single-agent loop, then a verification gate, then
multi-agent roles only where they can be shown to help. This page lists each phase with what the
code does today. "Library" means the code exists and is unit-tested but no CLI command or
workflow calls it.

## Python implementation

### Phase 0: durable single-agent spine (done)

- `MissionWorkflow` on Temporal: one `run_agent_cycle` activity per cycle, Continue-As-New, park
  with backoff on outages (`DEGRADED_PARK`) until the health probe reports git, the model and the
  sandbox healthy, `status_v1` and `open_question` queries, `human_decision_v1`, `steer_v1` and
  snooze signals ([08-durable-execution.md](08-durable-execution.md), [14-running-on-temporal.md](14-running-on-temporal.md)).
- Human gates (`WAITING_ON_HUMAN`): an approval gate for irreversible actions (approve / reject,
  default reject, `--approval-timeout-hours`, default `LHA_APPROVAL_TIMEOUT_S` = 24 h) and a
  deadlock gate (retry / abort / impossible, default `LHA_DEADLOCK_GATE_DEFAULT` = abort,
  `--deadlock-gate-hours`, default 24 h). Every gate has an escalation ladder
  (`LHA_GATE_ESCALATION_SECONDS`): each reminder is committed as a `gate_*` event in the anchor
  and posted to the optional `LHA_GATE_WEBHOOK_URL`.
- `SLEEPING`: a scheduled start (`--start-in-seconds`), a pause between cycles
  (`--cycle-pause-seconds`, `LHA_CYCLE_PAUSE_SECONDS`) and `lha mission-snooze`, all on durable
  timers.
- Retry-safe cycles: workdir lock, reset to `HEAD`, exactly-once commit per cycle id, heartbeats,
  spend journal across attempts.
- Git mission anchor (`.lha/`) as the source of truth ([06-mission-anchor.md](06-mission-anchor.md)).
- ClaimCheck codec, replay test against a recorded history.
- CLI: `worker`, `mission-start`, `mission-status`, `mission-approve`, `mission-snooze`,
  `mission-abort`, plus the local `run-local` and `mission` ([17-cli.md](17-cli.md)).
- Crash recovery, Continue-As-New and long-run tests on the Temporal test server.

### Phase 1: deterministic verifier as the gate (done; measurement partly library)

- Done: `DeterministicVerifier`; an item is `done` only after a passing gating check; default
  Python checks; `--check` commands; harness-integrity checks that revert tampering with existing
  tests; auto-blocking after 3 consecutive failures ([07-verification.md](07-verification.md)).
- Library: flaky-test quarantine, mutation-based checks and trust bootstrap
  (`verify/flaky_quarantine.py`, `verify/mutation.py`, `verify/trust_bootstrap.py`).
- Not done: measurements of how much the gate changes outcomes on real tasks.

### Large missions (done)

Features for missions built from an operator's roadmap over weeks, wired into every Python run
path (local, durable and `orchestrate`) through [`agent/assembly.py`](../python/src/lha/agent/assembly.py):

- `--checklist` import of JSON checklists and Markdown roadmaps
  ([06-mission-anchor.md](06-mission-anchor.md#importing-a-checklist));
- item witnesses (`go:`, `pytest:`, `cmd:`, `trusted:`) and operator-defined trusted checks run
  outside the sandbox on the candidate commit ([07-verification.md](07-verification.md#witnesses));
- operator-protected paths (`LHA_HARNESS_PATHS`);
- the replanner, which splits a blocked item instead of deadlocking
  ([11-multi-agent-organization.md](11-multi-agent-organization.md));
- a configurable sandbox image, a reference polyglot image in `sandbox/`, and an allow-listed
  egress proxy on an internal Docker network (`LHA_SANDBOX_IMAGE`, `LHA_SANDBOX_EGRESS`);
- web tools (`fetch_url`, and `web_search` with `LHA_WEB_SEARCH_PROVIDER` and a key) when
  `LHA_WEB_ALLOW_HOSTS` or `--allow-host` names at least one host: a default-deny egress policy,
  public addresses only, redirects re-checked, a credential broker that binds each secret to its
  hosts, output fenced as untrusted, and a Rule of Two preflight that refuses web tools together
  with private data (`LHA_PRIVATE_DATA` or the `local` sandbox);
- `lha vendor` with `--reference`;
- human approval of irreversible commands: durable (`WAITING_ON_HUMAN`, `lha mission-approve`) and
  local (`--approve-interactive`, a terminal prompt that rejects after
  `LHA_CONSOLE_APPROVAL_TIMEOUT_S`, default 3600 s).

Not done: `--checklist` on `orchestrate`; a Planner that writes witnesses; measurements of split
quality with real models.

### Phase 2: independent reviewer and decision log (partial)

- Done, local only: the reviewer in `lha orchestrate` gives a structured verdict and can reopen an
  item.
- Done, every run path: the hash-chained decision log. The lead's `record_decision` tool queues a
  decision, the cycle's checkpoint chains it into `.lha/decisions.ndjson`, and the five newest
  decisions go into the next prompt. `lha decisions --verify` checks the chain. An altered log
  stops a local run, and fails a durable cycle with a non-retryable error
  ([06-mission-anchor.md](06-mission-anchor.md#decisionsndjson)).
- Not done: the reviewer inside the durable workflow.

### Phase 3: read-only researcher fan-out (partial)

- Done, local only: `lha orchestrate` fans out read-only researchers per item.
- Library: `SubAgentWorkflow` and `research_children` run sub-agents as durable child workflows.
  The worker registers `SubAgentWorkflow` and it is tested, but `MissionWorkflow` never starts
  it.

### Phase 4 and later

| Area | State |
|---|---|
| Planner (task -> checklist) | done: used by `mission`, `orchestrate`, `mission-start` (skipped when `--checklist` is given) |
| Replanner (split a blocked item) | done: every run path |
| Per-role model routing (Claude tiers) | done: `orchestrate` with the `claude` backend |
| File ownership, tickets, blackboard, parallel implementers in git worktrees, `BranchIntegrator` | done in `lha orchestrate` only (`LHA_MAX_PARALLEL_IMPLEMENTERS`, default 3); not in the durable workflow ([11-multi-agent-organization.md](11-multi-agent-organization.md)) |
| Memory: episodic, semantic, skills, hybrid BM25 + dense retrieval, consolidation, degradation to lexical-only | done: every run path gives the lead a memory block (`LHA_MEMORY_ENABLED`, default on) ([12-memory.md](12-memory.md)) |
| Persistence: mission rows and the idempotent cost ledger on SQLite (default) or Postgres (`LHA_POSTGRES_DSN`), `lha missions`, `lha costs`, `lha db migrate` | done: every run path |
| Model resilience: fallback chain (`LHA_FALLBACK_MODELS`), health probe before a parked mission resumes | done: every run path ([13-models.md](13-models.md#retries-and-failover)) |
| Operator-configurable egress allow-list | done: `LHA_SANDBOX_EGRESS` (sandbox, via proxy) and `LHA_WEB_ALLOW_HOSTS` / `--allow-host` (web tools) |
| Human approval of irreversible actions | done: durable approval gate and local `--approve-interactive` |
| Deadlock gate with "impossible", escalation ladder, gate webhook, `SLEEPING` | done: durable path |
| Offline evolution: prompt evolver, judge, eval harness, promotion gate | library |
| Observability: Langfuse export, OTel exporter setup | library: `build_langfuse` builds a client nobody calls; `orchestrate` opens OTel spans but LHA installs no exporter ([16-observability.md](16-observability.md)) |
| Saga compensation, orphan-branch reconciliation, durable ticket ledgers | library (`durable/saga.py`, `durable/reconcile.py`, `durable/ledgers.py`) |
| Auditor, Librarian, Tester and model-backed Integrator runners | library: thin role wrappers no run path calls |
| Worker Build IDs / versioned deploys | planned |
| Re-embedding after an embedding-model change | planned |

### Known limitations of built features

These limitations are in the current code:

- Gates are not persisted to the `hitl_gates` table. An open gate lives in the workflow state and
  its events in the anchor.
- The `missions` row is written by the local runners, `mission-start` and the durable cycle
  activity, never by `MissionWorkflow`. For a durable mission the activity writes `RUNNING`,
  `DONE`, `IMPOSSIBLE` (the checklist is deadlocked), `ABORTED` (budget refused) and
  `WAITING_ON_HUMAN` (the cycle queued an approval). The row never shows `DEGRADED_PARK`,
  `SLEEPING`, `WAITING_ON_HUMAN` for the deadlock gate, or the outcome the workflow reaches after
  a gate decision (for example abort or impossible at the deadlock gate) or at `max_cycles`.
- Lease granting: a `LeaseRequest` for another writer's file is never granted.
- Parallel implementer waves, tickets and the blackboard exist only in `lha orchestrate`, not in
  the Temporal workflow.
- The Go port has no CLI (`go/cmd/lha`) and no Temporal worker, and Temporal histories with
  timers do not replay across the two languages (see [Go phase 2](#go-phase-2-temporal-not-started)).
- `lha orchestrate` re-initializes the anchor and does not resume an earlier run.
- The default memory embedder (`hash`) is lexical, not semantic; `sentence_transformers` needs the
  `embeddings` extra.
- Every `openai_compat` model, primary or fallback, uses the one endpoint in
  `LHA_OPENAI_BASE_URL`.
- Web tools check the resolved address before connecting, but the HTTP client resolves again,
  so DNS rebinding between the two lookups is a residual risk.

## Go port

The Go implementation ([`go/`](../go/)) is being built in three phases so that each lands usable
and wire-compatible with Python ([19-wire-contract.md](19-wire-contract.md)). Until a phase lands,
use the Python implementation for that feature
([04-choosing-an-implementation.md](04-choosing-an-implementation.md)).

### Go phase 1: the spine (in progress)

| Component | Package | State |
|---|---|---|
| `LHA_*` settings and `.env` | `internal/config` | present, including the large-mission settings (read, not yet used). Not yet read: the gate, `SLEEPING`, web-tool, fallback-chain, health-probe, persistence, memory and parallel-implementer settings |
| Shared contracts (state, model, tools, sandbox, verify) | `internal/contracts` | present, including witnesses, `split` and `Checklist.Split`, references and `Check.Where`; `spec/state/checklist.json` passes |
| Safety: command classifier, egress policy, IDNA, shlex | `internal/safety` | present; `spec/safety/*` pass |
| Model backends: stub, OpenAI-compatible, Claude, pricing, retry, failover | `internal/model` | present; `spec/model/pricing.json` passes |
| Budget governor, cost ledger, metering | `internal/governor` | present |
| Git mission anchor and git operations | `internal/state` | present; cross-implementation tests read Python anchors and vice versa |
| Verifier, harness integrity, flaky quarantine | `internal/verify` | present; `spec/verify/harness_files.json` passes. No witnesses, trusted runner or `LHA_HARNESS_PATHS` yet |
| Redaction and structured events | `internal/obs` | present; `spec/obs/redact.json` passes |
| Checklist import, `vendor` | `internal/state` | not started |
| Sandboxes (local, Docker, E2B), egress proxy and tools | `internal/execution` | not started |
| Agent loop, replanner, approval gates | `internal/agent`, `internal/hitl` | not started |
| CLI | `cmd/lha` | not started (`go/cmd/lha` does not exist) |

`go test ./...` passes for the present packages. No CI job runs the Go tests yet.

### Go phase 2: Temporal (not started)

A Go worker serving `MissionWorkflow` and `SubAgentWorkflow` with the same names, payloads and
ClaimCheck codec, and the `worker` and `mission-*` commands. Known limitation to resolve or
document: the Go SDK shares one sequence counter between activities and timers while the Python
SDK keeps separate ones, so histories with timers do not replay across languages
([19-wire-contract.md](19-wire-contract.md#known-cross-language-limitation)).

### Go phase 3: organization, memory, Postgres, observability (not started)

The planner, reviewer, researchers and orchestrator; file ownership, tickets, the blackboard,
parallel implementers and the integrator; human gates and web tools; memory; the mission store
(SQLite and Postgres) and `db migrate`; and `spec/coordination/shared_paths.json` (Go passes
`decision_chain.json` today, in `internal/state`).
