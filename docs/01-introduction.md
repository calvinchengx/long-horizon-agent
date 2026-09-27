# Introduction

LHA (Long-Horizon Agent) runs software missions that are expected to take days: a task is
planned into a checklist, and an agent works through it one item per cycle, committing each
cycle to git. The goal is that a mission survives process crashes, reboots, provider outages and
context-window limits without losing track of what it was doing or claiming work it did not do.

The repository contains a Python implementation ([`python/`](../python/)), a Go port
([`go/`](../go/)), language-neutral conformance cases shared by both ([`spec/`](../spec/)), the
Postgres schema ([`db/migrations/`](../db/migrations/)) and a local service stack
([`docker-compose.yml`](../docker-compose.yml)).

## The problem

A single LLM loop is a poor unit for multi-day work:

- Its state lives in the context window, which is lost on restart and degraded by compaction.
- A crash or API error mid-run ends the run.
- The model's own statement that a step is done is not evidence that it is done, and small
  per-step errors compound over many steps.

LHA treats these as system-design problems. The model is used in short bursts (one checklist
item per cycle); everything that has to persist lives outside it.

## Core ideas

### Truth outside the model: the mission anchor

Each mission's workspace is a git repository with a `.lha/` directory holding the mission spec,
the checklist, a progress log, a decision log and an event log. Every cycle starts by reading
this anchor from the committed `HEAD`, not from the model's memory, and ends by committing it
together with the code changes. The mission spec is recited at the top of every cycle's prompt.
Because every cycle re-reads the anchor, a restart is handled the same way as a normal cycle
start ("assume interruption"). See [the mission anchor](06-mission-anchor.md).

### Durable execution

On the durable path, a mission is a Temporal workflow (`MissionWorkflow`). The workflow body is
a deterministic scheduler; each agent cycle runs as one `run_agent_cycle` activity whose result
Temporal journals. After a worker crash, completed cycles are replayed from history rather than
re-run; a cycle that was in flight is retried from a clean checkout. Transient failures park the
mission (`DEGRADED_PARK`) with a durable, backed-off sleep instead of failing it, and it resumes
when a health probe (git, a real model request, the sandbox) passes. A mission can also sleep by
design (`SLEEPING`): a scheduled start, a pause between cycles or an operator snooze. See
[architecture](05-architecture.md) and [durable execution](08-durable-execution.md).

The unit of journaling is the cycle, not the individual model call: model calls made by an
attempt that crashed are made again (and paid for again) by the retry.

### Deterministic verification

An item becomes `done` only when the deterministic verifier returns a `passed` verdict: at
least one gating check (an argv such as `uv run pytest -q`) ran in the sandbox and every gating
check exited 0. An item can also name its own acceptance checks (`witnesses`, such as
`go:TestLivy`), which must pass too. A model saying "done" only ends its turn loop. Zero gating
checks is `unverified`, never a pass. Pre-existing tests and test configuration (plus any paths
the operator protects) are hashed at cycle start so the agent cannot pass the gate by weakening
it. See [verification](07-verification.md).

### Guardrails

- **Sandboxed execution.** Tool calls and checks run in a sandbox: `docker` (default), `e2b`, or
  `local`. `local` has no isolation and is refused unless explicitly enabled.
- **Tool dispatcher.** Every tool call passes an allow-list, JSON-schema argument validation,
  workspace path containment, and write protection for `.lha/` and `.git/`.
- **Human gate on irreversible commands.** Shell commands classified as irreversible or
  outward-facing (for example `git push`) are sent to a human. A durable mission queues the exact
  call and waits as `WAITING_ON_HUMAN` for `lha mission-approve`; a local run asks on the
  terminal with `--approve-interactive`; otherwise the command is denied. An open gate sends
  reminders on an escalation ladder (optionally to a webhook, `LHA_GATE_WEBHOOK_URL`) and applies
  its default action on timeout. A durable mission that deadlocks opens a gate too: retry, abort,
  or declare the mission impossible.
- **Default-deny egress.** The Docker sandbox has no network unless the operator lists hosts in
  `LHA_SANDBOX_EGRESS` (package-registry download hosts only; other hosts, and hosts that accept
  pushes or uploads, need their own settings), which a proxy on an internal Docker network
  enforces. Sandbox egress counts against the Rule of Two like web access. The lead gets the
  `fetch_url` tool (and `web_search`, if a provider is configured) only when
  `LHA_WEB_ALLOW_HOSTS` or `--allow-host` lists hosts it may read; fetched content is marked
  untrusted, and a run that would combine web access with private data or a `local` sandbox is
  refused (Rule of Two).
- **Budget governor.** Each model call is authorized before it runs against a USD ceiling
  (`LHA_BUDGET_USD_CEILING`, default 10.0). Calls whose cost cannot be computed are refused
  unless `LHA_ALLOW_UNPRICED_MODELS=true`.

### Fast judgments, never verdicts

