# Cost and budget

Every model call is checked against the budget before it runs and recorded after it returns. Cost is
computed from the token usage the provider reports. If a call's cost cannot be computed, it is
recorded as unknown, never as $0. Code: [`python/src/lha/governor/`](../python/src/lha/governor/),
[`python/src/lha/model/pricing.py`](../python/src/lha/model/pricing.py).

## Token usage

Providers return a `Usage` ([contracts/model.py](../python/src/lha/contracts/model.py)) taken
from the API response:

- `input_tokens` and `output_tokens`;
- for Claude, `cache_read_input_tokens`, `cache_creation_input_tokens` and
  `cache_creation_1h_input_tokens`;
- `model`, the model that actually served the turn. Cost is keyed on this model, not the
  requested one;
- `provider`, which failover sets so the responding provider's prices apply.

`StubModel` returns synthetic token counts, and its cost is $0. It is for tests only.

## Prices

`CLAUDE_PRICES` in [pricing.py](../python/src/lha/model/pricing.py), in USD per million tokens:

| Model | Input | Output |
|---|---|---|
| `claude-opus-4-8` | 5.00 | 25.00 |
| `claude-sonnet-4-6` | 3.00 | 15.00 |
| `claude-haiku-4-5` | 1.00 | 5.00 |

These are Anthropic first-party rates as last checked by the authors (2026-06). Verify them
before relying on them. Partner platforms (Bedrock, Vertex) price differently. A dated snapshot
suffix (`-20251001`) is stripped before lookup. Prompt cache writes are billed at 1.25x the input
price (5-minute TTL) or 2x (1-hour TTL), and cache reads at 0.1x. Each is counted separately from
`input_tokens`. The table and per-call costs are pinned in
[`spec/model/pricing.json`](../spec/model/pricing.json).

How each backend is priced (`build_provider`, [model/__init__.py](../python/src/lha/model/__init__.py)):

| Backend | Price source |
|---|---|
| `stub` | always $0 |
| `ollama` | explicit $0 / $0 (local) |
| `openai_compat` | `LHA_OPENAI_PRICE_IN_PER_MTOK` and `LHA_OPENAI_PRICE_OUT_PER_MTOK`; set both or neither. Unset means unknown |
| `claude` | the table above; `LHA_CLAUDE_PRICE_IN_PER_MTOK` / `_OUT_` override it for `LHA_MODEL_NAME` only. A configured model with no price is refused at construction (`UnknownPriceError`) |

## Unknown prices are never $0

If a provider cannot price a turn, `estimate_cost_usd` raises `UnknownPriceError`. The ledger
then records the entry with `cost_known=False` and `usd=0.0`. In this case `0.0` means "not
counted", not "free". `CostLedger.total_usd` is known spend only. `unknown_cost_entries` counts
the rest.

The governor refuses to spend an unknown amount:

- a call whose worst case cannot be priced is denied;
- once the ledger holds any unknown-cost entry, every later call is denied.

`LHA_ALLOW_UNPRICED_MODELS=true` lifts both refusals. Spend is then unverifiable.

## Metering and worst-case reservation

`CostMeter` ([metering.py](../python/src/lha/governor/metering.py)) holds one ledger, one governor
and a running total of in-flight reservations. Every provider a mission uses is wrapped with
`meter.wrap(provider, role=…)`, which returns a `MeteredModel`. The planner, lead, researchers,
reviewer and reflection models are all wrapped. For each `complete()` call:

1. **Worst case.** The input-token estimate is deliberately high: characters / 2, plus the size
   of tool-call arguments and tool schemas, plus 8 tokens per message. Output is assumed to be
   the full `max_tokens` (else the provider's `default_max_tokens`, else 8192). This usage is
   priced with the provider's prices.
2. **Authorize.** `BudgetGovernor.authorize_call()` checks spent + reserved + worst case against
   the ceiling. A refusal raises `BudgetExceeded`.
3. **Reserve.** The worst case is reserved while the call is in flight, so concurrent calls (the
   research fan-out) cannot overshoot together. The reservation is released afterwards.
4. **Record.** The actual cost is computed from the returned usage and appended with the role and
   the current `meter.cycle_id`.

## Budget governor and cycle limits

`BudgetGovernor(ceiling_usd, max_cycles, allow_unknown_cost)`
([governor.py](../python/src/lha/governor/governor.py)) has two checks:

- `authorize_call` is the hard per-call stop described above. It applies on every path.
- `authorize_next` is checked before each cycle. It denies when `cycles_done >= max_cycles`, when
  unknown spend exists (unless allowed), or when spent + the most expensive cycle so far would
  exceed the ceiling. The local runner (`lha run-local`, `lha mission`) and `lha orchestrate`
  call it. The durable cycle activity does not. There the workflow enforces
  `MissionInput.max_cycles` and the per-call stop enforces the budget.

| Setting | Default | Used by |
|---|---|---|
| `LHA_BUDGET_USD_CEILING` | 10.0 | all paths; durable missions use `MissionInput.budget_usd` if set |
| `LHA_MAX_CYCLES` | 1000 | local runners, sub-agent meters. `MissionInput.max_cycles` has its own default of 1000 |
| `LHA_MAX_TURNS_PER_CYCLE` | 8 | agent loop turns per cycle |
| `LHA_STALL_LIMIT` | 5 | `LoopDetector` threshold (local runners) |
| `LHA_ALLOW_UNPRICED_MODELS` | false | governor |

Locally, a refusal ends the run with `stopped_reason` `governor: <reason>`. `lha` exits with code 3
if `BudgetExceeded` escapes, for example from the planner. In the durable path the cycle activity
raises a non-retryable `BudgetExceeded`, and the mission ends with outcome `budget_exhausted`
(status `ABORTED`).

### Budget across durable retries

A crashed activity attempt loses its in-memory ledger. To keep failed attempts counted, each
attempt appends one row `{key, cycle_id, usd, unknown, calls}` to `.git/lha/spend.ndjson` (fsynced;
`key = idempotency_key(mission, cycle, attempt)`). The next attempt seeds its ledger with the sum.
Unknown-cost calls are carried forward as unknown entries.

Sub-agent activities (`run_subagent`) build a fresh meter from the worker's ceiling. Their spend
is not added to the mission's journal.

## Loop detection and per-item failure budget

Two mechanisms stop repeated failure on the same item:

- **Per-item failure budget.** `Checklist.record_failure()`
  ([contracts/state.py](../python/src/lha/contracts/state.py)) increments
  `consecutive_failures`. At `max_consecutive_failures` it sets the item to `blocked`. `AgentLoop`
  defaults this to 3, and no entry point overrides it. Blocked items are skipped, so independent
  items continue. When only blocked items remain, the mission is deadlocked. `Checklist.unblock()`
  (or the durable `unblock_items` activity) resets the item to `todo` with a fresh count. This
  applies on every path.
- **`LoopDetector`.** It counts consecutive failures per signature and resets on success. The local
  runner observes `<item>:failed`. The orchestrator also observes `<item>:review_blocked`. It stops
  the run with `loop on item <id>` once a count reaches `LHA_STALL_LIMIT`. With the defaults (block
  at 3, stall limit 5), a failing item is blocked before the detector's limit is reached. The
  detector matters when `LHA_STALL_LIMIT` is 3 or less, and for review-blocked loops, which do not
  block the item. The durable workflow does not use `LoopDetector`.

`ops.lifecycle.should_declare_impossible()` exists but is not called by any run path.

## Cost ledger

| Store | Where | Unknown cost |
|---|---|---|
| `CostLedger` | [governor/cost.py](../python/src/lha/governor/cost.py), in memory, one per mission run | `cost_known=False`, `usd=0.0` |
| Spend journal | `.git/lha/spend.ndjson`, durable path only | `unknown` count per attempt |
| `CostLedgerRepo` | [persistence/repositories.py](../python/src/lha/persistence/repositories.py), Postgres `cost_ledger` | `usd = NULL` |

The Postgres schema is in [`db/migrations/`](../db/migrations/):

- `0001` creates `cost_ledger`;
- `0002` adds `idempotency_key` (unique), `role` and `cost_known`;
- `0003` makes `usd` nullable and rewrites unknown rows to `NULL`.

`CostLedgerRepo.record()` inserts with `ON CONFLICT (idempotency_key) DO NOTHING`, using the key
`idempotency_key("cost", mission_id, cycle_id, call_key)`, so a retried write does not double
count. `total_usd()` is `SUM(usd)`, which gives known spend. `unknown_cost_calls()` counts
`NOT cost_known`. It needs the `postgres` extra and `lha db migrate`.

`CostLedgerRepo` is not called by any run path today: no runner or activity writes spend to
Postgres. Reported cost comes from the in-memory ledger (`lha run-local` / `mission` /
`orchestrate` print `cost $…`) and, for durable runs, from `spent_usd` in each `CycleResult`.

Related: [Models](13-models.md), [Configuration](18-configuration.md).
