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

### Fallback chain pricing

`LHA_FALLBACK_MODELS` is a comma-separated list of `backend:model[@in/out]` entries (USD per
million input / output tokens; the model part may itself contain `:`, as in
`ollama:qwen3:8b`). With it set, `build_provider` returns a `FailoverModel`
([model/failover.py](../python/src/lha/model/failover.py)) over the primary and each fallback in
order. Each fallback is priced like a primary of its backend, except that the settings prices
(`LHA_OPENAI_PRICE_*`, `LHA_CLAUDE_PRICE_*`) apply to the primary only:

- `@in/out` on the entry is the explicit price and wins over the Claude table;
- a `claude` fallback without `@in/out` uses the table, and a model not in it is refused when the
  provider is built (`UnknownPriceError`);
- an `openai_compat` fallback without `@in/out` is unpriced (its cost is unknown), and an
  `ollama` fallback is $0 whatever the entry says.

A malformed entry or price raises `ValueError` when the provider is built. Every
`openai_compat` fallback uses the same `LHA_OPENAI_BASE_URL` and `LHA_OPENAI_API_KEY` as the
primary: the chain can switch models on one OpenAI-compatible endpoint, not between two such
endpoints.

`FailoverModel` stamps `Usage.provider` with the member that served the turn, and
`estimate_cost_usd` prices the turn with that member. The pre-call worst case (no provider yet)
is the maximum over all members, so one unpriced member makes every worst case unpriceable and
the governor denies the call unless `LHA_ALLOW_UNPRICED_MODELS=true`.

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
reviewer and reflection models are all wrapped. A `claude_code` lead engine session is metered
through the lead's model (`run_external`): authorized with `LHA_CLAUDE_CODE_MAX_BUDGET_USD` as its
worst case, then charged the cost it reports, or that worst case when it reports none. The
replanner uses the lead's metered model, so the call that splits a blocked item is authorized
against the budget and recorded under role `lead` in the same cycle. For each `complete()` call:

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
| `LHA_MAX_TURNS_PER_CYCLE` | 20 | agent loop turns per cycle |
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