LHA can optionally use a second kind of model: a **System One decision model**, such as TypeSafe's
hosted Jev or the open-weights Kev. It answers typed questions (yes/no, pick an option, rate on a
scale) with calibrated probabilities, in about 100 ms and for a fraction of an LLM call, and
generates no text. LHA asks it only questions where a wrong answer costs effort, not safety. When
an item keeps failing, triage asks whether the item is too big (split it now) or the environment is
broken (hand it to a human now). It can also rerank recalled memory. It can never allow an action,
answer a gate or mark work done, and when it is unavailable LHA behaves exactly as without it. Off
by default; see [System One decision models](25-system-one.md).

### Honesty policy

Numbers reported by LHA (tokens, cost) come from the provider responses. The `stub` model is a
deterministic test double; its model name is prefixed `stub:` and it is never presented as a real
agent run. Predicted outputs (in [`predicted-runs/`](predicted-runs/)) are labeled as predictions.
Anything shown as a real run uses real model output or a labeled recorded replay.

## Current status

The project is early and under active development.

| Area | State |
|---|---|
| Python single-agent spine (`lha mission`, `lha run-local`) | Implemented and tested |
| Python durable spine (`lha worker`, `lha mission-start`) | Implemented; durability and replay tests run against the Temporal test server in CI |
| Large-mission features: imported checklists with witnesses, trusted checks, protected paths, replanning of blocked items, sandbox image and egress allow-list, vendored references, human approval of irreversible actions | Implemented and wired into every Python run path (local, durable, `orchestrate`); an end-to-end test exercises them together on real Docker |
| Python multi-agent flow: researchers, lead, reviewer, parallel implementers | Implemented locally (`lha orchestrate`, resumable with `--resume`) and, opt-in per mission, on Temporal (`lha mission-start --research N --review --max-parallel N`) |
| Human approval gates, escalation ladder and gate webhook, `SLEEPING` (scheduled start, cycle pause, `lha mission-snooze`), the retry/abort/impossible deadlock gate | Implemented and wired. Every gate event is written to the `hitl_gates` table (`lha gates`); the workflow writes its own states (`DEGRADED_PARK`, `SLEEPING`, an open gate's `WAITING_ON_HUMAN`, every final status) to the mission row, best effort |
| Web tools (`fetch_url`, `web_search`) with egress policy, credential broker, untrusted-content fencing and the Rule of Two preflight | Implemented and wired when `LHA_WEB_ALLOW_HOSTS` or `--allow-host` is non-empty. Connections go only to the addresses that were checked (no DNS-rebinding window) |
| Fallback model chain (`LHA_FALLBACK_MODELS`) and a real model health probe for parked missions | Implemented and wired. Fallbacks of one backend share its endpoint (for example `LHA_OPENAI_BASE_URL`) |
| Persistence (SQLite by default, Postgres with `LHA_POSTGRES_DSN`): mission rows, the cost ledger and human gates (`lha missions`, `lha costs`, `lha gates`) | Implemented and wired into every run path |
| Tiered memory (episodic, semantic, skills) in the lead's prompt, with degradation to BM25 and `git grep` | Implemented and wired into every run path. The default `hash` embedder is not semantic; `LHA_MEMORY_EMBEDDER=ollama` uses a local Ollama for semantic embeddings at $0 |
| Hash-chained decision log (`record_decision`, `lha decisions --verify`) | Implemented and wired; a broken chain stops the run |
| File ownership with lease granting, tickets, parallel implementer waves in git worktrees merged by the `BranchIntegrator` | Wired into `lha orchestrate` and into durable missions started with `--max-parallel`; the blackboard and reflection only in `orchestrate` |
| Flaky-check quarantine in the verifier; OTLP trace export (a collector or Langfuse) | Implemented and wired into every Python run path (the Lead's cycle, parallel implementers and branch integration alike); export is off until an endpoint or the Langfuse keys are set and needs the `observability` extra |
| System One decision models (Jev, Kev): stall triage and memory reranking | Implemented and wired into every run path in Python and Go, off by default; tested with a stub and mocked HTTP, not against a live Jev or Kev ([25](25-system-one.md)) |
| Go port | A `go/cmd/lha` binary with every `lha` command: missions run locally (`run-local`, `mission`, and the organization with `orchestrate`) in the local or Docker sandbox, and missions durably on Temporal (`worker`, `mission-*`, the durable organization's research / review / parallel rounds included), with the same tools, gates, `.lha/` anchor, mission store and tiered memory as Python; everything except E2B and the `sentence_transformers` / `cross_encoder` extras, and its worker cannot share a task queue with Python workers ([04](04-choosing-an-implementation.md)) |
| Fully hands-off multi-week autonomy | Not claimed. The system is built to run for weeks; the model advances it in verified bursts |

## Where to go next

- [Quickstart](02-quickstart.md): run a first mission.
- [Installation](03-installation.md): Python, Go, Docker images and the compose stack.
- [Choosing an implementation](04-choosing-an-implementation.md): Python versus Go today.
- [Architecture](05-architecture.md): the planes, the organization and the cycle.
- [The mission anchor](06-mission-anchor.md): the `.lha/` files and item lifecycle.
- [Verification](07-verification.md): what "done" means.
