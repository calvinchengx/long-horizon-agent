# LHA — Go implementation

The Go implementation of the `lha` CLI: one static binary, wire-compatible with the Python
implementation in `../python/` (same commands, `LHA_*` settings and `.lha/` mission anchor). See
the [project README](../README.md) and [`spec/`](../spec/) for the shared conformance cases, and
[choosing an implementation](../docs/04-choosing-an-implementation.md) for what is ported.

```bash
go build -o lha ./cmd/lha
LHA_SANDBOX=local LHA_ALLOW_UNSAFE_LOCAL=true ./lha run-local --item "say hello" \
  --no-default-checks --check true --workdir /tmp/lha-demo
```

## What the Go CLI runs today

- `version`, `config`, `decisions` and the local mission commands `run-local` and `mission`
  (plan a task, or import a `.json`/`.md` checklist, then run it), with Python's options, output
  and exit codes. For the same inputs a Go run leaves the same checkpoint commits and `.lha/`
  anchor as a Python run (`cmd/lha/e2e_test.go` runs both side by side).
- `orchestrate`: the multi-agent organization (`internal/agents/org`, `internal/coordination`):
  research fan-out, the Lead, reflection, the fresh-context Reviewer, parallel implementer waves in
  worktrees with leases, and the integrator; `--checklist` skips planning and `--resume`
  continues a mission anchored by either implementation (`cmd/lha/orchestrate_test.go` runs
  both side by side, and resumes each one's interrupted mission with the other).
- Sandboxes: `local` (only with `LHA_ALLOW_UNSAFE_LOCAL=true` / `--unsafe-local`) and `docker`
  (`LHA_SANDBOX_IMAGE`; the `LHA_SANDBOX_EGRESS*` settings route egress through a per-session
  allow-list proxy, and its requests are committed as `sandbox_egress` events).
- The lead's tools: `read_file`, `write_file`, `list_files`, `grep`, `run_command` and
  `record_decision`, plus `fetch_url` / `web_search` when `LHA_WEB_ALLOW_HOSTS` or `--allow-host`
  is set. An unsafe local sandbox and a Rule-of-Two (lethal trifecta) run are refused before the
  workspace is touched (exit 2); bad web settings are refused before any model spend.
- Irreversible commands (`git push`, publishing, uploads) are refused, or with
  `--approve-interactive` asked on the terminal (`internal/hitl`: the exact argv and the
  classifier's reason, y/N, reminders at `LHA_GATE_ESCALATION_SECONDS`, rejection after
  `LHA_CONSOLE_APPROVAL_TIMEOUT_S` or without a TTY, the optional `LHA_GATE_WEBHOOK_URL`). The
  prompt text, the `gate_reminder` / `tool_approval` events and the webhook body are
  byte-identical to Python's.
- Flaky checks: a failing gating check is re-run up to `LHA_FLAKY_RETRIES` times; a check that
  passed and failed on the same work tree is quarantined (never the evidence for green) with a
  committed `check_quarantined` event, read back from `HEAD` by later runs, exactly as in Python
  (`spec/verify/flaky_retry.json`).
- Model backends `stub`, `ollama`, `openai_compat`, `claude` and `claude_code` (`claude -p`), and
  the `claude_code` lead engine (`LHA_LEAD_ENGINE=claude_code`: one `claude -p` session per
  cycle, with LHA's tools served over MCP by `internal/agent/mcpbridge`); `LHA_FALLBACK_MODELS`
  failover chains and the model health probe (`model.ProbeModel`).
- `vendor`: reference pages snapshotted with the egress rules and a DNS-pinned fetch
  (`internal/state/vendor`, `safety.PinnedDialer`); the same files and `MANIFEST.json` as Python.
- OTLP/HTTP trace export (`LHA_OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_ENDPOINT` or
  Langfuse) with Python's span names and redacted attributes (`internal/obs/tracing`).
- The mission store (`internal/persistence`): `run-local`, `mission` and `orchestrate` write the
  mission row and every metered model call (the Planner's, every org role's, and a `claude_code`
  session's reported cost included) to the per-user SQLite file (`LHA_SQLITE_PATH`, pure-Go
  driver) or Postgres (`LHA_POSTGRES_DSN`, falling back to SQLite unless
  `LHA_POSTGRES_FALLBACK_TO_SQLITE=false`), and terminal gates to `hitl_gates`. `missions`,
  `costs`, `gates` and `db migrate` read and migrate it with Python's output. The tables, JSON
  columns and ledger keys are Python's: either implementation reads what the other wrote.
- Tiered memory (`internal/memory`): episodic recall, skills and hybrid retrieval (BM25 + the
  `hash` or `ollama` embedder, pgvector on Postgres) in the lead's prompt, for the turn loop and
  the `claude_code` engine alike, with consolidation and the same degradation to BM25 +
  `git grep`; the memory block is byte-identical to Python's (`spec/memory/`).

## Python-only

The Temporal worker and the durable commands `worker`, `mission-start`, `mission-status`,
`mission-approve`, `mission-abort` and `mission-snooze` (and with them the durable multi-agent
organization rounds); the E2B sandbox (E2B has no Go SDK; see
[choosing an implementation](../docs/04-choosing-an-implementation.md#e2b-is-not-supported-in-go));
the `sentence_transformers` embedder and `cross_encoder` reranker (Python extras: Go degrades as
Python does without them) and the library-only `VoyageEmbedder`. The unported commands print
that they are not available and exit 2.

The Docker sandbox's egress proxy container runs the stdlib-only Python proxy source on
`python:3.12-alpine` by default, exactly like Python. `internal/execution/egressproxy` is the same
proxy in Go, served by the hidden `lha egress-proxy` command (`LHA_PROXY_ALLOW`, `LHA_PROXY_PORT`,
`LHA_PROXY_BIND`); the sandbox uses it only when given `DockerOptions.ProxyCommand` and an image
that contains a Linux `lha` binary, which no setting selects yet.

## Tests

```bash
gofmt -l . && go vet ./... && go test ./...
```

Tests that compare with Python (`cmd/lha/e2e_test.go`, `cmd/lha/orchestrate_test.go`,
`cmd/lha/claude_code_test.go`, `cmd/lha/store_cmds_test.go`, `internal/hitl`, `internal/state`,
`internal/persistence`) run it with `uv run --project ../python` and skip that half when `uv` is not on `PATH`. The Docker
tests against a real daemon are opt-in, like Python's integration tests:
`LHA_IT_DOCKER=1 go test ./internal/execution/ ./cmd/lha/ -run Docker`. The Postgres tests
(the store, pgvector memory, a run on Postgres, and the store shared with Python) need an admin
DSN of a Postgres with pgvector:

```bash
docker run -d --rm --name lha-pg -e POSTGRES_USER=lha -e POSTGRES_PASSWORD=lha -e POSTGRES_DB=lha \
  -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16
LHA_IT_POSTGRES_DSN=postgresql://lha:lha@127.0.0.1:55432/lha go test ./internal/persistence/ ./internal/memory/ ./internal/agent/
```

## Layout

Layout mirrors the Python packages: `internal/contracts` (shared types), `internal/config`
(`LHA_*` settings), `internal/spec` (the conformance runner), `internal/safety` (command
classifier, egress policy, Rule of Two), `internal/model` (backends, pricing, retry, failover),
`internal/state` (git ops, the mission anchor and the hash-chained decision log),
`internal/persistence` (the mission store; `services` opens store, ledger sink, mission tracker
and memory for a run), `internal/memory` (the memory plane), `internal/ops` (degradation rules),
`internal/checklistimport`, `internal/verify` (verifier, harness integrity, witnesses, trusted
runner), `internal/governor`, `internal/obs`, `internal/execution` (local and Docker sandboxes,
the egress proxy, path containment, the allow-list dispatcher and the tools), `internal/hitl`
(the console approval gate, escalation ladder and gate webhook), `internal/agent` (prompts, the
turn loop, compaction, the local runner, the `claude_code` engine and its MCP bridge),
`internal/agents` (Planner, Replanner; `org`: the multi-agent organization),
`internal/coordination` (ownership, leases, tickets),
`internal/pyfmt` (Python string semantics for byte-identical prompts) and `cmd/lha`, whose
`wiring.go` links the execution layer into the runner.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
