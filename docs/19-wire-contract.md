# Wire contract

What the Python and Go implementations must agree on so that one deployment can mix them: the
same Temporal names and payloads, the same on-disk mission anchor, the same Postgres schema, and
the same observable behaviour pinned by [`spec/`](../spec/). The Python implementation is the
reference; every name below is taken from its code. The Go port implements the anchor and the
spec'd behaviours today; it has no CLI and no Temporal worker yet (see [23-roadmap.md](23-roadmap.md)).

## Temporal

### Names

| Kind | Name | Defined in |
|---|---|---|
| Task queue | `LHA_TASK_QUEUE`, default `lha-mission` | [`config.py`](../python/src/lha/config.py) |
| Workflow | `MissionWorkflow` | [`durable/workflows.py`](../python/src/lha/durable/workflows.py) |
| Workflow | `SubAgentWorkflow` | [`durable/subagent_workflow.py`](../python/src/lha/durable/subagent_workflow.py) |
| Activity | `run_agent_cycle`: `CycleInput` -> `CycleResult` | [`durable/activities.py`](../python/src/lha/durable/activities.py) |
| Activity | `check_mission_health`: `HealthInput` -> `HealthReport` | same |
| Activity | `unblock_items`: `UnblockInput` -> `CycleResult` | same |
| Activity | `read_mission_snapshot`: `HealthInput` -> `CycleResult` | same |
| Activity | `notify_gate`: `GateNotice` -> `NoticeResult` | same |
| Activity | `declare_impossible`: `FinalizeInput` -> `CycleResult` | same |
| Activity | `run_subagent`: `SubAgentInput` -> `SubAgentOutput` | [`durable/agent_activities.py`](../python/src/lha/durable/agent_activities.py) |
| Signal | `human_decision_v1` (string) | [`durable/signals.py`](../python/src/lha/durable/signals.py) |
| Signal | `steer_v1` (string) | same |
| Signal | `snooze_v1` (int seconds; `0` wakes a sleeping mission) | same |
| Query | `status_v1` -> string | same |
| Query | `gate_v1` -> `GateView` or null | same |
| Query | `gate_log_v1` -> list of strings (at most 50, oldest first) | same |
| Query | `resume_at` -> float (epoch seconds, `0` when not sleeping) | `MissionWorkflow` |
| Query | `cycles_done` -> int | `MissionWorkflow` |
| Query | `last_item` -> string or null | `MissionWorkflow` |
| Query | `park_reason` -> string | `MissionWorkflow` |
| Query | `open_question` -> string (`""` when no gate is open) | `MissionWorkflow` |
| Query | `rejected_decisions` -> list of strings | `MissionWorkflow` |
| Update | `verify_verdict_v1` | constant only; no handler exists |

Identifiers:

| Identifier | Format |
|---|---|
| mission id | `mission_<12 hex>` (`ids.new_id("mission")`) |
| mission workflow id | `mission:<mission_id>` |
| sub-agent child workflow id | `subagent:<mission_id>:<role>:<12 hex from workflow.uuid4()>` |
| cycle id | `c<n>` where `n = cycles_done + 1` |
| unblock id | `u<n>` where `n` is the deadlock-retry count |
| approval gate id | `approval-<first 12 hex of the action fingerprint>` |
| deadlock gate id | `deadlock-<cycles_done>` |
| gate event cycle id | `gate:<gate id>` |
| impossible checkpoint cycle id | `impossible-<cycles_done>` |

Non-retryable `ApplicationError` types: `BudgetExceeded`, `MissionConfigError`. A committed
`.lha/decisions.ndjson` that fails hash-chain verification surfaces from `run_agent_cycle` as a
non-retryable `MissionConfigError` whose message starts `decision log failed verification:`
(`_refuse_tampered_chain` in [`durable/activities.py`](../python/src/lha/durable/activities.py)).

Mission status strings ([`durable/signals.py`](../python/src/lha/durable/signals.py)):
`RUNNING`, `SLEEPING` (on a durable timer: scheduled start, pause between cycles, or snooze),
`WAITING_ON_HUMAN` (a gate is open), `DEGRADED_PARK` (a critical dependency is down), `DONE`,
`ABORTED`, `IMPOSSIBLE`. Outcome strings: `completed`, `deadlocked`, `budget_exhausted`,
`max_cycles`, `aborted`, `impossible`. The terminal status for each outcome: `completed` ->
`DONE`; `deadlocked` and `impossible` -> `IMPOSSIBLE`; `budget_exhausted`, `max_cycles` and
`aborted` -> `ABORTED`.

