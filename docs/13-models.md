# Models

Every model call in LHA goes through one interface, `ModelProvider`
([`contracts/model.py`](../python/src/lha/contracts/model.py)). `build_provider` in
[`model/__init__.py`](../python/src/lha/model/__init__.py) is the only place a concrete backend is
chosen, from `LHA_MODEL_BACKEND`. Four backends exist:

| `LHA_MODEL_BACKEND` | Class | Transport | Cost source |
|---|---|---|---|
| `stub` (default) | `StubModel` | none (in-process) | always `$0` |
| `ollama` | `OpenAICompatModel` (label `ollama`) | `POST {LHA_OLLAMA_BASE_URL}/v1/chat/completions` | fixed `$0` |
| `openai_compat` | `OpenAICompatModel` | `POST {LHA_OPENAI_BASE_URL}/chat/completions` | `LHA_OPENAI_PRICE_*`; unknown if unset |
| `claude` | `ClaudeModel` | `POST https://api.anthropic.com/v1/messages` | built-in table, or `LHA_CLAUDE_PRICE_*` |

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

The planner call in `lha orchestrate` uses `LHA_MODEL_NAME`, not the router. Explicit
`LHA_CLAUDE_PRICE_*` values apply only to calls whose model is `LHA_MODEL_NAME`; routed roles are
priced from the table. With any other backend every role uses `LHA_MODEL_NAME`.

The `claude` extra installs `claude-agent-sdk` for
[`agents/claude_sdk_lead.py`](../python/src/lha/agents/claude_sdk_lead.py), an optional Lead
engine. No CLI command uses it.

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
transient failures it backs off (same rules) and tries again, up to `max_rounds` (default 2).
Non-transient errors raise immediately. Each turn is priced by the provider that served it.
`FailoverModel` is a library class: no `LHA_*` setting selects it, and `build_provider` never
constructs it. Using it requires constructing it in code.

## Go implementation

`go/internal/model` ports the stub, OpenAI-compatible and Claude backends, pricing, retry and
failover, and runs the shared `spec/model/pricing.json` cases. See
[04-choosing-an-implementation.md](04-choosing-an-implementation.md) for what the Go CLI can run
today.
