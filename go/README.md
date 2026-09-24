# LHA — Go implementation

The Go implementation of the `lha` CLI and worker: one static binary, wire-compatible with the
Python implementation in `../python/` (same commands, `LHA_*` settings, `.lha/` mission anchor,
Postgres schema, and Temporal workflow/activity names and payloads). See the
[project README](../README.md) and [`spec/`](../spec/) for the shared conformance cases.

The Go port is landing in phases. `cmd/lha` builds the CLI (`go build -o lha ./cmd/lha`) with
`version`, `config`, `run-local`, `mission` and `decisions`; runs need the execution layer
(sandboxes and tools), which links in through `cmd/lha/wiring.go`, and there is no Temporal
worker yet, so use the Python implementation to run missions today. See
[choosing an implementation](../docs/04-choosing-an-implementation.md) for what is ported.

Tests, vet and formatting (from this directory):

```bash
gofmt -l . && go vet ./... && go test ./...
```

Layout mirrors the Python packages. Present today: `internal/contracts` (shared types),
`internal/config` (`LHA_*` settings), `internal/spec` (the conformance runner),
`internal/safety` (command classifier, egress policy, Rule of Two), `internal/model` (backends,
pricing, retry, failover), `internal/state` (git ops, the mission anchor and the hash-chained
decision log), `internal/checklistimport`, `internal/verify` (verifier, harness integrity,
witnesses, trusted runner), `internal/governor`, `internal/obs`, `internal/agent` (prompts, the
turn loop, compaction, the local runner), `internal/agents` (Planner, Replanner),
`internal/pyfmt` (Python string semantics for byte-identical prompts) and `cmd/lha`. Not started:
`internal/execution`, the approval gates, memory, persistence and the durable worker.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
