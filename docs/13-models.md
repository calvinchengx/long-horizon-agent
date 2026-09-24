# Models

Every model call in LHA goes through one interface, `ModelProvider`
([`contracts/model.py`](../python/src/lha/contracts/model.py)). `build_provider` in
[`model/__init__.py`](../python/src/lha/model/__init__.py) is the only place a concrete backend is
chosen, from `LHA_MODEL_BACKEND` (plus an optional fallback chain, see
[Failover](#retries-and-failover)). Five backends exist:

| `LHA_MODEL_BACKEND` | Class | Transport | Cost source |
|---|---|---|---|
| `stub` (default) | `StubModel` | none (in-process) | always `$0` |
| `ollama` | `OpenAICompatModel` (label `ollama`) | `POST {LHA_OLLAMA_BASE_URL}/v1/chat/completions` | fixed `$0` |
| `openai_compat` | `OpenAICompatModel` | `POST {LHA_OPENAI_BASE_URL}/chat/completions` | `LHA_OPENAI_PRICE_*`; unknown if unset |
| `claude` | `ClaudeModel` | `POST https://api.anthropic.com/v1/messages` | built-in table, or `LHA_CLAUDE_PRICE_*` |
| `claude_code` | `ClaudeCodeModel` | the `claude -p` CLI (Claude Code) | the `total_cost_usd` Claude Code reports |

Every provider is wrapped in a `MeteredModel` (see [10-cost-and-budget.md](10-cost-and-budget.md)):
the budget governor checks each call's worst-case cost *before* it is sent and records the actual
cost *after*. Token counts come from the provider response (`usage`); nothing is estimated after
the fact.

## `stub`: tests and CI only

`StubModel` ([`model/stub.py`](../python/src/lha/model/stub.py)) is a deterministic test double.
Its name is always `stub:<LHA_MODEL_NAME>` so it cannot be mistaken for a real model in logs. With
no script it returns an echo such as `[stub:1a2b3c4d] acknowledged 3 message(s).` with synthetic
token counts, and no tool calls. Tests pass a `script` of canned `TurnResult`s to drive specific
scenarios (for example, "fail verification once, then fix it").

The stub exists so the durable spine, the anchor, the verifier and the CLI can be exercised
offline at `$0`. It does not write code: a mission run on the stub exercises the plumbing, and its
items will not pass real checks. Output from the stub is never presented as a real agent run (see
[22-honesty.md](22-honesty.md)).

## `ollama`: local models

| Setting | Default | Notes |
|---|---|---|
| `LHA_MODEL_BACKEND` | `stub` | set to `ollama` |
| `LHA_MODEL_NAME` | `stub-1` | an Ollama model tag you have pulled |
| `LHA_OLLAMA_BASE_URL` | `http://localhost:11434` | `/v1` is appended |

Ollama is reached through its OpenAI-compatible endpoint with a placeholder bearer token
(`ollama`). Prices are fixed at `0.0` in and out, so spend is recorded as a genuine `$0` while
tokens are still counted.

```bash
ollama pull qwen3:8b
cd python
LHA_MODEL_BACKEND=ollama LHA_MODEL_NAME=qwen3:8b \
  uv run lha mission --task "Add a slugify() helper with tests" --sandbox docker
```

The agent loop ([`agent/loop.py`](../python/src/lha/agent/loop.py)) does not pass a `tools`
parameter: tools are described in the prompt, and the model acts by replying with a JSON object
(`{"tool": ..., "arguments": ...}` or `{"done": true, ...}`). Native tool calls in a response are
also accepted. A model that cannot reliably produce that JSON gets corrective turns and makes no
progress.

## `openai_compat`: any chat-completions endpoint

| Setting | Default | Notes |
|---|---|---|
| `LHA_OPENAI_BASE_URL` | unset | required; `build_provider` raises `ValueError` without it |
| `LHA_OPENAI_API_KEY` | unset | sent as `Authorization: Bearer ...` when set |
| `LHA_MODEL_NAME` | `stub-1` | the provider's model id |
| `LHA_OPENAI_PRICE_IN_PER_MTOK` | unset | USD per 1M input tokens |
| `LHA_OPENAI_PRICE_OUT_PER_MTOK` | unset | USD per 1M output tokens |

Set both prices or neither; setting only one raises `ValueError` when the provider is built. With
no prices the cost of every call is **unknown**, not `$0`, and the governor refuses the call
unless `LHA_ALLOW_UNPRICED_MODELS=true` (see below). For a genuinely free endpoint, set both
prices to `0`.

Groq:

```bash
export LHA_MODEL_BACKEND=openai_compat
export LHA_OPENAI_BASE_URL=https://api.groq.com/openai/v1
export LHA_OPENAI_API_KEY=...            # from the Groq console
export LHA_MODEL_NAME=llama-3.3-70b-versatile
export LHA_OPENAI_PRICE_IN_PER_MTOK=0.59   # example values: use the provider's current prices
export LHA_OPENAI_PRICE_OUT_PER_MTOK=0.79
```

OpenRouter:

```bash
export LHA_MODEL_BACKEND=openai_compat
export LHA_OPENAI_BASE_URL=https://openrouter.ai/api/v1
export LHA_OPENAI_API_KEY=...
export LHA_MODEL_NAME=meta-llama/llama-3.3-70b-instruct
export LHA_OPENAI_PRICE_IN_PER_MTOK=...    # the listed price for that model
export LHA_OPENAI_PRICE_OUT_PER_MTOK=...
```

The configured prices apply to whatever model the endpoint reports in its response. Requests
always carry `max_tokens` (the call's value, else 8192) so the meter's worst-case reservation
bounds the real output. The HTTP timeout is 120 s.

## `claude`: the Anthropic Messages API

| Setting | Default | Notes |
|---|---|---|
| `LHA_ANTHROPIC_API_KEY` | unset | required; `build_provider` raises `ValueError` without it |
| `LHA_MODEL_NAME` | `stub-1` | a Claude model id |
| `LHA_CLAUDE_PRICE_IN_PER_MTOK` | unset | overrides the table for `LHA_MODEL_NAME` only |
| `LHA_CLAUDE_PRICE_OUT_PER_MTOK` | unset | as above; both must be set to take effect |

The built-in price table ([`model/pricing.py`](../python/src/lha/model/pricing.py), last checked
2026-06 against Anthropic's first-party rates; verify before relying on it):

| Model id | Input $/MTok | Output $/MTok |
|---|---|---|
| `claude-opus-4-8` | 5.00 | 25.00 |
| `claude-sonnet-4-6` | 3.00 | 15.00 |
| `claude-haiku-4-5` | 1.00 | 5.00 |

A dated snapshot suffix is accepted (`claude-haiku-4-5-20251001` prices as `claude-haiku-4-5`).
Prompt-cache tokens are priced separately: 5-minute cache writes at 1.25x input, 1-hour writes at
2x, cache reads at 0.1x. Cost is computed from the model the API *reports* as having served the
turn.

```bash
export LHA_MODEL_BACKEND=claude
export LHA_ANTHROPIC_API_KEY=...
export LHA_MODEL_NAME=claude-sonnet-4-6
cd python && uv run lha mission --task "..." --sandbox docker
```

A model id that is neither in the table nor covered by explicit prices fails when the provider is
constructed (`UnknownPriceError`), even with `LHA_ALLOW_UNPRICED_MODELS=true`. Default
`max_tokens` is 4096; the HTTP timeout is 300 s.

### Per-role routing (`lha orchestrate`)

With the `claude` backend, `lha orchestrate` builds each role's model from its tier
([`agents/roles.py`](../python/src/lha/agents/roles.py), [`agents/router.py`](../python/src/lha/agents/router.py)):

| Tier | Model | Roles |
|---|---|---|
| Opus | `claude-opus-4-8` | lead, reviewer (and the planner role definition) |
| Sonnet | `claude-sonnet-4-6` | tester, integrator, auditor, librarian, implementer |
| Haiku | `claude-haiku-4-5-20251001` | researcher |

The durable organization (`lha mission-start --review --max-parallel N`) routes its implementers
and reviewer the same way; its researchers (`run_subagent`) and the Lead of its serial rounds
use `LHA_MODEL_NAME`. The replanner, which splits a blocked item, uses the lead's model on every
run path. The planner call in `lha orchestrate` uses `LHA_MODEL_NAME`, not the router. Explicit
`LHA_CLAUDE_PRICE_*` values apply only to calls whose model is `LHA_MODEL_NAME`; routed roles are
priced from the table. With any other backend every role uses `LHA_MODEL_NAME`.

The `claude` extra installs `claude-agent-sdk` for
[`agents/claude_sdk_lead.py`](../python/src/lha/agents/claude_sdk_lead.py), an optional Lead
engine. No CLI command uses it; to run the lead through Claude Code, use the `claude_code` lead
engine below.

## `claude_code`: Claude Code (`claude -p`)

LHA can run through the Claude Code CLI instead of an API. With a Claude Pro or Max login it
needs no API key. It comes in two strengths, and you can use both at once.

**The model backend** (`LHA_MODEL_BACKEND=claude_code`,
[`model/claude_code.py`](../python/src/lha/model/claude_code.py)). Each model turn is one
`claude -p --output-format json` call with every built-in tool switched off (`--tools ""`). The
conversation goes in on stdin, the system prompt with `--system-prompt`, and the lead replies with
the same JSON actions it uses with any other backend. The planner, replanner and every other role
work unchanged. `LHA_MODEL_NAME` is passed as `--model` (`sonnet`, `opus`, or a full model id);
left at its default, Claude Code picks the model.

**The lead engine** (`LHA_LEAD_ENGINE=claude_code`,
[`agent/claude_code_engine.py`](../python/src/lha/agent/claude_code_engine.py)). A whole lead cycle
is one `claude -p` session, so Claude Code's own agentic loop does the work: it reads, edits, runs
tests and iterates for as many turns as it needs. What comes before and after the session is
unchanged. LHA picks the item and recites the mission anchor; afterwards it runs the checks and
witnesses, commits verified work or rolls a failed attempt back, and replans blocked items. Each
cycle starts a fresh session: the anchor in git is the memory, not the chat. Setting
`LHA_LEAD_ENGINE=claude_code` alone also switches `LHA_MODEL_BACKEND` to `claude_code`, unless
you set a backend explicitly.

```bash
claude            # once, to log in (or set ANTHROPIC_API_KEY for API billing)
cd python
LHA_LEAD_ENGINE=claude_code LHA_MODEL_NAME=sonnet \
  uv run lha mission --task "Create hello.py with hello() returning 'hello', and a pytest test" \
  --workdir ../.lha/workspaces/demo --sandbox local --unsafe-local \
  --no-default-checks --check "uv run --with pytest pytest -q"
```

The engine's tools come in two modes, set by `LHA_CLAUDE_CODE_TOOLS`:

| Mode | What Claude Code can use | Guardrails |
|---|---|---|
| `lha` (default) | LHA's tools only, served over MCP by an in-process server ([`agent/mcp_bridge.py`](../python/src/lha/agent/mcp_bridge.py)) on 127.0.0.1 with a per-session bearer token; its built-in tools are off | all of LHA's: the configured sandbox (Docker by default), the irreversible-command gate, path rules and the egress allow-list |
| `native` | its own Read, Edit, Write, Glob, Grep and Bash, on the host workdir | none from LHA beyond a deny list for `git commit`/`push`/`reset`/`checkout` and similar, publishing commands, `gh`, `curl`, `wget`, WebFetch and WebSearch. Prefix rules are not a safety boundary (`sh -c 'git push'` is not caught). Requires `LHA_SANDBOX=local` and `LHA_ALLOW_UNSAFE_LOCAL=true`, and is refused under an ownership guard (`lha orchestrate`, or a durable mission with an ownership map) |

In both modes the session also gets a `verify` tool. It runs the mission's checks and the item's
witnesses exactly as the harness will, so Claude Code can iterate to green before it stops. It
never marks anything done: LHA verifies again after the session.

**Cost and budget.** Before a `claude -p` call runs, the governor authorizes it with
`LHA_CLAUDE_CODE_MAX_BUDGET_USD` (default $5) as its worst case. The same amount is passed as
`--max-budget-usd`. Afterwards the ledger records the `total_cost_usd` Claude Code reports.
Claude Code checks the cap between API calls, so one call can overshoot it by a single turn. On a
subscription, that figure is the API-equivalent cost, not a bill, but the budget ceiling still
applies to it: raise `LHA_BUDGET_USD_CEILING` for long missions. A session killed at
`LHA_CLAUDE_CODE_TIMEOUT_S` (default 3600 s) reports no cost and is charged its full cap. A session
that stops at its cap or timeout is not wasted: whatever it left in the workdir is verified like
any other attempt.

**Failures.** An expired login or a bad flag fails the call (and the cycle) without retries; rate
limits, overload and 5xx answers are retried with backoff like any other backend. Run `claude`
once in a terminal to log in again.

## Unpriced models: `LHA_ALLOW_UNPRICED_MODELS`

When a provider cannot price a call, the governor has two refusals
([`governor/governor.py`](../python/src/lha/governor/governor.py)):

- before the call, if its worst-case cost cannot be computed;
- before any later call or cycle, if the ledger already holds unknown-cost entries.

Setting `LHA_ALLOW_UNPRICED_MODELS=true` lifts both. Unknown-cost calls are then recorded with
`cost_known=False` and `usd=0.0` in the in-memory ledger; the printed total counts known spend
only, so spend is no longer verifiable against `LHA_BUDGET_USD_CEILING`.

## Retries and failover

**Retries inside a provider** ([`model/retry.py`](../python/src/lha/model/retry.py)). Both HTTP
backends retry transient failures up to 3 times (4 attempts in total):

| Retried | Not retried |
|---|---|
| HTTP 408, 409, 429, any 5xx (including 529) | 400, 401, 403 and other 4xx |
| timeouts, connection and transport errors | programming errors, parse errors |

The delay is the server's `Retry-After` (seconds or HTTP date) if present, else 1 s, 2 s, 4 s;
every delay is capped at 60 s. After the last retry the error propagates. Inside a Temporal
activity the attempt then fails and Temporal's own retry policy takes over (see
[14-running-on-temporal.md](14-running-on-temporal.md)).

**Failover** ([`model/failover.py`](../python/src/lha/model/failover.py)). `FailoverModel` wraps an
ordered list of providers: on a transient error it tries the next provider; after a full round of
transient failures it backs off (same rules) and tries again, up to `max_rounds`. Non-transient
errors (400, 401, 403, programming errors) raise immediately.

`LHA_FALLBACK_MODELS` selects it. It is a comma-separated, ordered list of
`backend:model[@in/out]` entries, where `in`/`out` are USD per 1M tokens. When it is non-empty,
`build_provider` returns `FailoverModel([primary, *fallbacks])`, so every run path gets the chain
(`mission`, `run-local`, `orchestrate` including per-role routing and the planner,
`mission-start`, and the Temporal cycle and sub-agent activities).

```bash
export LHA_MODEL_BACKEND=claude
export LHA_MODEL_NAME=claude-sonnet-4-6
export LHA_ANTHROPIC_API_KEY=...
export LHA_OPENAI_BASE_URL=https://api.groq.com/openai/v1
export LHA_OPENAI_API_KEY=...
export LHA_FALLBACK_MODELS="openai_compat:llama-3.3-70b-versatile@0.59/0.79,ollama:qwen3:8b"
```

| Setting | Default | Notes |
|---|---|---|
| `LHA_FALLBACK_MODELS` | empty | `backend` is `stub`, `ollama`, `openai_compat` or `claude`; the model part may contain `:` (`ollama:qwen3:8b`). A malformed entry raises `ValueError` when the provider is built |
| `LHA_FALLBACK_MAX_ROUNDS` | `2` | `FailoverModel.max_rounds` |

- Fallback entries use the backend's shared settings: `openai_compat` entries use
  `LHA_OPENAI_BASE_URL` / `LHA_OPENAI_API_KEY` (so one OpenAI-compatible endpoint per
  deployment), `claude` entries use `LHA_ANTHROPIC_API_KEY`, `ollama` entries use
  `LHA_OLLAMA_BASE_URL`. A missing key or base URL fails when the provider is built.
- Prices: an entry's `@in/out` price wins. Otherwise `claude` entries use the built-in table
  (an id not in the table fails with `UnknownPriceError`), `ollama` and `stub` are `$0`, and an
  unpriced `openai_compat` entry is **unknown**. `LHA_OPENAI_PRICE_*` and `LHA_CLAUDE_PRICE_*`
  apply to the primary model only.
- Per-role routing (`lha orchestrate` with `claude`) changes the primary model only; the
  fallbacks stay as configured.
- In a chain, each member retries a transient error once (`CHAIN_MEMBER_RETRIES`) instead of 3
  times, so an outage fails over in seconds.
- Cost is keyed on the provider that served the turn: each backend stamps `Usage.provider` (and
  `Usage.model` from the response), and `FailoverModel.estimate_cost_usd` prices the turn with that
  provider. The pre-call worst-case estimate uses the most expensive member; if any member is
  unpriced, the governor refuses the call unless `LHA_ALLOW_UNPRICED_MODELS=true`.

## Health probe

A parked durable mission resumes only when `check_mission_health` reports the model healthy
([`model/health.py`](../python/src/lha/model/health.py)). `probe_model` builds the configured
provider (including the fallback chain) and sends the cheapest request each backend has, under
`LHA_MODEL_PROBE_TIMEOUT_S` (default 10 s). No tokens are spent.

| Provider | Probe | Healthy when |
|---|---|---|
| `stub` | none | always |
| `ollama` | `GET {LHA_OLLAMA_BASE_URL}/api/tags` | 2xx and `LHA_MODEL_NAME` (or `<name>:latest`) is pulled |
| `openai_compat` | `GET {LHA_OPENAI_BASE_URL}/models` with the bearer key | 2xx |
| `claude` | `GET https://api.anthropic.com/v1/models/{model}` with the API key | 2xx |
| `claude_code` | `claude --version` | exit 0 (it cannot prove the login is valid) |
| failover chain | every member, concurrently | any member is healthy |

A configuration error, transport error, timeout or non-2xx response (including 401/403 and 404
for an unknown Claude model) is reported as DOWN with the reason. See
[15-operations-runbook.md](15-operations-runbook.md#degradation-modes).

## Go implementation

`go/internal/model` ports the stub, OpenAI-compatible and Claude backends, pricing, retry and
failover, and runs the shared `spec/model/pricing.json` cases. The Go settings do not yet read
`LHA_FALLBACK_MODELS`, `LHA_FALLBACK_MAX_ROUNDS` or `LHA_MODEL_PROBE_TIMEOUT_S`, there is no Go
health probe, and there is no Go CLI to run a mission with. The `claude_code` backend and lead engine are
Python-only; the Go settings reject `LHA_MODEL_BACKEND=claude_code`. See
[04-choosing-an-implementation.md](04-choosing-an-implementation.md).
