<h1 align="center">LHA — Long-Horizon Agent</h1>

<p align="center">
  <em>A durable agent organization that makes verified progress on software missions spanning
  days to weeks, surviving crashes, reboots and context-window limits.</em>
</p>

<p align="center">
  <a href="https://calvinchengx.github.io/long-horizon-agent/">Documentation</a> ·
  <a href="https://calvinchengx.github.io/long-horizon-agent/02-quickstart/">Quickstart</a> ·
  <a href="https://calvinchengx.github.io/long-horizon-agent/05-architecture/">Architecture</a> ·
  <a href="spec/">Conformance spec</a>
</p>

---

> **Status: early, work in progress.** The durable single-agent spine is built and tested; the
> multi-agent organization is layered on where it measurably helps. See
> [what is proven and what is frontier](docs/22-honesty.md).

## What it is

Most autonomous agents are one LLM loop that dies when the process restarts, the context fills
or an API call fails. LHA treats long-horizon autonomy as an engineering problem:

- **Truth lives outside the model.** A git repo plus a structured checklist, progress log and
  decision log (the `.lha/` mission anchor) is re-read every cycle, so a restart rebuilds
  situational awareness in seconds.
- **Durable execution.** Missions run as [Temporal](https://temporal.io) workflows: each agent
  cycle is a journaled activity, so a crashed worker resumes from the last completed cycle (the
  cycle in flight is retried), and waiting on an outage, a schedule or a human is a durable
  timer.
- **Deterministic verification.** An item is done only when real checks (tests, lint, type
  checks) pass in the sandbox, never on the model's say-so.
- **Guardrails.** Sandboxed execution; irreversible commands (`git push`, deploys, uploads) go
  to a human for approval (durable missions wait as `WAITING_ON_HUMAN`, local runs can ask on
  the terminal with `--approve-interactive`, otherwise they are refused), with reminders on an
  escalation ladder and an optional webhook; default-deny network egress, with an optional
  per-host allow-list for the sandbox and for the `fetch_url` tool; and a budget governor that
  refuses spend before it happens.
- **A record of the run.** Every run keeps a mission row, a per-call cost ledger and its human
  gates (SQLite by default, Postgres optionally; `lha missions`, `lha costs`, `lha gates`),
  recalls tiered memory into the
  prompt, and appends design decisions to a hash-chained log (`lha decisions --verify`).

## Two implementations

The `lha` CLI and worker are implemented in [Python](python/). The [Go](go/) port is one static
binary that runs single-agent missions locally (`lha run-local`, `lha mission`) and durably on
its own Temporal worker (`lha worker`, `lha mission-*`) with the same tools, gates and `.lha/`
anchor; the durable organization, `orchestrate`, persistence and memory are Python-only for now,
and a Go and a Python worker must use different task queues. The two share
`LHA_*` settings, the `.lha/` anchor format (including the decision-log hash chain), the Postgres
schema and the Temporal workflow and activity names, and both run the language-neutral cases in
[`spec/`](spec/); see [choosing an implementation](docs/04-choosing-an-implementation.md).

## Quickstart

```bash
cd python
uv sync
LHA_MODEL_BACKEND=ollama LHA_MODEL_NAME=qwen3:8b \
  uv run lha mission --task "Create hello.py with hello() returning 'hello', and a pytest test" \
  --workdir ../.lha/workspaces/demo --sandbox local --unsafe-local \
  --no-default-checks --check "uv run --with pytest pytest -q"
git -C ../.lha/workspaces/demo log --oneline
```

This runs at $0 against a local [Ollama](https://ollama.com). The mission is done only when the
check passes: if the model's code fails it three times in a row, the item is blocked (the model
may first split it into smaller items) and the run ends `deadlocked` instead of accepting broken
work. That is also what you see with the default
`stub` model, which writes nothing, and it can happen with small local models. `--sandbox local` runs commands on your host
without isolation; the default `docker` sandbox is the one to use for anything you did not write.
The [quickstart](docs/02-quickstart.md) covers real models, Temporal and the full organization.

To run it with Claude Code instead, on a Claude Pro/Max login or an API key, set
`LHA_LEAD_ENGINE=claude_code`: each cycle becomes one `claude -p` session that uses LHA's
sandboxed tools, and LHA still verifies and commits the result. See
[models](docs/13-models.md#claude_code-claude-code-claude--p).

## Repository layout

| Path | Contents |
|---|---|
| [`python/`](python/) | Python implementation (uv, Python 3.12) |
| [`go/`](go/) | Go implementation (one static binary), being ported in phases |
| [`spec/`](spec/) | Conformance cases both implementations must pass |
| [`db/migrations/`](db/migrations/) | Postgres schema shared by both |
| [`docs/`](docs/) | Documentation source, published by [`website/`](website/) |
| [`docker-compose.yml`](docker-compose.yml) | Local Temporal, Postgres + pgvector and Langfuse |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and the checks every change must pass, and
[SECURITY.md](SECURITY.md) to report a vulnerability.

## Author and license

Created and maintained by **Calvin Cheng** ([calvin@calvinx.com](mailto:calvin@calvinx.com)).
MIT, Copyright (c) 2026 Calvin Cheng. See [LICENSE](LICENSE).
