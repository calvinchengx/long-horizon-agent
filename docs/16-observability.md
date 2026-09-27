# Observability

What LHA records about a mission and where it goes. The code is in
[`python/src/lha/obs/`](../python/src/lha/obs/).

| Signal | Local runs (`run-local`, `mission`, `orchestrate`) | Durable runs (`lha worker`) |
|---|---|---|
| Structured events (`TraceRecorder`) | yes, printed via structlog | no (memory events are logged via structlog in the worker) |
| Git history + `.lha/` anchor | yes | yes, plus `gate_*` events for human gates |
| Temporal event history | n/a | yes |
| Mission row (`missions`) | yes | yes, written by `mission-start`, each cycle activity and the workflow (`record_mission_status`) |
| Cost ledger (`cost_ledger`) | every metered call | every metered call, plus `.git/lha/spend.ndjson` |
| Gate webhook (`LHA_GATE_WEBHOOK_URL`) | gate events from `--approve-interactive` | every gate event |
| OpenTelemetry spans (OTLP) | mission, cycle, implementer, model call and tool call spans, when an endpoint or Langfuse is configured and the `observability` extra is installed | cycle activity, cycle, implementer, model call and tool call spans, under the same conditions |
| Langfuse | the same spans, through Langfuse's OTLP endpoint | the same spans, through Langfuse's OTLP endpoint |
| Cost summary | summary line on exit | `spent_usd` per cycle; `lha costs` for both |

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
| `claude_code_session` | `AgentLoop` (the `claude_code` lead engine) | `turns`, `tool_calls`, `session_id`, `stopped` |
| `check_quarantined`, `quarantined_check_failed` | `AgentLoop` (from the verifier) | `check`, `revision`, `passes`, `fails` (also committed to `.lha/events.ndjson`; see [07-verification.md](07-verification.md#flaky-check-quarantine)) |
| `sandbox_egress` | `AgentLoop` (from the Docker sandbox's egress proxy log) | `decision` (`allow`, `deny`, `fail`), `method`, `host`, `port`, `detail` (the address connected to, or the reason), `count` (requests with that decision, method, host and port in the cycle). Also committed to `.lha/events.ndjson`; see [09-safety-model.md](09-safety-model.md#sandbox-network) |
| `governor_block` | runner, orchestrator | `reason` |
| `deadlocked` | runner, orchestrator | `reason` |
| `loop_detected` | runner | `item_id` |
| `decision_chain_invalid` | runner, orchestrator | `reason` (the committed decision log failed verification; the run stops) |
| `research`, `research_failed` | orchestrator | research fan-out results |
| `reflection` | orchestrator | `item` |
| `review`, `review_reopened` | orchestrator | reviewer verdict |
| `parallel_wave` | orchestrator | the items of a parallel wave |
| `integration` | orchestrator | `item`, `merged`, `branch`, `reason` |
| `ownership_violation`, `ownership_released` | orchestrator | writer and paths |
| `lease` | orchestrator | `writer`, `path`, `granted`, `why` (one per lease request of an implementer) |
| `resumed` | orchestrator (`--resume`) | `run`, `cycle_offset`, `board` |
| `memory_degraded`, `memory_error`, `memory_consolidated`, `skill_stored` | memory service | reason or counts ([12-memory.md](12-memory.md)) |

Model spend is recorded by the metering wrapper, not by `llm_turn` events.

The local runners return the collected trace as `MissionSummary.trace_jsonl`, but the CLI does not
print or save it; it is available when calling `run_mission_local`, `plan_and_run_local` or
`Orchestrator.run_mission` from Python.

The durable activity constructs `AgentLoop` without a recorder, so a mission on Temporal emits no
`TraceEvent`s. Its record is the Temporal history, the git anchor and the mission store.

## Human gate events

Gate activity is recorded in the anchor's `.lha/events.ndjson` and, one row per gate, in the
mission store's `hitl_gates` table (`lha gates [MISSION_ID]`: kind, question, options, reminders,
decision, who and when):

- **Durable runs.** The `notify_gate` activity commits one checkpoint per gate event (kind
  `gate_opened`, `gate_reminder`, `gate_resolved` or `gate_defaulted`, commit message
  `lha: gate <event> (<kind> <gate_id>)`) and POSTs the same JSON to `LHA_GATE_WEBHOOK_URL` when
  it is set. Reminders follow `LHA_GATE_ESCALATION_SECONDS`. Secrets in the question and the
  arguments are redacted. A webhook failure is reported in the activity result and never fails
  the mission.
- **Local runs with `--approve-interactive`.** The terminal approver's reminders become
  `gate_reminder` events, and the decision a `tool_approval` event, in the cycle's checkpoint.
  The opened, reminder, resolved and defaulted events also go to the webhook.

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

The rules are pinned by [`spec/obs/redact.json`](../spec/obs/redact.json). The Python suite and
the Go port (`go/internal/obs`) both run it.

`lha config` masks every `SecretStr` setting (`***` when set, `None` when unset). See
[18-configuration.md](18-configuration.md).

## OpenTelemetry and Langfuse

[`obs/otel.py`](../python/src/lha/obs/otel.py) exports traces over OTLP/HTTP. Install the
`observability` extra (`opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-http`):

```bash
cd python && uv sync --extra observability
```

and set at least one destination:

| Setting | Destination |
|---|---|
| `LHA_OTEL_EXPORTER_OTLP_ENDPOINT`, or the standard `OTEL_EXPORTER_OTLP_ENDPOINT` | an OTLP collector (Tempo, Jaeger, Datadog, Honeycomb, ...). `/v1/traces` is appended to the base URL. `OTEL_EXPORTER_OTLP_HEADERS` is passed to the collector |
| `LHA_LANGFUSE_HOST` + `LHA_LANGFUSE_PUBLIC_KEY` + `LHA_LANGFUSE_SECRET_KEY` | Langfuse, at `<host>/api/public/otel/v1/traces` with HTTP Basic auth (public key : secret key). No Langfuse SDK is needed |

With both set, every span goes to both. At process start the CLI (every `lha` command, including
`lha worker`) and `python -m lha.durable.worker` call `configure_tracing`, which installs a
`TracerProvider` (resource `service.name` = `LHA_OTEL_SERVICE_NAME`, default `lha`, and
`lha.component` = `cli` or `worker`) with one batch exporter per destination. With no
destination, with `OTEL_SDK_DISABLED=true`, or without the extra, it installs nothing (the
missing extra is logged as a warning) and every span is a no-op.

Spans, on every run path:

| Span | Where | Attributes |
|---|---|---|
| `lha.mission` | `run-local`, `mission` (`run_mission_local`) and `orchestrate` (`Orchestrator.run_mission`) | `lha.run_path`, `lha.title` (and `lha.resume` for `orchestrate`), then `lha.mission_id`, `lha.stopped_reason`, `lha.completed`, `lha.cycles`, `lha.items_done`, `lha.items_total`, `lha.cost_usd` |
| `lha.activity.run_agent_cycle` | each attempt of the durable `run_agent_cycle` activity (the worker) | `lha.mission_id`, `lha.cycle_id`, `lha.attempt`, `lha.verdict`, `lha.advanced` |
| `lha.cycle` | every `AgentLoop.run_cycle` (local, durable, the Lead in `orchestrate`) | `lha.mission_id`, `lha.cycle_id`, `lha.item_id`, `lha.verdict`, `lha.verified`, `lha.tool_calls`, `lha.turns`, `lha.head_sha` |
| `chat <model>` | every metered model call (every role: planner, lead, researchers, reviewer, implementers, replanner, memory consolidation) | `gen_ai.operation.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.response.finish_reasons`, `lha.role`, `lha.cycle_id`, `lha.cost_usd` (absent when the cost is unknown) |
| `invoke_agent <model>` | a whole `claude -p` session metered with `run_external` (the `claude_code` lead engine) | as `chat`, with the cost Claude Code reported |
| `execute_tool <name>` | every tool call of the lead and of sub-agents (researchers, reviewer, implementers), including calls the policy refuses | `gen_ai.tool.name`, `gen_ai.tool.call.id`, `lha.mission_id`, `lha.tool.ok`; status `ERROR` with the (redacted) error when the tool failed |

The `orchestrate` Lead cycle keeps its older `cycle` span, and every parallel implementer (in
`orchestrate` and in the durable `run_implementer` activity) opens an `implement` span (attributes
`gen_ai.mission_id`, `gen_ai.item`). The other organization activities (`plan_round`,
`integrate_branch`, `review_cycle`) have no span of their own; their model and tool calls are
traced as above. The workflow itself is not traced: the durable spans start in the activity, and
the Temporal history is the record of the workflow.

Spans carry metadata only. Prompts, model output, tool arguments and tool output are never
attached, and every attribute passes through the redaction below.

The Go implementation ([`go/internal/obs/tracing/`](../go/internal/obs/tracing/)) exports the
same way, with no extra to install: the same settings and destinations, the resource attributes
above, one batch exporter per destination, and the same span names and attributes for
`lha.mission`, `lha.cycle`, `chat <model>` and `execute_tool <name>` on `run-local` and
`mission`. Go also redacts the recorded error text of a failed span. Not emitted by Go yet: the
durable activity's `lha.activity.run_agent_cycle` span, the `lha.mission` span of `orchestrate`,
and the organization's agent spans (the helpers `StartActivityCycle` and `AgentSpan` exist but
nothing calls them).

Tracing never blocks or stops the agent. Spans are handed to a `BatchSpanProcessor` (a bounded
background queue that drops spans when full), an export gives up after
`LHA_OTEL_EXPORT_TIMEOUT_S` (default 5), and an unreachable backend is logged by the exporter and
otherwise ignored. At exit the provider flushes, bounded by the same timeout. An exception
inside the tracing code is swallowed; an exception from the traced work is recorded on its span
and re-raised unchanged.

The compose stack runs a Langfuse server on port 3000. Create a project in it and set the three
`LHA_LANGFUSE_*` settings to send spans there.

## Temporal UI

For durable missions the Temporal UI (<http://localhost:8080> with the compose stack) is the
primary view. Per workflow `mission:<mission_id>` it shows:

- each `run_agent_cycle` activity: its `CycleInput`, its `CycleResult` (`item_id`, `advanced`,
  `verdict`, `items_done`/`items_total`, `head_sha`, `note`, `spent_usd`), attempts and failures;
- for a mission that opted into the organization, each round's `plan_round`, `run_implementer`,
  `integrate_branch` and `review_cycle` activities and its researcher child workflows
  (`subagent:<mission_id>:researcher:<id>`);
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

- **Mission store**: every run path installs a `LedgerSink`
  ([`persistence/tracking.py`](../python/src/lha/persistence/tracking.py)) on the cost meter, so
  every metered call (planner, lead, sub-agents, memory consolidation) is written to
  `cost_ledger` under an idempotency key; unknown cost is stored as `NULL`. The store is SQLite
  (`LHA_SQLITE_PATH`; unset: a per-user file every process shares: `$XDG_DATA_HOME/lha/lha.sqlite3`, else `~/Library/Application Support/lha/lha.sqlite3` on macOS or `~/.local/share/lha/lha.sqlite3` on Linux) unless
  `LHA_POSTGRES_DSN` is set. `lha costs <mission_id>` prints a mission's calls and totals, and
  `lha missions` lists missions with their status and known spend. See
  [10-cost-and-budget.md](10-cost-and-budget.md).

Budget enforcement is described in [10-cost-and-budget.md](10-cost-and-budget.md).
