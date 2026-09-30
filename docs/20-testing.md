# Testing

Both implementations are tested independently, and both run the shared cases in `spec/`. CI
([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)) runs the Python suites and the Go
suite.

## Python test layout

All Python tests live in [`python/tests/`](../python/tests/) and run with `pytest`
(`asyncio_mode = "auto"`). Run every command from `python/`.

| Directory | What it covers | Needs |
|---|---|---|
| `tests/unit/` | every module in isolation: safety classifier and egress, sandboxes (with fakes), the egress proxy (`test_egress_proxy.py`), tools, dispatcher, model backends over mocked HTTP, pricing and retry, governor and metering, anchor, checklist import (`test_checklist_import.py`), verifier, witnesses (`test_witnesses.py`), trusted runner (`test_trusted_runner.py`), agent loop, planner, reviewer, orchestrator, memory, coordination, CLI wiring, redaction, `spec/` conformance; the feature files are listed below | nothing |
| `tests/unit/test_large_missions.py` | the large-mission features through the real loop, dispatcher, verifier, `local` sandbox and git anchor with a scripted model: witnesses, trusted checks, protected paths, replanning of blocked items (and its budget and depth limits), approval gates and fingerprints, `fetch_url` registration, references and `lha vendor` | nothing |
| `tests/durability/` | `MissionWorkflow` and `SubAgentWorkflow` on a real Temporal test server: completion, crash after and before commit, Continue-As-New, deadlock, outage park and resume, budget exhaustion, gate decisions (`test_durable_spine.py`); durable approval of irreversible actions (`test_approvals.py`: `WAITING_ON_HUMAN` with the question in `open_question`, an approval reaches the next cycle once, a rejected action is not asked again); the escalation ladder, SLEEPING and the deadlock gate's `impossible` (`test_human_gates.py`); the mission row the workflow writes: `SLEEPING` while sleeping, `WAITING_ON_HUMAN` while the deadlock gate is open, the final status after retry, abort, impossible and a cancellation (while sleeping, at a gate, mid-cycle, which never parks; a mid-cycle abort waits for the cancelled cycle, whose late `RUNNING` write lands first, and a cycle that completes anyway cannot swallow the abort), and a failing row write that never fails the mission (`test_mission_row.py`); sub-agent fan-out (`test_subagent_fanout.py`); the opt-in multi-agent organization (`test_org_workflow.py`: researcher child workflows with failures surfaced, a parallel wave merged one checkpoint at a time, review that reopens, the options validated, a failed implementer recorded as a failed attempt, an implementer outage that parks after integrating the rest, a review that cannot run, an abort during a wave that waits for both implementers, integrates nothing and ends `ABORTED` in the workflow and the row, and the org activities' retry safety and leases); ClaimCheck and the object store (`test_hardening.py`); history replay (`test_replay.py`); the cross-language worker guard (`test_worker_guard.py`: the startup check, the periodic re-check that shuts a running worker down when a Go worker appears, and `lha worker`'s exit 2, with a fake server; on a real server, a refused queue and a re-check that stops a running worker); worker versioning (`test_worker_versioning.py`: the deployment settings and behaviour, half-set settings refused, promotion retries and the warning when a deployment has no current version; on a real server, a mission that stays on the build that started it after a newer build becomes current) | the Temporal test server (downloaded automatically); a real Temporal server for the guard's and versioning's real-server tests (skipped without one, see below) |
| `tests/load/` | `test_long_run.py`: many cycles with Continue-As-New firing repeatedly; history stays bounded and every item completes once. `test_memory_growth.py`: a 40-cycle local mission whose traced memory and embedding cache stop growing with every cycle once the bounded caches fill | the Temporal test server (`test_long_run.py` only) |
| `tests/integration/` | real Postgres + pgvector: migrations, schema, cost ledger, mission upsert, semantic index (`test_postgres.py`), and `PostgresStore`, pgvector memory, the SQLite fallback for an unmigrated database and a `run-local` mission persisted to Postgres (`test_postgres_store.py`); the real Docker sandbox (exit codes, timeouts, OOM, read-only harness dirs, no network); the egress proxy against real Docker and the internet (`test_docker_egress.py`); and one mission that uses every large-mission feature together (`test_large_mission_e2e.py`) | opt-in, see below |

Unit test files for the human-in-the-loop, persistence, web, model, memory and coordination
features (all run without services; HTTP, Postgres and the network are faked):

| File | Covers |
|---|---|
| `test_hitl.py`, `test_hitl_ladder.py` | gate policies; the escalation schedule and rungs; `TerminalApprover` (`--approve-interactive`: shows argv and reason, default reject, no TTY or EOF rejects, reminders then reject on timeout) and its `hitl_gates` rows (who answered, reminders, a broken store never changes the answer, a local run binds it to its store); dispatcher `tool_approval` and `pending` events; the webhook outcomes; the redacted gate payload; the `notify_gate` activity (anchor event, idempotent `hitl_gates` row, a broken store) and `declare_impossible`; `lha gates`, `lha mission-status`, `mission-approve`, `mission-snooze` and the `mission-start` wiring of the ladder and SLEEPING |
| `test_wrapper_gate_events.py` | dispatcher wrappers (`record_decision`, ownership guard) forward the gate's approval events; a tampered decision chain fails the cycle activity with a non-retryable `MissionConfigError` |
| `test_decision_chain_wiring.py` | `.lha/decisions.ndjson` as a hash chain: legacy prefix folding, the `record_decision` tool, cycle-id stamping, the newest five in the snapshot, tamper detection on reads and checkpoints (a committed torn line counts), agent edits discarded, the local runner stopping on tampering, `lha decisions` and `--verify` |
| `test_ownership_integration.py` | the Planner's disjoint write sets, `OwnershipGuard`, the git-layer ownership check, `.lha/ownership.json`, parallel waves in worktrees merged by `BranchIntegrator` with post-merge re-verification, failing implementers, review and reopen of parallel items, witnesses and splitting in a wave, the orchestrator stopping on a tampered chain |
| `test_leases_and_resume.py` | lease decisions (unowned, own, finished owner, open owner, shared, harness, the lead), the broker's anchor-only commits and `lease` events, the live map, the `request_lease` tool, a lease granted and one refused mid-wave in `orchestrate`, `orchestrate --resume` (same mission id, decisions kept, cycle ids continued, residue discarded, board and reflections restored) and its CLI refusals, `mission-start --research --review --max-parallel` |
| `test_coordination.py`, `test_coord_fixes.py` | the ticket lifecycle and the ownership map |
| `test_persist_db.py`, `test_persistence_postgres_fake.py`, `test_persistence_store.py` | the migration runner and repositories against fakes; `PostgresStore` against a fake connection (NULL for unknown cost, the migration check on open, the monotonic status and gate SQL); `SqliteStore` (a terminal status is never overwritten by a non-terminal one unless `reopen`; the gate lifecycle, idempotent and order-tolerant), the store factory and its SQLite fallback, the SQLite path (the per-user default, a relative path's warning, the move out of a checkout), `LedgerSink` and `MissionTracker` |
| `test_persistence_wiring.py` | every run path writes the mission row and every metered call: `run-local`, `mission` (with the Planner backfill), `orchestrate`, the cycle and sub-agent activities (including `WAITING_ON_HUMAN` for a queued approval, `ABORTED` on budget, and an unusable store without fallback as a config error), `mission-start` (the row before the workflow starts: `RUNNING`, `SLEEPING` for a scheduled start, `ABORTED` if the start fails), the `record_mission_status` activity, `lha config`'s `mission store` line, and `lha missions` / `lha costs` |
| `test_web_pinning.py` | DNS rebinding: with a resolver that answers public first and loopback afterwards, `fetch_url`, `web_search` and `lha vendor` dial only the vetted address, on every redirect hop, with TLS SNI, certificate checking and the `Host` header on the hostname; `PinnedNetworkBackend` refuses unpinned hosts, private IP literals and unix sockets |
| `test_web_tools.py`, `test_web_wiring.py` | `fetch_url` and `web_search` error paths; the `LHA_WEB_ALLOW_HOSTS` allow-list and `--allow-host`, default-deny egress, private addresses and redirects, IDNA hosts, the size cap, host-bound credentials from the broker, untrusted wrapping, the Rule of Two preflight (refused before the workspace is touched, and a non-retryable config error in the activities), web tools in every run path |
| `test_model_failover_health.py` | `LHA_FALLBACK_MODELS` parsing and chain order, pricing by the serving model, the health probe per backend (stub, OpenAI-compatible model list, Ollama pulled model, Claude model lookup, failover), the durable health probe reporting a model outage |
| `test_memory_service.py`, `test_hybrid_memory.py`, `test_memory.py`, `test_mem_fixes.py` | tiered memory in the prompt (budget, determinism, episodic recall, skills admitted only after verification, consolidation), degradation to lexical retrieval when the embedder or dense index fails, Postgres memory needing pgvector, padding a narrower embedder to 1024 and refusing a wider one, hybrid retrieval and fusion |
| `test_memory_ollama.py` | the Ollama embedder over a mocked Ollama API: the probe (`/api/tags`, digest in the version, measured `dim`), batching, unreachable server / unpulled model / failing embed, recall by meaning through the memory service, version gating, lexical-only when Ollama is absent or fails mid-run, padding for pgvector |
| `test_flaky_retry_verifier.py` | the verifier re-runs a failing check; quarantine needs a pass and a fail on one work-tree revision; a consistent failure still gates; a quarantined check never makes an item green; timeouts and advisory checks are not re-run; the committed `check_quarantined` event is read back from `HEAD` (uncommitted edits ignored) and written by a local mission |
| `test_otel_tracing.py` | exporter setup from `LHA_OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_ENDPOINT` and the Langfuse keys (Basic auth), `OTEL_SDK_DISABLED`, the missing extra, the CLI configuring it at start (`cli` / `worker`); mission, cycle, model-call and tool-call spans and their parents on a local run, the cycle activity span; redaction; a dead backend never blocking |

```bash
uv run pytest -q tests/unit
uv run pytest -q tests/durability tests/load
uv run pytest -q                     # everything under tests/ (integration tests skip unless enabled)
uv run pytest -q tests/durability/test_durable_spine.py::test_crash_after_commit_is_idempotent
```

The durability and load tests use the stub model's scripted mode and the `local` sandbox with a
gating check that is a real command, so they need no API key, Docker or network (beyond the
first download of the test server).

## The Temporal time-skipping server

Durability and load tests call `WorkflowEnvironment.start_time_skipping()` from `temporalio`. It
starts an in-process Temporal test server; on first use the SDK downloads the test-server binary
and caches it. Durable timers (the park backoff, gate timeouts) complete without real waiting, so
a test that parks for "an hour" runs in seconds. Each test builds its own `Worker` with injected
activities (for example a cycle activity bound to a scripted model via `make_cycle_activity`), so
crashes and outages are simulated deterministically.

## A real Temporal server

The time-skipping test server does not implement every API: `DescribeTaskQueue`, which the
cross-language worker guard calls, is missing. The tests that need a real server use
`LHA_IT_TEMPORAL_ADDRESS` (an existing server, e.g. `temporal server start-dev`), else start
`temporal server start-dev --headless` on free ports with a temporary database when the
[Temporal CLI](https://docs.temporal.io/cli) is on `PATH` (and stop it), else skip: the real-server
half of `tests/durability/test_worker_guard.py` and `tests/durability/test_worker_versioning.py`, and in Go the cross-language durable tests in
`go/cmd/lha/durable_e2e_test.go` and `durable_org_e2e_test.go` (a Go mission driven by the Python
CLI and back, each worker refusing a queue the other polls, two workers started together leaving
at most one running, the re-check stopping workers that raced past the startup check),
`go/cmd/lha/versioning_e2e_test.go` (a mission staying on the build that started it) and the
real-server tests of `go/internal/durable` (`TestRecordHistories`, an abort during an org wave).
Without the Temporal CLI the Go tests fall back to the Python SDK's cached test server, where the
guard tests skip.

```bash
temporal server start-dev --headless --port 7233 &   # or let the tests start their own
LHA_IT_TEMPORAL_ADDRESS=127.0.0.1:7233 uv run pytest -q tests/durability/test_worker_guard.py
cd ../go && LHA_IT_TEMPORAL_ADDRESS=127.0.0.1:7233 go test ./cmd/lha ./internal/durable
```

## The replay test

[`tests/durability/test_replay.py`](../python/tests/durability/test_replay.py) guards workflow
determinism:

- `test_fresh_history_replays` records a three-item mission and replays it.
- `test_fresh_approval_ladder_history_replays` records a mission whose queued `git push` is
  approved at a gate on the escalation ladder, and replays it.
- `test_fresh_mission_row_history_replays` records a mission that deadlocks, is retried at the
  deadlock gate and completes, with the workflow's mission-row writes, and replays it.
- `test_fresh_cancel_mid_cycle_history_replays` records a mission aborted while its first cycle
  runs, and replays it.
- `test_fresh_org_history_replays` records a three-item mission with the organization on (a
  parallel wave, a serial round, one researcher per item, every verified item reviewed), and
  replays it.
- `test_recorded_histories_still_replay` replays every history in
  `tests/durability/histories/` against the current `MissionWorkflow`, and requires at least
  these seven:
  - `mission_three_items.json`: a plain three-item mission;
  - `mission_deadlock_gate_legacy.json` and `mission_approval_gate_legacy.json`: a deadlock
    gate answered `retry` and an approval gate answered `approve`, recorded with the workflow
    code from before the escalation ladder. They replay only because the ladder is guarded by
    `workflow.patched("lha-gate-escalation-v1")`;
  - `mission_approval_ladder.json`: the ladder path;
  - `mission_row_gate_retry.json`: the mission-row path (`lha-mission-row-v1`), a deadlock gate
    answered `retry`, then done;
  - `mission_cancel_mid_cycle.json`: a mission aborted while its first cycle runs, on the
    `lha-cycle-wait-cancel-v1` path (the workflow waits for the cycle's cancellation before it
    writes `ABORTED`);
  - `mission_org_wave_review.json`: the organization's rounds (`plan_round`, `run_implementer`,
    `integrate_branch`, `review_cycle`) with researcher child workflows.

  If a code change makes an in-flight mission non-deterministic, this fails.
- `test_committed_histories_cover_what_they_claim` checks that the legacy histories carry no
  patch marker (and schedule `unblock_items`, or no `notify_gate`), and that the ladder history
  carries the `lha-gate-escalation-v1` marker and schedules `run_agent_cycle`, `notify_gate`,
  `notify_gate`, `run_agent_cycle`; that no older history carries `lha-mission-row-v1` or
  schedules `record_mission_status`; and that the mission-row history carries both markers and
  schedules `record_mission_status` when the deadlock gate opens and at the end; and that only
  the cancel history carries `lha-cycle-wait-cancel-v1`, with the cycle's cancellation recorded
  between its scheduling and the `record_mission_status` that writes `ABORTED`.
- `test_replay_detects_a_changed_workflow` renames the activity in the recorded history and
  requires replay to fail with a non-determinism error.

No committed history covers the SLEEPING path (`lha-sleeping-v1`); `test_human_gates.py`
exercises it live.

Compatible changes are guarded with `workflow.patched` and do not re-record. After an
intentional incompatible change (shipped under a new Build ID), re-record one history:

```bash
LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py -k <test>
```

The recorder rewrites machine-specific paths in payloads (`/workspace/mission`, `python3`) so the
committed file is portable. See [15-operations-runbook.md](15-operations-runbook.md#safe-deploys-during-an-in-flight-mission).

## Integration tests

`tests/integration/` is skipped unless enabled by environment variables
([`tests/integration/conftest.py`](../python/tests/integration/conftest.py)):

| Variable | Enables |
|---|---|
| `LHA_IT_POSTGRES_DSN` | Postgres tests. An admin DSN for a server with the `vector` extension; each test creates a uniquely named database and drops it afterwards |
| `LHA_IT_DOCKER=1` | Docker sandbox tests against the local daemon; they pull `python:3.12-slim`, `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` and `python:3.12-alpine` (the egress proxy) on first use. `test_docker_egress.py` needs internet access to `pypi.org` |
| `LHA_IT_SANDBOX_IMAGE` | the image `test_large_mission_e2e.py` runs the lead in (default `lha-sandbox:dev`); build it from [`sandbox/Dockerfile`](../sandbox/Dockerfile) |

`test_large_mission_e2e.py` drives a small Go project from a Markdown roadmap with a scripted
model: the lead works in the polyglot image with egress limited to the Go module proxy, a
`go:TestHello` witness gates one item, a `trusted:e2e` check runs on the host against the
candidate commit, a too-coarse item is split, a vendored reference is recited, and a `git push` is
routed to a (simulated) approver. It needs `LHA_IT_DOCKER=1`, the sandbox image, a local `go` and
network access:

```bash
docker build -t lha-sandbox:dev sandbox/          # from the repository root
cd python && LHA_IT_DOCKER=1 uv run pytest -q tests/integration/test_large_mission_e2e.py
```

With a throwaway pgvector container on a free port:

```bash
docker run -d --name lha-it-pg -e POSTGRES_USER=lha -e POSTGRES_PASSWORD=lha -e POSTGRES_DB=lha \
  -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16
uv sync --extra postgres --extra sandbox
LHA_IT_POSTGRES_DSN=postgresql://lha:lha@127.0.0.1:55432/lha LHA_IT_DOCKER=1 \
  uv run pytest -q tests/integration
docker rm -f lha-it-pg
```

## Coverage

Coverage uses `pytest-cov` (a dev dependency). Settings are in
[`python/pyproject.toml`](../python/pyproject.toml):

- `source = ["src/lha"]`, branch coverage on.
- `fail_under = 90` with `precision = 2`, so 89.95% fails.

Coverage flags are not in pytest `addopts`; pass them explicitly:

```bash
uv run pytest -q --cov --cov-report= tests/unit tests/durability tests/load
uv run coverage report
```

The floor applies to unit + durability + load together. Integration tests are not included in the
measured total.

## Mutation audit

Coverage says a line ran; it does not say a test would notice if the line were wrong. The mutation
audit checks that for the code where an unnoticed change is a security hole: the command
classifier, the egress policy and public-address checks, the Rule of Two, redaction, workspace
path containment and the sandbox egress lists. A tool changes that code one small edit at a time
(`<` to `<=`, `and` to `or`, a string or a constant) and runs the tests against each change, a
mutant. A mutant no test fails on *survived*: a rule that could break unnoticed.

| | Python | Go |
|---|---|---|
| Tool | mutmut 3.8 (the `mutation` dependency group) | gremlins v0.6.0 |
| Code | `[tool.mutmut]` `paths_to_mutate` in [`pyproject.toml`](../python/pyproject.toml) | `internal/safety` (`commands.go`, `egress.go`, `ipaddr.go`, `rule_of_two.go`), `internal/obs/redact.go`, `internal/execution/paths.go`, `internal/execution/egressproxy/policy.go` |
| Tests | the safety tests and the spec conformance cases (`tests_dir`) | each file's own package tests |
| Script | [`python/scripts/mutation_audit.sh`](../python/scripts/mutation_audit.sh) | [`go/scripts/mutation_audit.sh`](../go/scripts/mutation_audit.sh) |

```bash
python/scripts/mutation_audit.sh   # 4 workers by default (MUTATION_WORKERS)
go/scripts/mutation_audit.sh       # needs gremlins on PATH
```

A survivor, timeout or uncovered mutant that is not a reviewed exception fails the audit. The Go
script exits 1 on one. The Python script prints each as `== <name>` with its diff and exits 0;
the workflow's "Fail on survivors" step fails the run when that line appears.
Most exceptions are proven-equivalent mutants: they change the code but not what it does, so no
test can fail on them. Python marks those lines `# pragma: no mutate (<why>)` (or the line is
rewritten so the redundancy is gone). gremlins has no such comment, so the Go script reads
[`go/scripts/mutation_equivalents.txt`](../go/scripts/mutation_equivalents.txt), which names each
by mutator, file, column and the line's text (so edits elsewhere do not break the list) with the
reason. It also lists, in their own sections, the mutants a named test kills but gremlins cannot
run (it reports `switch` cases as not covered), two that do not compile and one that hangs package
initialisation. The Go spec cases run in `internal/spec`, not in the mutated packages, so the
safety packages also run them as package-level tables.

The [`mutation.yml`](../.github/workflows/mutation.yml) workflow runs both nightly and on demand;
a mutation run re-runs the tests once per mutant, which is too slow for every push. A red run
means a new survivor: add a test that fails on it, or prove it equivalent and mark it.

## Static checks

```bash
uv run ruff check .           # lint: rules E, F, I, UP, B, SIM, RUF; line length 100
uv run ruff format --check .  # formatting
uv run ty check               # types for src/, Python 3.12; warnings are errors
```

`ty` is told to ignore unresolved imports of optional extras (`psycopg`, `docker`, `e2b`,
`opentelemetry`, and others), so the check passes without them installed. The OpenTelemetry SDK
and OTLP/HTTP exporter (the `observability` extra) are also dev dependencies, so the tracing
tests run against the real SDK.

## What CI enforces

On every push to `main` and every pull request:

| Job | Steps |
|---|---|
| `python-check` | `uv sync --locked --extra e2b`; `ruff check`; `ruff format --check`; `ty check`; `pytest tests/unit` with coverage; the Temporal CLI (a pinned, checksum-verified release) and a `temporal server start-dev` on `127.0.0.1:7233`; `pytest tests/durability tests/load` with coverage appended and `LHA_IT_TEMPORAL_ADDRESS` set, so the worker-guard tests run on the dev server (12-minute timeout); `coverage report` (fails under 90%) |
| `go` | Go 1.26 with the module cache; uv and `uv sync --locked` in `python/` (the cross-implementation tests run the Python implementation); `gofmt -l .` must print nothing; `go vet ./...`; `GOOS=windows go vet ./...`; the Temporal CLI and a dev server as in `python-check`; `go test -race ./...` with `LHA_IT_TEMPORAL_ADDRESS` set, so the cross-language durable e2e tests, the worker guard and the recorded histories run on the dev server (15-minute timeout) |
| `python-services-integration` | a `pgvector/pgvector:pg16` service container; `uv sync --locked --extra postgres --extra sandbox`; Go 1.26 (for the trusted `go run` check); `docker build -t lha-sandbox:dev sandbox/`; `pytest tests/integration` with `LHA_IT_POSTGRES_DSN` and `LHA_IT_DOCKER=1` set (20-minute timeout) |

A separate workflow, [`docs-site.yml`](../.github/workflows/docs-site.yml), builds this
documentation site on changes to `docs/` or `website/`, and
[`mutation.yml`](../.github/workflows/mutation.yml) runs the [mutation audit](#mutation-audit)
nightly.

The Go Docker integration tests (`LHA_IT_DOCKER`) are not run by the `go` job.

## Spec conformance tests

The cases in [`spec/`](../spec/) pin behaviour both implementations must share (see
[19-wire-contract.md](19-wire-contract.md#conformance-cases-spec)).

```bash
cd python && uv run pytest -q tests/unit/test_spec_conformance.py
cd go && go test ./internal/spec/
```

Both implementations run every file.

The spec files live outside the Go module (`spec/` is next to `go/`), so Go's test cache does not
see them change. After regenerating `spec/` (or editing it), run the Go conformance tests with
`-count=1`, or a cached pass hides the change:

```bash
cd go && go test -count=1 ./internal/spec/
```

## Go tests

From `go/` (Go 1.26, per [`go/go.mod`](../go/go.mod)):

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
go test -short ./...   # skips the slow cross-implementation anchor tests
```

Every package under `internal/` and `cmd/lha` has tests, apart from the test helpers
(`agent/agenttest`, `model/claudecodetest`, `persistence/pgtest`) and `persistence/services`. `internal/state/decision_chain_test.go` covers the
Go decision chain. `internal/state/crossimpl_test.go` writes a mission anchor with Go and reads
it with the Python implementation (via `uv run --project ../python`), and the reverse, including
byte-identical `decisions.ndjson` links and each side verifying the other's chain; it skips
when `uv` is not on `PATH` or under `-short`. `cmd/lha/e2e_test.go` builds the Go CLI, runs
`lha run-local` with the stub model on the real local sandbox and tools, and runs the same inputs
through Python's `lha run-local` (and a scripted stub mission through `run_mission_local`),
comparing exit codes, reports, checkpoint commits and the `.lha/` files; `internal/hitl` runs
Python's `TerminalApprover` on the same scenarios as the Go console gate. Both skip the Python
half when `uv` is not on `PATH`; the Docker run is gated on `LHA_IT_DOCKER=1`. The Go
Temporal worker's workflows run in the SDK's test environment and replay the histories recorded in
`internal/durable/testdata/histories`; the tests that need a real server are listed under
[A real Temporal server](#a-real-temporal-server).