Gate kinds: `tool_call` (options `approve`, `reject`; default `reject`) and `deadlock` (options
`retry`, `abort`, `impossible`; default `MissionInput.deadlock_gate_default`, which must be
`abort` or `impossible`, anything else falls back to `abort`). A `human_decision_v1` value is
matched case-insensitively against the open gate's options; a non-matching value is recorded in
`rejected_decisions` and the gate keeps waiting.

`workflow.patched` ids, for behaviour added after histories were recorded:
`lha-gate-escalation-v1` (gates walk the escalation ladder and emit `notify_gate`; the deadlock
gate offers `impossible`), `lha-sleeping-v1` (the SLEEPING timer before a cycle, and
`cycle_pause_seconds`), `lha-mission-row-v1` (the workflow's `record_mission_status` writes) and
`lha-cycle-wait-cancel-v1` (the `run_agent_cycle` activity is scheduled with cancellation type
`WAIT_CANCELLATION_COMPLETED` instead of `TRY_CANCEL`, so a cancelled workflow waits for the cycle
to acknowledge before it writes `ABORTED`; a cycle that completes anyway does not cancel the
cancellation). A worker in another language must branch on the same ids to replay histories from
either side of the change.

Timeouts and retry policies are part of the workflow's recorded commands, so they must match for
replay: see [14-running-on-temporal.md](14-running-on-temporal.md#how-a-cycle-runs). The
sub-agent activity uses a 15-minute start-to-close, 2-minute heartbeat timeout and 3 attempts.
`notify_gate` uses a 3-minute start-to-close and 3 attempts (a failure is logged in the gate
log, never fails the gate); `declare_impossible` a 5-minute start-to-close and 3 attempts.

### Payload types

All payloads are dataclasses in [`durable/types.py`](../python/src/lha/durable/types.py),
serialized by Temporal's default JSON converter: a JSON object whose keys are the field names
below, `None` as `null`. A workflow `run` takes exactly one argument; state carried across
Continue-As-New rides inside `MissionInput.state`.

| Type | Fields (default) |
|---|---|
| `MissionInput` | `mission_id: str`, `workdir: str`, `max_cycles: int (1000)`, `cycles_before_can: int (200)`, `check_commands: list[list[str]] \| null (null)`, `budget_usd: float \| null (null)`, `park_initial_seconds: int (60)`, `park_max_seconds: int (3600)`, `deadlock_gate_seconds: int (0)`, `approval_timeout_seconds: int (86400)`, `gate_escalation_seconds: list[int] ([900, 2700, 14400, 43200])`, `deadlock_gate_default: str ("abort")`, `impossible_after_failures: int (3)`, `cycle_pause_seconds: int (0)`, `resume_at: float (0.0)` (epoch seconds; scheduled start), `state: MissionState \| null (null)` |
| `MissionState` | `cycles_done: int (0)`, `status: str ("RUNNING")`, `head_sha: str ("")`, `items_done: int (0)`, `items_total: int (0)`, `last_item: str \| null`, `pending_decision: str \| null`, `steer_notes: list[str] ([])`, `parks: int (0)`, `deadlock_retries: int (0)`, `approved_actions: list[ApprovedAction] ([])`, `rejected_actions: list[str] ([])` (fingerprints), `fail_item: str \| null (null)`, `fail_streak: int (0)`, `resume_at: float (0.0)`, `escalations: int (0)`, `gate_log: list[str] ([])` |
| `CycleInput` | `mission_id`, `workdir`, `cycle_id: str`, `check_commands: list[list[str]] \| null`, `budget_usd: float \| null`, `max_cycles: int (1000)`, `steer_notes: list[str] ([])`, `approved_actions: list[ApprovedAction] ([])` |
| `CycleResult` | `item_id: str \| null`, `advanced: bool`, `head_sha: str`, `is_complete: bool`, `items_done: int`, `items_total: int`, `note: str ("")`, `verdict: str ("")`, `is_deadlocked: bool (false)`, `item_blocked: bool (false)`, `reason: str ("")`, `spent_usd: float (0.0)`, `item_split: bool (false)`, `pending_approvals: list[PendingApproval] ([])`, `used_approvals: list[str] ([])` (fingerprints) |
| `PendingApproval` | `fingerprint: str`, `tool: str`, `reason: str`, `arguments: str ("")` (the `repr` of the arguments, at most 2000 chars) |
| `ApprovedAction` | `fingerprint: str`, `summary: str` (`<tool> <arguments>`, at most 500 chars) |
| `GateView` | `gate_id: str`, `kind: str` (`tool_call` \| `deadlock`), `question: str`, `options: list[str]`, `default_action: str`, `opened_at: str ("")`, `deadline: str ("")`, `escalations_sent: int (0)`, `next_escalation_at: str ("")`, `recommended: str ("")`, `request: PendingApproval \| null (null)`; times are ISO 8601 UTC to the second |
| `GateNotice` | `mission_id`, `workdir`, `gate_id`, `kind`, `event: str` (`opened` \| `reminder` \| `resolved` \| `defaulted`), `question: str ("")`, `options: list[str] ([])`, `default_action: str ("")`, `decision: str ("")`, `step: int (0)`, `deadline: str ("")`, `request: PendingApproval \| null (null)`, `at: str ("")` (when the event happened, workflow time, ISO 8601 UTC to the second; empty from older workflows, and the activity then uses its own clock) |
| `NoticeResult` | `recorded: bool`, `webhook: str ("off")` (`off` \| `sent` \| `failed: <reason>`), `stored: bool (false)` (the `hitl_gates` row was written) |
| `FinalizeInput` | `mission_id`, `workdir`, `cycle_id`, `reason: str ("")` |
| `HealthInput` | `mission_id`, `workdir` |
| `HealthReport` | `healthy: bool`, `reason: str ("")`, `degraded: list[str] ([])` |
| `UnblockInput` | `mission_id`, `workdir`, `cycle_id` |
| `MissionResult` | `mission_id`, `completed: bool`, `cycles: int`, `head_sha`, `items_done`, `items_total`, `outcome: str ("completed")`, `reason: str ("")`, `status: str ("")` |
| `SubAgentInput` | `role_name: str`, `objective: str`, `workdir`, `mission_id`, `allow_egress: bool (false)` |
| `SubAgentOutput` | `role: str`, `brief: str`, `tool_calls: int`, `turns: int` |
| `FanOutResult` | `outputs: list[SubAgentOutput]`, `failures: list[str]` (returned in workflow code, not a Temporal payload) |

`check_commands: null` means the default Python checks; an explicit empty list is rejected with
`MissionConfigError`, as are `cycles_before_can < 1` and `max_cycles < 0`. `items_total` counts
work items: `split` parents are excluded. `gate_escalation_seconds` offsets outside
`(0, gate timeout)` are dropped; the rest are sorted and de-duplicated
([`hitl/escalation.py`](../python/src/lha/hitl/escalation.py)).

`MissionState.resume_at` is set from `MissionInput.resume_at` on the first run, by
`snooze_v1` (`now + seconds`, or `0`), and after each cycle when `cycle_pause_seconds > 0`.
Before each cycle the workflow sleeps (status `SLEEPING`) until it; a `snooze_v1` during the
sleep moves or ends it. `fail_item`/`fail_streak` count consecutive non-passing verified cycles
on one item; when `fail_streak >= impossible_after_failures` the deadlock gate sets
`recommended: "impossible"`. `gate_log` lines are `<ISO time> <text>`, at most 50.

The JSON that `notify_gate` commits to the anchor and POSTs to `LHA_GATE_WEBHOOK_URL` (when set)
is: `source` (`"lha"`), `mission_id`, `gate_id`, `kind`, `event`, `question` (secrets
redacted), `options`, `default_action`, `deadline`, plus `decision` and `step` when non-empty,
and `request` (`fingerprint`, `tool`, `arguments` redacted, `reason`) for a tool-call gate
(`gate_notice_payload` in [`durable/activities.py`](../python/src/lha/durable/activities.py)).

An action fingerprint is the first 32 lowercase hex characters of the SHA-256 of the canonical JSON
`{"arguments": <arguments>, "tool": <tool name>}` (sorted keys, separators `,` and `:`, non-JSON
values stringified with `str`, non-ASCII escaped as `\uXXXX`) ([`hitl/approvals.py`](../python/src/lha/hitl/approvals.py)). A worker in
another language must compute it identically for approvals to carry across cycles.

### ClaimCheck codec

[`durable/codec.py`](../python/src/lha/durable/codec.py) keeps large payloads out of the history.
The client, every worker and the replayer must install it
([`durable/data_converter.py`](../python/src/lha/durable/data_converter.py)) over the same object
store.

- **Encode**: a payload whose `data` is larger than 32 KiB (32768 bytes) is serialized as a whole
  (the `Payload` protobuf, deterministic serialization, including every metadata entry) and
  stored in the object store. The journaled payload becomes `metadata = {"encoding":
  "lha/claimcheck/v2"}`, `data =` the ASCII key. Smaller payloads pass through unchanged.
- **Key**: lowercase hex SHA-256 of the stored bytes (64 characters); the blob lives at
  `<LHA_OBJECT_STORE_ROOT>/<key>`, written atomically. Keys are validated before use and blobs are
  re-hashed on read.
- **Decode**: `lha/claimcheck/v2` payloads are replaced by the parsed stored `Payload`. The legacy
  encoding `lha/claimcheck` (v1) is still decoded: its blob is the raw data, and the original
  encoding is in metadata `lha-orig-encoding`.
- No encryption: blobs are plaintext.

### Known cross-language limitation

Temporal replay compares the commands a worker issues with the recorded history, including
command sequence ids. The Python SDK numbers activity ids and timer ids with separate counters; the
Go SDK uses one shared counter. A history containing both activities and timers (any mission that
parked, slept, or opened a gate) therefore does not replay on a worker of the other
language. Histories with activities only are not affected by this. Until this is resolved, keep a
mission's workers to one language once it has recorded a timer.

## Mission anchor (`.lha/`)

The anchor is a directory in the mission's git repository, committed with the agent's work in one
commit per checkpoint ([`state/mission_anchor.py`](../python/src/lha/state/mission_anchor.py);
design in [06-mission-anchor.md](06-mission-anchor.md)). Reads come from `HEAD`, not the working
tree. The files are force-added so a `.gitignore` cannot exclude them.

| File | Format |
|---|---|
| `mission.json` | `MissionSpec` as JSON, 2-space indent: `title`, `description`, `acceptance` (`""`), `references` (`[]`, workspace-relative paths), `schema_version` (1) |
| `checklist.json` | `Checklist` as JSON, 2-space indent: `items`, `schema_version`. Each item: `id`, `description`, `status` (`todo`\|`in_progress`\|`blocked`\|`done`\|`split`), `verified_by`, `depends_on`, `attempts`, `consecutive_failures`, `last_failure`, `allow_harness_edits`, `witnesses` (`[]`), `notes`, `schema_version` |
| `progress.md` | `# Mission: <title>`, the description, `## Progress`, then one `- <cycle_id> [<item>] <description>: <note>` line per checkpoint; trimmed oldest-first to 16 000 characters with a `- _(older entries trimmed)_` marker |
| `decisions.ndjson` | SHA-256 hash chain of `DecisionRecord`s (`decision`, `rationale`, `alternatives_rejected`, `affected`, `cycle_id`), one `\n`-terminated line each; format below |
| `events.ndjson` | one `EventRecord` per line: `kind`, `cycle_id`, `payload`, `payload_ref` |
| `ownership.json` | optional; `FileOwnershipMap` as JSON, 2-space indent: `owners` (normalized, case-folded repo-relative path -> writer id `implementer-<item id>`); absent means an empty map |

A cycle checkpoint writes an event with `kind: "cycle"` and payload `item_id`, `verified`,
`verdict`, `status`, `tool_calls`, `split_into` (child ids, `[]` unless split), `checks` (each
`name`, `passed`, `gating`, `exit_code`, `duration_s`). An `orchestrate` integration
checkpoint's `cycle` event adds `writer` and `branch`, and it is followed by a `kind: "ticket"`
event (`ticket_id`, `item_id`, `role`, `write_set`, `branch`, `status`, `history` (list of
`{status, note}`), `ownership_violations`). The exactly-once check looks for the `cycle` event
among the last 64 lines of `events.ndjson` at `HEAD`.

Durable gates add their own checkpoints (the checklist is rewritten unchanged). Each
`notify_gate` call commits one event with `kind` `gate_opened`, `gate_reminder`,
`gate_resolved` or `gate_defaulted`, `cycle_id` `gate:<gate id>` and the gate payload described
under [Payload types](#payload-types). A mission declared impossible gets a final checkpoint with
cycle id `impossible-<n>` and two events: `mission_impossible` (`reason`, `blocked` (item ids),
`items_done`, `items_total`) and `cycle` (`outcome: "impossible"`, `blocked`), the second one
making a retry of `declare_impossible` a no-op. A human `retry` at the deadlock gate commits an
`unblock` event (`items`: the unblocked ids).

Within a cycle checkpoint, each tool call that reached an approval gate adds a `tool_approval`
event: `tool`, `arguments` (redacted), `reason`, `fingerprint`, `decision` (`approve`, `reject`,
or `pending` when a durable cycle queued it for the workflow), `approved`, `resolved_by`,
`defaulted`. The local `TerminalApprover` also adds a `gate_reminder` event per reminder
(`gate_id`, `gate: "tool_call"`, `step`, `tool`, `fingerprint`). A flaky-check quarantine adds
a `check_quarantined` event, and a quarantined check that failed every attempt a
`quarantined_check_failed` event, each with `check`, `revision` (the work tree's git tree id),
`passes` and `fails`. The set of quarantined checks is the `check` of every committed
`check_quarantined` event.

Commit messages: `lha: initialize mission anchor`,
`lha: complete|attempt|block|split <id> (<description>)` (orchestrate appends
` [merged <branch>]` to an integration commit, which is a two-parent merge commit),
`lha: review reopened <id>`, `lha: unblock <ids> (human retry)`,
`lha: gate <event> (<kind> <gate id>)`, `lha: mission declared impossible`.

Decisions reach `decisions.ndjson` two ways: the agent's `record_decision` tool queues a
`DecisionRecord` in memory (`GitMissionAnchor.record_decision`), and a `Checkpoint` can carry
`decisions`. At the next checkpoint both are chained onto the committed log, and a record with an
empty `cycle_id` gets the checkpoint's. The queue lives in memory, so a cycle that ends without
a checkpoint (a crash) loses it. In `orchestrate`, each parallel implementer records into its own
buffer, and only merged work has its decisions committed.

Fields added since the first release (`references`, `witnesses`, status `split`) have empty
defaults, so older anchors still load. The Go `contracts` package reads and writes them (and
implements `Checklist.Split`).

The `Check` shape in `contracts/verify.py` also gained `where` (`"sandbox"` default, or
`"trusted"`); Go validates the same two values.

`decisions.ndjson` line format
([`coordination/decision_log.py`](../python/src/lha/coordination/decision_log.py); cases in
`spec/coordination/decision_chain.json`, key `log`):

- A chained line is the JSON object `{"prev": P, "hash": H, "record": R}`. `R` is the full
  `DecisionRecord` (all five fields). `H` is the lowercase hex of
  `sha256(P + "\n" + canonical(R))`, where `canonical` is `R` re-serialized with keys sorted,
  separators `,` and `:` with no spaces, and non-ASCII characters (including U+2028/U+2029) left
  unescaped. Python writes the envelope with `json.dumps` default separators (`", "`, `": "`) and
  keys in the order `prev`, `hash`, `record`. Readers must not depend on the envelope's
  whitespace or key order: the hash covers only `canonical(R)` as parsed.
- The first line's `P` is 64 `0`s. Each later chained line's `P` is the previous line's running
  hash.
- A legacy line is a bare `DecisionRecord` object (no `hash`/`record` keys), as written before
  the chain existed. Legacy lines may only form a leading prefix. Each one advances the running
  hash to `sha256(running + "\n" + canonical(line))`, so the first chained line's `P` is the
  running hash after the prefix. A legacy line after a chained line is invalid.
- Verification fails on an unparseable line, a `P` that is not the running hash, an `H` that
  does not match, a legacy line after a chained one, or (for the anchor) a final line without
  its `\n`. Split lines on `\n` only.
- Appending: parse and verify the committed file, then write committed content + new envelope
  lines chained from the last running hash. Never rewrite existing lines.

Two files under `.git/` (outside the worktree, so a reset keeps them) are shared between attempts:

| File | Format |
|---|---|
| `.git/lha-cycle.lock` | empty file locked with `flock(LOCK_EX)` for the duration of an attempt |
| `.git/lha/spend.ndjson` | one line per attempt: `key`, `cycle_id`, `usd`, `unknown`, `calls`; readers keep the last row per `key` |

`go/internal/state/crossimpl_test.go` writes an anchor in Go and reads it with Python, and the
reverse, and requires identical results. For `decisions.ndjson` that includes the bytes: Go
re-encodes every Python-written link to the same line and hash, and each implementation verifies
the other's chain. The Go chain code is `go/internal/state/decision_chain.go`, and the Go anchor
verifies and appends through it just as the Python anchor does. The Go anchor carries
`ownership.json` along (restore, force-add), but nothing in Go reads it.

## Postgres schema

Migrations are plain SQL in [`db/migrations/`](../db/migrations/), one file per version:

| Version | Changes |
|---|---|
| `0001_init` | `CREATE EXTENSION vector`; tables below; seeds `schema_registry` (`lha-core`, 1) |
| `0002_idempotent_ledger` | `cost_ledger` gains `idempotency_key` (unique index), `role`, `cost_known`; `semantic_memory.id` becomes `text` |
| `0003_cost_unknown_usd_null` | `cost_ledger.usd` becomes nullable with no default; unknown-cost rows set to `NULL` |
| `0004_memory_skills` | additive only (`IF NOT EXISTS`): `semantic_memory` gains `kind text NOT NULL DEFAULT 'semantic'` and `metadata jsonb NOT NULL DEFAULT '{}'`, plus index `semantic_memory_mission` (`mission_id`); new table `skills` with index `skills_namespace` (`namespace`) |
| `0005_hitl_gates` | `hitl_gates` is keyed by (`mission_id`, `gate_id`) instead of `gate_id` (a gate id such as `deadlock-3` recurs across missions; nothing wrote the table before); gains `kind text NOT NULL DEFAULT ''`, `options jsonb NOT NULL DEFAULT '[]'`, `request jsonb`, `reminders int NOT NULL DEFAULT 0`, `updated_at timestamptz`, plus index `hitl_gates_opened` (`created_at`) |

Tables after all five:

| Table | Columns |
|---|---|
| `missions` | `mission_id` PK, `title`, `description`, `acceptance`, `status` text (default `RUNNING`; the values in the column comment are the seven status strings above, not a database enum), `workflow_id`, `run_id`, `latest_snapshot_id`, `latest_session_id`, `head_sha`, `schema_version`, `created_at`, `updated_at` |
| `checklist_items` | PK (`mission_id`, `item_id`), `description`, `status`, `verified_by` jsonb, `depends_on` jsonb, `attempts`, `schema_version`, `updated_at` |
| `episodic_events` | `id` bigserial PK, `mission_id`, `cycle_id`, `ts`, `kind`, `payload` jsonb, `payload_ref`, `schema_version`; index (`mission_id`, `ts`) |
| `semantic_memory` | `id` text PK, `mission_id` (indexed), `text`, `tsv` tsvector (GIN), `embedding` vector(1024) (HNSW cosine), `embedding_model`, `embedding_version`, `valid`, `source_event_id`, `created_at`, `schema_version`, `kind` (default `semantic`), `metadata` jsonb (default `{}`) |
| `skills` | `id` text PK, `namespace` (default `global`, indexed), `name`, `description`, `code` (default `''`), `preconditions` jsonb (default `[]`), `provenance` (default `''`), `expires_at` text (ISO date), `verified` (default false), `uses` (default 0), `created_at`, `updated_at`, `schema_version` |
| `idempotency_keys` | `key` PK, `result_ref`, `created_at` |
| `cost_ledger` | `id` bigserial PK, `mission_id`, `cycle_id`, `ts`, `model`, `input_tokens`, `output_tokens`, `usd` numeric(12,6) nullable, `idempotency_key` unique, `role`, `cost_known` |
| `hitl_gates` | PK (`mission_id`, `gate_id`), `kind` (`tool_call` \| `deadlock`), `question`, `risk` (`irreversible` for a tool call, else the kind), `default_action`, `options` jsonb, `request` jsonb (`fingerprint`, `tool`, `arguments`, `reason`, and `argv` from the terminal approver; secrets redacted), `status` (`OPEN` \| `ESCALATED` \| `RESOLVED` \| `DEFAULTED`), `deadline`, `decision`, `resolved_by`, `reminders`, `created_at` (when the gate opened), `resolved_at`, `updated_at` |
| `snapshots` | `snapshot_id` PK, `mission_id`, `sandbox_kind`, `meta` jsonb, `created_at` |
| `schema_registry` | PK (`artifact`, `version`), `applied_at`, `notes` |
| `schema_migrations` | `version` text PK (the file stem), `applied_at` |

`lha db migrate` ([`persistence/db.py`](../python/src/lha/persistence/db.py)) connects in
autocommit mode, takes `pg_advisory_lock(0x6C68616D69677238)` so concurrent runs serialize,
creates `schema_migrations` if needed, and applies each `*.sql` file whose stem is not recorded,
in name order. Each file runs as one batch inside its own transaction together with the insert of
its `schema_migrations` row, so a failing migration leaves neither schema changes nor a record.
Each file also inserts its own row, so a database initialized by Postgres'
`docker-entrypoint-initdb.d` (the compose `appdb` service) counts as migrated.

### Mission store

Every run path persists through one `MissionStore` interface
([`persistence/store.py`](../python/src/lha/persistence/store.py)), opened by `open_store`:
`PostgresStore` when `LHA_POSTGRES_DSN` is set, otherwise `SqliteStore`. `PostgresStore` refuses
to open unless `schema_migrations` lists all five versions above (run `lha db migrate` first).
If Postgres is configured but unusable and `LHA_POSTGRES_FALLBACK_TO_SQLITE` is true (the
default), the run falls back to SQLite and the store's `degraded_reason` says why; with the
fallback off, the durable cycle activity fails with a non-retryable `MissionConfigError`.

`SqliteStore` ([`persistence/sqlite.py`](../python/src/lha/persistence/sqlite.py)) uses the
file `LHA_SQLITE_PATH` (unset: a per-user file every process shares: `$XDG_DATA_HOME/lha/lha.sqlite3`, else `~/Library/Application Support/lha/lha.sqlite3` on macOS or `~/.local/share/lha/lha.sqlite3` on Linux; a relative path resolves against the current
directory, with a warning; a path inside the mission checkout is moved to
`<workdir>/.git/lha/<name>`), WAL mode, and creates
its schema on open from its own migration list (`sqlite_0001_init`, `sqlite_0002_hitl_gates`,
recorded in its own `schema_migrations`). It has the same `missions`, `hitl_gates`,
`cost_ledger`, `episodic_events`, `semantic_memory` and `skills` columns as Postgres after
`0005`, with SQLite types: JSON as text, booleans as integers, timestamps as ISO 8601 text, and
`semantic_memory.embedding` as JSON text instead of `vector(1024)`, with no `tsv` column. It has
no `checklist_items`, `idempotency_keys` or `snapshots` tables.

What is written, and by whom:

| Table | Written by |
|---|---|
| `missions` | `MissionTracker` ([`persistence/tracking.py`](../python/src/lha/persistence/tracking.py)) and the `record_mission_status` activity, an upsert on `mission_id` (an empty title or description keeps the stored one). The status is monotonic: a row in `DONE`, `IMPOSSIBLE` or `ABORTED` keeps it when a non-terminal status arrives (the other columns still update), unless the caller passes `reopen=True` |
| `hitl_gates` | `record_gate_event`: the `notify_gate` activity (durable gates; `resolved_by` is `human (human_decision signal)` or `default (timeout)`) and the local `TerminalApprover` (`terminal:<login>`, `timeout`, `end of input`, `non-interactive (stdin is not a TTY)`). An upsert on (`mission_id`, `gate_id`): `opened` (re)opens the row unless it repeats the stored opening time, `reminder` raises `reminders` (never lowers it) on an open row, `resolved` / `defaulted` close an open row and are no-ops on a closed one; an event for a missing row inserts it |
| `cost_ledger` | `LedgerSink`, installed as `CostMeter.on_record`: one row per metered model call, keyed by an idempotency key derived from mission id, cycle id and `<key prefix>#<sequence number>`, so a repeated write is a no-op. Key prefixes: `<cycle id>@<attempt>` for a durable cycle (a retried attempt's calls are new rows), `sub:<workflow id>:<activity id>@<attempt>` for a durable sub-agent, `planner` for the Planner's calls in `lha mission-start`; a local runner uses an empty prefix and backfills the calls its meter recorded before the store opened |
| `episodic_events`, `semantic_memory`, `skills` | the memory plane ([`memory/service.py`](../python/src/lha/memory/service.py)) when `LHA_MEMORY_ENABLED` is true |
| `checklist_items`, `idempotency_keys`, `snapshots` | nothing; the tables exist in Postgres only |

Mission row statuses actually written: a local runner (`run-local`, `mission`, `orchestrate`)
writes `RUNNING` at start and, at the end, `DONE` (complete), `IMPOSSIBLE` (deadlocked) or
`ABORTED` (anything else). `lha mission-start` writes the row as `RUNNING` (`SLEEPING` with
`--start-in-seconds`) with the workflow id before starting the workflow, and `ABORTED` if the
start fails. In a durable run the cycle activity writes `RUNNING` when a cycle starts, `ABORTED`
when the budget is exceeded, and after the checkpoint `DONE` (checklist complete),
`WAITING_ON_HUMAN` (the cycle queued an irreversible action for approval) or `RUNNING`
(including a deadlocked checklist: the workflow decides that ending). The workflow writes the statuses only it decides through the
`record_mission_status` activity (`MissionStatusInput`: `mission_id`, `workdir`, `status`,
`head_sha`, `reason`; the reason is logged, not stored): `SLEEPING`, `DEGRADED_PARK`,
`WAITING_ON_HUMAN` when a gate opens, and the final status of every ending (`DONE`,
`IMPOSSIBLE`, `ABORTED` for abort at the deadlock gate, `max_cycles`, budget, a non-retryable
failure or a cancellation). Those writes are best effort; `status_v1` stays the live source.
Every gate event is also written to `hitl_gates` (above); the anchor's `gate_*` events and the
`gate_log_v1` query remain the full history.

`lha missions` lists mission rows with their cost summary, `lha costs <mission id>` prints a
mission's cost summary and its most recent ledger rows, and `lha gates [mission id]` lists the
recorded gates, from the same store.

## Conformance cases (`spec/`)

JSON files exported from the Python implementation by
[`python/scripts/export_spec.py`](../python/scripts/export_spec.py); see
[`spec/README.md`](../spec/README.md).

| File | Pins | Python | Go |
|---|---|---|---|
| `safety/classify_command.json` | argv -> gate reason or `null` | yes | yes |
| `safety/egress.json` | host normalization (IDNA 2008), URL parsing, public-address checks, allow-list | yes | yes |
| `obs/redact.json` | secret redaction | yes | yes |
| `contracts/check_names.json` | check names from argv, de-duplication | yes | yes |
| `state/checklist.json` | next actionable item, completion, deadlock reasons, transitions, `split` (child ids, dependencies and witnesses) | yes | yes |
| `coordination/decision_chain.json` | canonical JSON bytes and SHA-256 chain of the decision log; `log`: an anchor `decisions.ndjson` with a legacy prefix, its running hashes, and verification verdicts for tampered variants | yes | yes |
| `coordination/shared_paths.json` | files only the lead engineer may write | yes | not yet |
| `verify/harness_files.json` | test/harness files the agent may not weaken | yes | yes |
| `model/pricing.json` | Claude price table and per-call cost | yes | yes |

Python runs them in [`tests/unit/test_spec_conformance.py`](../python/tests/unit/test_spec_conformance.py),
Go in `go/internal/spec/conformance_*_test.go`. `shared_paths.json` waits for a Go port of the
ownership map.

The Go port has no Temporal worker, so the payload types, queries and fingerprints above have no
Go implementation yet; the Go `config` package already reads the new settings
([18-configuration.md](18-configuration.md)).
