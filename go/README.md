# LHA — Go implementation

The Go implementation of the `lha` CLI and worker: one static binary, wire-compatible with the
Python implementation in `../python/` (same commands, `LHA_*` settings, `.lha/` mission anchor,
Postgres schema, and Temporal workflow/activity names and payloads). See the
[project README](../README.md) and [`spec/`](../spec/) for the shared conformance cases.

The Go port is landing in phases, and the `lha` command itself (`cmd/lha`) is not built yet;
use the Python implementation to run missions today. See
[choosing an implementation](../docs/04-choosing-an-implementation.md) for what is ported.

Tests, vet and formatting (from this directory):

```bash
gofmt -l . && go vet ./... && go test ./...
```

Layout mirrors the Python packages: `internal/contracts` (shared types), `internal/config`
(`LHA_*` settings) and `internal/spec` (the conformance runner) today; `internal/safety`,
`internal/model`, `internal/state`, `internal/verify`, `internal/governor` and `internal/obs` as
phase 1 lands, then `internal/execution`, `internal/agent` and `cmd/lha`.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
