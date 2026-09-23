# LHA — Go implementation

The Go implementation of the `lha` CLI and worker: one static binary, wire-compatible with the
Python implementation in `../python/` (same commands, `LHA_*` settings, `.lha/` mission anchor,
Postgres schema, and Temporal workflow/activity names and payloads). See the
[project README](../README.md) and [`spec/`](../spec/) for the shared conformance cases.

The Go port is landing in phases. There is no `lha` command (`cmd/lha` does not exist yet) and
no Temporal worker, so use the Python implementation to run missions today. See
[choosing an implementation](../docs/04-choosing-an-implementation.md) for what is ported.

Tests, vet and formatting (from this directory):

```bash
gofmt -l . && go vet ./... && go test ./...
```

Layout mirrors the Python packages. Present today: `internal/contracts` (shared types),
`internal/config` (`LHA_*` settings), `internal/spec` (the conformance runner),
`internal/safety` (command classifier, egress policy, Rule of Two), `internal/model` (backends,
pricing, retry, failover), `internal/state` (git ops, the mission anchor and the hash-chained
decision log), `internal/verify`, `internal/governor` and `internal/obs`. Not started:
`internal/execution`, `internal/agent`, the approval gates, memory, persistence, the durable
worker and `cmd/lha`.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
