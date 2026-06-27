# Roadmap

LHA is built lean-baseline-first: a durable single-agent loop, then a verification gate, then
multi-agent roles only where they can be shown to help. This page lists each phase with what the
code does today. "Library" means the code exists and is unit-tested but no CLI command or
workflow calls it.

## Python implementation

### Phase 0: durable single-agent spine (done)

- `MissionWorkflow` on Temporal: one `run_agent_cycle` activity per cycle, Continue-As-New, park
  with backoff on outages, deadlock gate, `status_v1` query, `human_decision_v1` and `steer_v1`
  signals ([08-durable-execution.md](08-durable-execution.md), [14-running-on-temporal.md](14-running-on-temporal.md)).
- Retry-safe cycles: workdir lock, reset to `HEAD`, exactly-once commit per cycle id, heartbeats,
  spend journal across attempts.
- Git mission anchor (`.lha/`) as the source of truth ([06-mission-anchor.md](06-mission-anchor.md)).
- ClaimCheck codec, replay test against a recorded history.
- CLI: `worker`, `mission-start`, `mission-status`, `mission-approve`, `mission-abort`, plus the
  local `run-local` and `mission` ([17-cli.md](17-cli.md)).
- Crash recovery, Continue-As-New and long-run tests on the Temporal test server.

The README still labels Phase 0 "in progress"; by the code, it is complete for the single-agent
path.

### Phase 1: deterministic verifier as the gate (done; measurement partly library)

- Done: `DeterministicVerifier`; an item is `done` only after a passing gating check; default
  Python checks; `--check` commands; harness-integrity checks that revert tampering with existing
  tests; auto-blocking after 3 consecutive failures ([07-verification.md](07-verification.md)).
- Library: flaky-test quarantine, mutation-based checks and trust bootstrap
  (`verify/flaky_quarantine.py`, `verify/mutation.py`, `verify/trust_bootstrap.py`).
- Not done: measurements of how much the gate changes outcomes on real tasks.

### Phase 2: independent reviewer and decision log (partial)

- Done, local only: the reviewer in `lha orchestrate` gives a structured verdict and can reopen an
  item.
- Library: the hash-chained decision log (`coordination/decision_log.py`). The anchor has
  `decisions.ndjson` and loads its last five records into the situation snapshot, but no runtime
  path writes decisions and the prompt does not include them.
- Not done: the reviewer inside the durable workflow.

### Phase 3: read-only researcher fan-out (partial)

- Done, local only: `lha orchestrate` fans out read-only researchers per item.
- Library: `SubAgentWorkflow` and `research_children` run sub-agents as durable child workflows
  (tested), but `MissionWorkflow` does not call them.

### Phase 4 and later (mostly library)

| Area | State |
|---|---|
| Planner (task -> checklist) | done: used by `mission`, `orchestrate`, `mission-start` |
| Per-role model routing (Claude tiers) | done: `orchestrate` with the `claude` backend |
| Integrator and parallel writers | role definitions and file-ownership rules only ([11-multi-agent-organization.md](11-multi-agent-organization.md)) |
| Memory: episodic, semantic (in-memory and pgvector), hybrid BM25 + dense retrieval, skills, consolidation | library ([12-memory.md](12-memory.md)) |
| Offline evolution: prompt evolver, judge, eval harness, promotion gate | library |
| Postgres persistence: schema, `lha db migrate`, cost-ledger and mission repositories | schema and migrate done; repositories library |
| Observability: Langfuse export, OTel exporter setup | hooks only ([16-observability.md](16-observability.md)) |
| Saga compensation, orphan-branch reconciliation | library |
| Worker Build IDs / versioned deploys | planned |
| Operator-configurable egress allow-list | planned |
| Human gates beyond the deadlock gate (irreversible actions, "declare impossible?", escalation) | planned; policy classes are library |
| Re-embedding after an embedding-model change | planned |

## Go port

The Go implementation ([`go/`](../go/)) is being built in three phases so that each lands usable
and wire-compatible with Python ([19-wire-contract.md](19-wire-contract.md)). Until a phase lands,
use the Python implementation for that feature
([04-choosing-an-implementation.md](04-choosing-an-implementation.md)).

### Go phase 1: the spine (in progress)

| Component | Package | State |
|---|---|---|
| `LHA_*` settings and `.env` | `internal/config` | present |
| Shared contracts (state, model, tools, sandbox, verify) | `internal/contracts` | present |
| Safety: command classifier, egress policy, IDNA, shlex | `internal/safety` | present; `spec/safety/*` pass |
| Model backends: stub, OpenAI-compatible, Claude, pricing, retry, failover | `internal/model` | present; `spec/model/pricing.json` passes |
| Budget governor, cost ledger, metering | `internal/governor` | present |
| Git mission anchor and git operations | `internal/state` | present; cross-implementation tests read Python anchors and vice versa |
| Verifier, harness integrity, flaky quarantine | `internal/verify` | present; `spec/verify/harness_files.json` passes |
| Sandboxes (local, Docker, E2B) and tools | `internal/execution` | not started |
| Agent loop | `internal/agent` | not started |
| CLI | `cmd/lha` | not started (the directory is empty; `go build ./cmd/lha` fails) |
| Redaction and structured events | `internal/obs` | not started; `spec/obs/redact.json` not run by Go |

`go test ./...` passes for the present packages. No CI job runs the Go tests yet.

### Go phase 2: Temporal (not started)

A Go worker serving `MissionWorkflow` and `SubAgentWorkflow` with the same names, payloads and
ClaimCheck codec, and the `worker` and `mission-*` commands. Known limitation to resolve or
document: the Go SDK shares one sequence counter between activities and timers while the Python
SDK keeps separate ones, so histories with timers do not replay across languages
([19-wire-contract.md](19-wire-contract.md#known-cross-language-limitation)).

### Go phase 3: organization, memory, Postgres, observability (not started)

The planner, reviewer, researchers and orchestrator; memory; `db migrate` and the Postgres
repositories; the `obs` package; and the remaining `spec/coordination/*` cases.
