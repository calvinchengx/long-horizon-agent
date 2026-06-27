# Testing

Both implementations are tested independently, and both run the shared cases in `spec/`. CI
([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)) runs the Python suites; the Go suite
is run locally.

## Python test layout

All Python tests live in [`python/tests/`](../python/tests/) and run with `pytest`
(`asyncio_mode = "auto"`). Run every command from `python/`.

| Directory | What it covers | Needs |
|---|---|---|
| `tests/unit/` | every module in isolation: safety classifier and egress, sandboxes (with fakes), the egress proxy (`test_egress_proxy.py`), tools, dispatcher, model backends over mocked HTTP, pricing and retry, governor and metering, anchor, checklist import (`test_checklist_import.py`), verifier, witnesses (`test_witnesses.py`), trusted runner (`test_trusted_runner.py`), agent loop, planner, reviewer, orchestrator, memory, coordination, CLI wiring, redaction, `spec/` conformance | nothing |
| `tests/unit/test_large_missions.py` | the large-mission features through the real loop, dispatcher, verifier, `local` sandbox and git anchor with a scripted model: witnesses, trusted checks, protected paths, replanning of blocked items (and its budget and depth limits), approval gates and fingerprints, `fetch_url` registration, references and `lha vendor` | nothing |
| `tests/durability/` | `MissionWorkflow` and `SubAgentWorkflow` on a real Temporal test server: completion, crash after and before commit, Continue-As-New, deadlock, outage park and resume, budget exhaustion, gate decisions, durable approval of irreversible actions (`test_approvals.py`: `WAITING_ON_HUMAN` with the question in `open_question`, an approval reaches the next cycle once, a rejected action is not asked again), ClaimCheck, object store, saga, reconcile, history replay | the Temporal test server (downloaded automatically) |
| `tests/load/` | `test_long_run.py`: many cycles with Continue-As-New firing repeatedly; history stays bounded and every item completes once | the Temporal test server |
| `tests/integration/` | real Postgres + pgvector (migrations, schema, cost ledger, mission repo, semantic index); the real Docker sandbox (exit codes, timeouts, OOM, read-only harness dirs, no network); the egress proxy against real Docker and the internet (`test_docker_egress.py`); and one mission that uses every large-mission feature together (`test_large_mission_e2e.py`) | opt-in, see below |

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

## The replay test

[`tests/durability/test_replay.py`](../python/tests/durability/test_replay.py) guards workflow
determinism:

- `test_fresh_history_replays` records a three-item mission and replays it.
- `test_recorded_histories_still_replay` replays every history in
  `tests/durability/histories/` (currently `mission_three_items.json`) against the current
  `MissionWorkflow`. If a code change makes an in-flight mission non-deterministic, this fails.
- `test_replay_detects_a_changed_workflow` renames the activity in the recorded history and
  requires replay to fail with a non-determinism error.

To re-record after an intentional incompatible change:

```bash
LHA_RECORD_HISTORY=1 uv run pytest tests/durability/test_replay.py
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
- Omitted: `execution/sandbox_e2b.py` (needs an E2B account) and `obs/langfuse_exporter.py` (needs
  the optional `langfuse` package and a server).
- `fail_under = 90` with `precision = 2`, so 89.95% fails.

Coverage flags are not in pytest `addopts`; pass them explicitly:

```bash
uv run pytest -q --cov --cov-report= tests/unit tests/durability tests/load
uv run coverage report
```

The floor applies to unit + durability + load together. Integration tests are not included in the
measured total.

## Static checks

```bash
uv run ruff check .           # lint: rules E, F, I, UP, B, SIM, RUF; line length 100
uv run ruff format --check .  # formatting
uv run ty check               # types for src/, Python 3.12; warnings are errors
```

`ty` is told to ignore unresolved imports of optional extras (`psycopg`, `docker`, `e2b`,
`langfuse`, `opentelemetry`, `claude_agent_sdk`, and others), so the check passes without them
installed.

## What CI enforces

On every push to `main` and every pull request:

| Job | Steps |
|---|---|
| `python-check` | `uv sync --locked`; `ruff check`; `ruff format --check`; `ty check`; `pytest tests/unit` with coverage; `pytest tests/durability tests/load` with coverage appended (12-minute timeout); `coverage report` (fails under 90%) |
| `python-services-integration` | a `pgvector/pgvector:pg16` service container; `uv sync --locked --extra postgres --extra sandbox`; Go 1.26 (for the trusted `go run` check); `docker build -t lha-sandbox:dev sandbox/`; `pytest tests/integration` with `LHA_IT_POSTGRES_DSN` and `LHA_IT_DOCKER=1` set (20-minute timeout) |

A separate workflow, [`docs-site.yml`](../.github/workflows/docs-site.yml), builds this
documentation site on changes to `docs/` or `website/`.

No CI job builds or tests the Go module.

## Spec conformance tests

The cases in [`spec/`](../spec/) pin behaviour both implementations must share (see
[19-wire-contract.md](19-wire-contract.md#conformance-cases-spec)).

```bash
cd python && uv run pytest -q tests/unit/test_spec_conformance.py
cd go && go test ./internal/spec/
```

Python runs all nine files. Go runs seven; `coordination/decision_chain.json` and
`coordination/shared_paths.json` wait for the Go package that implements them.

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

Packages with tests: `config`, `contracts`, `governor`, `model`, `obs`, `safety`, `spec`,
`state`, `verify`. `internal/state/crossimpl_test.go` writes a mission anchor with Go and reads it with the
Python implementation (via `uv run --project ../python`), and the reverse; it skips when `uv` is
not on `PATH` or under `-short`. `internal/safety/zz_pydiff_test.go` compares Go egress host,
URL and address handling against JSON dumps from Python in the directory named by `LHA_PYDIFF`;
it skips when that variable is unset.
