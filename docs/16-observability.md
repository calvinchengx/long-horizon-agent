# Observability

What LHA records about a mission, where it goes, and what is only a hook. The code is in
[`python/src/lha/obs/`](../python/src/lha/obs/).

| Signal | Local runs (`run-local`, `mission`, `orchestrate`) | Durable runs (`lha worker`) |
|---|---|---|
| Structured events (`TraceRecorder`) | yes, printed via structlog | no |
| Git history + `.lha/` anchor | yes | yes |
| Temporal event history | n/a | yes |
| OpenTelemetry spans | `orchestrate` only, if a tracer provider is installed | no |
| Langfuse | not sent | not sent |
| Cost | summary line on exit | `.git/lha/spend.ndjson`, `spent_usd` per cycle |

## Structured events

[`obs/events.py`](../python/src/lha/obs/events.py) defines `TraceEvent(kind, mission_id,
cycle_id, data)` and `TraceRecorder`. `record()` redacts `data`, appends the event to an in-memory
list, and logs it through structlog at `info` level with the event kind as the message.
`to_jsonl()` serializes the collected events one JSON object per line.

`configure_logging(json_logs=False)` sets structlog to add the log level and an ISO timestamp and
render with `ConsoleRenderer(colors=False)`; `json_logs=True` selects `JSONRenderer`. The local
runners call it with the default, so CLI runs print console-format lines. No CLI option switches
to JSON.

Event kinds emitted today:

| Kind | Emitted by | Data |
|---|---|---|
| `cycle_started` | `AgentLoop` | `item_id` |
| `llm_turn` | `AgentLoop` | `model`, `output_tokens`, `stop_reason` |
| `tool_call` | `AgentLoop` | `tool`, `ok` |
| `invalid_reply` | `AgentLoop` | `reason` |
| `checkpoint` | `AgentLoop` | `head_sha`, `verified`, `verdict` |
| `governor_block` | runner, orchestrator | `reason` |
| `deadlocked` | runner, orchestrator | `reason` |
| `loop_detected` | runner | `item_id` |
| `research`, `research_failed` | orchestrator | research fan-out results |
| `reflection` | orchestrator | `item` |
| `review`, `review_reopened` | orchestrator | reviewer verdict |

Model spend is recorded by the metering wrapper, not by `llm_turn` events.

The local runners return the collected trace as `MissionSummary.trace_jsonl`, but the CLI does not
print or save it; it is available when calling `run_mission_local`, `plan_and_run_local` or
`Orchestrator.run_mission` from Python.

The durable activity constructs `AgentLoop` without a recorder, so a mission on Temporal emits no
`TraceEvent`s. Its record is the Temporal history plus the git anchor.

## Redaction

[`obs/redact.py`](../python/src/lha/obs/redact.py) masks secrets with `***` before they reach a log
line, the collected trace or a span attribute:

- **Keys**: a value is masked when its key matches `api_key`, `apikey`, `secret`, `password`,
  `passwd`, `authorization`, `credential`, `private_key`, `cookie`, `dsn`, or ends in `token` /
  `auth` as a whole word (so `input_tokens` stays visible). camelCase keys are split first
  (`accessToken` counts).
- **Values** anywhere in strings: `sk-...` keys, GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`,
  `ghr_`, `github_pat_`), Slack tokens (`xox?-`), AWS access key ids (`AKIA...`), Google API keys
  (`AIza...`), `Authorization:` header values, `Bearer <token>`, and the password in
  `scheme://user:password@host`.
- `SecretStr` values are always masked. Mappings and lists are redacted recursively.

The rules are pinned by [`spec/obs/redact.json`](../spec/obs/redact.json). The Python suite runs it;
the Go port has no `obs` package yet.

`lha config` masks every `SecretStr` setting (`***` when set, `None` when unset). See
[18-configuration.md](18-configuration.md).

## OpenTelemetry

`agent_span(name, **attributes)` in [`obs/otel.py`](../python/src/lha/obs/otel.py) opens a span
whose redacted attributes are prefixed `gen_ai.`. If `opentelemetry` is not installed it does
nothing. Install it with the `observability` extra:

```bash
cd python && uv sync --extra observability   # langfuse, opentelemetry-sdk, opentelemetry-exporter-otlp
```

The only caller is the orchestrator (`lha orchestrate`), which opens one `cycle` span per item
attempt with `mission_id` and `item`. LHA never installs a `TracerProvider` or exporter, so with
the CLI the spans are no-ops. To export them, embed the orchestrator in a process that configures
the OpenTelemetry SDK (for example an OTLP exporter pointed at a collector or at Langfuse's OTLP
endpoint).

## Langfuse

[`obs/langfuse_exporter.py`](../python/src/lha/obs/langfuse_exporter.py) has one function,
`build_langfuse()`, which returns a `Langfuse` client when `LHA_LANGFUSE_HOST`,
`LHA_LANGFUSE_PUBLIC_KEY` and `LHA_LANGFUSE_SECRET_KEY` are all set and the `langfuse` package is
installed, else `None`. Nothing calls it, and no event is mirrored to Langfuse. The compose stack
runs a Langfuse server on port 3000; it stays empty unless you send data to it yourself.

## Temporal UI

For durable missions the Temporal UI (<http://localhost:8080> with the compose stack) is the
primary view. Per workflow `mission:<mission_id>` it shows:

- each `run_agent_cycle` activity: its `CycleInput`, its `CycleResult` (`item_id`, `advanced`,
  `verdict`, `items_done`/`items_total`, `head_sha`, `note`, `spent_usd`), attempts and failures;
- durable timers and `check_mission_health` results while parked;
- signals received (`human_decision_v1`, `steer_v1`) and Continue-As-New boundaries;
- query results, including `status_v1`, `cycles_done`, `last_item`, `park_reason`.

Payloads larger than 32 KiB are stored in the object store and appear as ClaimCheck pointers; the
UI cannot decode them without a codec server, which LHA does not provide.

## Cost summary

Every model call is recorded in a `CostLedger` ([`governor/cost.py`](../python/src/lha/governor/cost.py)):
model, input/output tokens, cache read/write tokens, role, USD, and `cost_known`.

- **Local runs** print on exit:

  ```
  mission mission_3f9a1c0b2d4e: complete
  items 3/3  cycles 3  cost $0.0000
  head 4e1d2c9...
  ```

  `cost` is known spend (`CostLedger.total_usd`). Calls with unknown cost add `$0` to that figure
  and are not listed in the summary.

- **Durable runs** append one line per cycle attempt to `<workdir>/.git/lha/spend.ndjson`:
  `{"key", "cycle_id", "usd", "unknown", "calls"}`. `key` is a hash of mission, cycle and attempt;
  readers keep the last row per key. Each `CycleResult` also carries that attempt's `spent_usd`.

- **Postgres**: `CostLedgerRepo` in
  [`persistence/repositories.py`](../python/src/lha/persistence/repositories.py) writes idempotent
  rows to `cost_ledger` (unknown cost stored as `NULL`) and can sum them, but no CLI command or
  workflow calls it.

Budget enforcement is described in [10-cost-and-budget.md](10-cost-and-budget.md).