The other durable activities that call a model are budgeted the same way: the organization's
`run_implementer`, `review_cycle` and `integrate_branch` (its replanner split), and
`run_subagent` when its workdir is a mission checkout. Each seeds its ledger from the journal,
uses `budget_usd` (else the worker's ceiling) as the ceiling, and appends its own spend row
(keys `idempotency_key(mission, cycle, "impl" | "split", attempt)`,
`idempotency_key(mission, "<cycle>-review", attempt)`, and for a sub-agent
`idempotency_key(mission, "sub:<workflow>:<activity>@<attempt>")`). The implementers of one
parallel wave run concurrently and each sees only the spend recorded before it started, so each
meters against its share of the budget: the spend recorded before the wave plus an equal fraction
of what is left (`wave_share`), and the wave cannot overshoot the ceiling. A `run_subagent` outside a git checkout builds
a fresh meter from the worker's ceiling. Every call of these activities is also written to the
persistent cost ledger under the mission id (sub-agent cycle id `subagent:<role>`, or
`<cycle>-research:<role>` for the organization's researchers).

## Loop detection and per-item failure budget

Two mechanisms stop repeated failure on the same item:

- **Per-item failure budget.** `Checklist.record_failure()`
  ([contracts/state.py](../python/src/lha/contracts/state.py)) increments
  `consecutive_failures`. At `max_consecutive_failures` it sets the item to `blocked`. `AgentLoop`
  defaults this to 3, and no entry point overrides it. With [System One triage](25-system-one.md)
  on, a confident answer can block or split the item after `LHA_SYSTEM_ONE_TRIAGE_MIN_FAILURES`
  (default 2) instead, which saves the remaining attempts. Blocked items are skipped, so independent
  items continue. When only blocked items remain, the mission is deadlocked. `Checklist.unblock()`
  (or the durable `unblock_items` activity) resets the item to `todo` with a fresh count. This
  applies on every path.
- **`LoopDetector`.** It counts consecutive failures per signature and resets on success. The local
  runner observes `<item>:failed`. The orchestrator also observes `<item>:review_blocked`. It stops
  the run with `loop on item <id>` once a count reaches `LHA_STALL_LIMIT`. With the defaults (block
  at 3, stall limit 5), a failing item is blocked before the detector's limit is reached. The
  detector matters when `LHA_STALL_LIMIT` is 3 or less, and for review-blocked loops, which do not
  block the item. The durable workflow does not use `LoopDetector`; there, the per-item failure
  budget blocks the item and a deadlock goes to the deadlock gate.

In the durable workflow, `ops.lifecycle.should_declare_impossible()` compares the count of
consecutive non-passing verified cycles on one item (`MissionState.fail_streak`) with
`MissionInput.impossible_after_failures` (default 3). When it is reached, the deadlock gate
recommends `impossible` ([Running on Temporal](14-running-on-temporal.md#5-gates-sleep-and-abort)).

System One calls ([25](25-system-one.md)) are metered like model calls, under the ledger role
`system_one`. Each is authorized against its worst case before it is sent and recorded at the
input tokens it reported (output is free). The price is `LHA_SYSTEM_ONE_PRICE_IN_PER_MTOK`, or
$0.042 per million for TypeSafe's host and $0 on loopback. Any other endpoint must set a price,
because an unpriced call would make the whole ledger unverifiable. A call the budget refuses is
skipped; it never stops the run.

## Cost ledger

| Store | Where | Unknown cost |
|---|---|---|
| `CostLedger` | [governor/cost.py](../python/src/lha/governor/cost.py), in memory, one per mission run; what the governor reads. Keeps only the newest `MAX_LEDGER_ENTRIES` (5,000) entries; its totals cover every entry, and the full record is in `cost_ledger` | `cost_known=False`, `usd=0.0` |
| Spend journal | `.git/lha/spend.ndjson`, durable path only; seeds the next attempt's governor | `unknown` count per attempt |
| Persistent `cost_ledger` | the mission store: SQLite (default) or Postgres (`LHA_POSTGRES_DSN`), see below | `usd = NULL`, `cost_known = false` |

### The persistent ledger

EVERY metered model call is also written to the `cost_ledger` table. `CostMeter.on_record`
([metering.py](../python/src/lha/governor/metering.py)) is an optional async hook called with each
recorded `CostEntry`; the run paths install a `LedgerSink`
([persistence/tracking.py](../python/src/lha/persistence/tracking.py)) there. A failing hook is
logged and never fails the call (its spend is already in the in-memory ledger). Where each run
path writes:

| Run path | Rows | Idempotency `call_key` |
|---|---|---|
| `lha run-local`, `lha mission`, `lha orchestrate` | every call of every role (planner, lead, researcher, reviewer, reflection, librarian); the Planner's call, made before the mission id exists, is backfilled when the run starts | `#<n>` (sequence within the run) |
| `run_agent_cycle` activity | every call of the attempt; the seeded `(prior)` entries are not written again | `<cycle>@<attempt>#<n>` |
| `run_subagent` activity | every sub-agent call, under the parent mission id | `sub:<workflow>:<activity>@<attempt>#<n>` |
| `run_implementer`, `review_cycle`, the split in `integrate_branch` | every call of the attempt | `impl:` / `review:` / `split:` + `<workflow>:<activity>@<attempt>#<n>` |
| `lha mission-start` | the Planner's call | `planner#<n>` |

The row key is `idempotency_key("cost", mission_id, cycle_id, call_key)` (the same derivation as
`CostLedgerRepo`), inserted with `INSERT OR IGNORE` (SQLite) / `ON CONFLICT (idempotency_key) DO
NOTHING` (Postgres): a replayed write of the same logical call is a no-op, while a retried
activity attempt (a new attempt number) records its re-spent calls as new rows. A call whose cost
is unknown is stored with `usd = NULL` and `cost_known = false`, never as $0, so `SUM(usd)` is the
known spend.

Read it back with `lha costs <mission_id>` (the most recent calls plus totals: known USD,
unknown-cost calls, tokens) and `lha missions [--limit N]` (each mission's status and totals).
The status column is the last status a run path wrote to the `missions` row. A durable mission's
workflow-only states (`DEGRADED_PARK`, `SLEEPING`, an open gate's `WAITING_ON_HUMAN`) are written
there best effort by the workflow's `record_mission_status` activity, on histories that carry
`lha-mission-row-v1` ([durable execution](08-durable-execution.md)). `lha mission-status` queries
the live status.

The Postgres schema is in [`db/migrations/`](../db/migrations/):

- `0001` creates `cost_ledger`;
- `0002` adds `idempotency_key` (unique), `role` and `cost_known`;
- `0003` makes `usd` nullable and rewrites unknown rows to `NULL`.

The SQLite store creates the same columns (`usd REAL` nullable, `idempotency_key` unique) when it
opens. The Postgres store (`PostgresStore`,
[persistence/postgres.py](../python/src/lha/persistence/postgres.py)) needs the `postgres` extra
and `lha db migrate`; if it cannot be used, the run falls back to SQLite with a warning unless
`LHA_POSTGRES_FALLBACK_TO_SQLITE=false` (then the run fails). `CostLedgerRepo`
([persistence/repositories.py](../python/src/lha/persistence/repositories.py)) is an older
Postgres-only helper with the same insert; the run paths use `PostgresStore`.

Reported cost on the terminal still comes from the in-memory ledger (`lha run-local` / `mission` /
`orchestrate` print `cost $…`) and, for durable runs, from `spent_usd` in each `CycleResult`.

### In the Go implementation

The Go `lha run-local`, `lha mission` and `lha orchestrate` persist the same way:
`governor.CostMeter` has a `CostHook` (`SetHook`, python's `on_record`) and the run services
install `persistence.LedgerSink` there, backfilling the Planner's call (the `planner` role, cycle
`c0`), with the same key prefixes (empty, or `run<N>` for a resumed `orchestrate`) and the same
row key, so a Go run's ledger rows are the ones a Python run would write. Every org role's calls
land there, and so does a `claude_code` session metered by `RunExternal` (its reported cost, or
the worst case when it reported none); a failing hook is logged (`cost_hook_failed`) and never
fails the call.
The Go `lha missions` and `lha costs` read either implementation's store with the same output.
The Postgres backend is built in (no extra); `lha db migrate` works from the Go binary too.

Related: [Models](13-models.md), [Configuration](18-configuration.md).
