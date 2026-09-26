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
  cycle, with LHA's tools served over MCP by `internal/agent/mcpbridge`).

## Python-only

The Temporal worker and the `worker`, `mission-start`, `mission-status`, `mission-approve`,
`mission-abort`, `mission-snooze` and `missions` commands; `orchestrate` (the multi-agent
organization); the mission store and persistent cost ledger (`costs`, `db`; Go runs write no
mission rows) and tiered memory; `vendor`; the
E2B sandbox; fallback model chains
(`LHA_FALLBACK_MODELS`); OTLP trace export. The unported commands print that they are not
available and exit 2.

The Docker sandbox's egress proxy container runs the stdlib-only Python proxy source on
`python:3.12-alpine` by default, exactly like Python. `internal/execution/egressproxy` is the same
proxy in Go, served by the hidden `lha egress-proxy` command (`LHA_PROXY_ALLOW`, `LHA_PROXY_PORT`,
`LHA_PROXY_BIND`); the sandbox uses it only when given `DockerOptions.ProxyCommand` and an image
that contains a Linux `lha` binary, which no setting selects yet.

## Tests

```bash
gofmt -l . && go vet ./... && go test ./...
```

Tests that compare with Python (`cmd/lha/e2e_test.go`, `internal/hitl`, `internal/state`) run
it with `uv run --project ../python` and skip that half when `uv` is not on `PATH`. The Docker
tests against a real daemon are opt-in, like Python's integration tests:
`LHA_IT_DOCKER=1 go test ./internal/execution/ ./cmd/lha/ -run Docker`.

## Layout

Layout mirrors the Python packages: `internal/contracts` (shared types), `internal/config`
(`LHA_*` settings), `internal/spec` (the conformance runner), `internal/safety` (command
classifier, egress policy, Rule of Two), `internal/model` (backends, pricing, retry, failover),
`internal/state` (git ops, the mission anchor and the hash-chained decision log),
`internal/checklistimport`, `internal/verify` (verifier, harness integrity, witnesses, trusted
runner), `internal/governor`, `internal/obs`, `internal/execution` (local and Docker sandboxes,
the egress proxy, path containment, the allow-list dispatcher and the tools), `internal/hitl`
(the console approval gate, escalation ladder and gate webhook), `internal/agent` (prompts, the
turn loop, compaction, the local runner), `internal/agents` (Planner, Replanner),
`internal/pyfmt` (Python string semantics for byte-identical prompts) and `cmd/lha`, whose
`wiring.go` links the execution layer into the runner.

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.
