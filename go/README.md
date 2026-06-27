# LHA — Go implementation

The Go implementation of the `lha` CLI and worker: one static binary, wire-compatible with the
Python implementation in `../python/` (same commands, `LHA_*` settings, `.lha/` mission anchor,
Postgres schema, and Temporal workflow/activity names and payloads). See the
[project README](../README.md) and [`spec/`](../spec/) for the shared conformance cases.

```bash
cd go
go build -o lha ./cmd/lha
./lha --help
```

Tests, vet and formatting (from this directory):

```bash
gofmt -l . && go vet ./... && go test ./...
```

Layout mirrors the Python packages: `internal/contracts` (shared types), `internal/config`
(`LHA_*` settings), `internal/safety`, `internal/execution`, `internal/model`, `internal/state`,
`internal/verify`, `internal/governor`, `internal/obs`, `internal/agent`, and `cmd/lha`.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
