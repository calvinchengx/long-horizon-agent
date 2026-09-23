# Wire contract

What the Python and Go implementations must agree on so that one deployment can mix them: the
same Temporal names and payloads, the same on-disk mission anchor, the same Postgres schema, and
the same observable behaviour pinned by [`spec/`](../spec/). The Python implementation is the
reference; every name below is taken from its code. The Go port implements the anchor and the
spec'd behaviours today; its Temporal worker is not written yet (see [23-roadmap.md](23-roadmap.md)).

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
| Activity | `run_subagent`: `SubAgentInput` -> `SubAgentOutput` | [`durable/agent_activities.py`](../python/src/lha/durable/agent_activities.py) |
| Signal | `human_decision_v1` (string) | [`durable/signals.py`](../python/src/lha/durable/signals.py) |
| Signal | `steer_v1` (string) | same |
| Query | `status_v1` -> string | same |
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

Non-retryable `ApplicationError` types: `BudgetExceeded`, `MissionConfigError`.

Mission status strings: `RUNNING`, `SLEEPING` (defined, never set), `WAITING_ON_HUMAN`,
`DEGRADED_PARK`, `DONE`, `ABORTED`, `IMPOSSIBLE`. Outcome strings: `completed`, `deadlocked`,
`budget_exhausted`, `max_cycles`, `aborted`.

Timeouts and retry policies are part of the workflow's recorded commands, so they must match for
replay: see [14-running-on-temporal.md](14-running-on-temporal.md#how-a-cycle-runs). The
sub-agent activity uses a 15-minute start-to-close, 2-minute heartbeat timeout and 3 attempts.

### Payload types

All payloads are dataclasses in [`durable/types.py`](../python/src/lha/durable/types.py),
serialized by Temporal's default JSON converter: a JSON object whose keys are the field names
below, `None` as `null`. A workflow `run` takes exactly one argument; state carried across
Continue-As-New rides inside `MissionInput.state`.

| Type | Fields (default) |
|---|---|
| `MissionInput` | `mission_id: str`, `workdir: str`, `max_cycles: int (1000)`, `cycles_before_can: int (200)`, `check_commands: list[list[str]] \| null (null)`, `budget_usd: float \| null (null)`, `park_initial_seconds: int (60)`, `park_max_seconds: int (3600)`, `deadlock_gate_seconds: int (0)`, `approval_timeout_seconds: int (86400)`, `state: MissionState \| null (null)` |
| `MissionState` | `cycles_done: int (0)`, `status: str ("RUNNING")`, `head_sha: str ("")`, `items_done: int (0)`, `items_total: int (0)`, `last_item: str \| null`, `pending_decision: str \| null`, `steer_notes: list[str] ([])`, `parks: int (0)`, `deadlock_retries: int (0)`, `approved_actions: list[ApprovedAction] ([])`, `rejected_actions: list[str] ([])` (fingerprints) |
| `CycleInput` | `mission_id`, `workdir`, `cycle_id: str`, `check_commands: list[list[str]] \| null`, `budget_usd: float \| null`, `max_cycles: int (1000)`, `steer_notes: list[str] ([])`, `approved_actions: list[ApprovedAction] ([])` |
| `CycleResult` | `item_id: str \| null`, `advanced: bool`, `head_sha: str`, `is_complete: bool`, `items_done: int`, `items_total: int`, `note: str ("")`, `verdict: str ("")`, `is_deadlocked: bool (false)`, `item_blocked: bool (false)`, `reason: str ("")`, `spent_usd: float (0.0)`, `item_split: bool (false)`, `pending_approvals: list[PendingApproval] ([])`, `used_approvals: list[str] ([])` (fingerprints) |
| `PendingApproval` | `fingerprint: str`, `tool: str`, `reason: str`, `arguments: str ("")` (the `repr` of the arguments, at most 2000 chars) |
| `ApprovedAction` | `fingerprint: str`, `summary: str` (`<tool> <arguments>`, at most 500 chars) |
| `HealthInput` | `mission_id`, `workdir` |
| `HealthReport` | `healthy: bool`, `reason: str ("")`, `degraded: list[str] ([])` |
| `UnblockInput` | `mission_id`, `workdir`, `cycle_id` |
| `MissionResult` | `mission_id`, `completed: bool`, `cycles: int`, `head_sha`, `items_done`, `items_total`, `outcome: str ("completed")`, `reason: str ("")`, `status: str ("")` |
| `SubAgentInput` | `role_name: str`, `objective: str`, `workdir`, `mission_id`, `allow_egress: bool (false)` |
| `SubAgentOutput` | `role: str`, `brief: str`, `tool_calls: int`, `turns: int` |
| `FanOutResult` | `outputs: list[SubAgentOutput]`, `failures: list[str]` (returned in workflow code, not a Temporal payload) |

`check_commands: null` means the default Python checks; an explicit empty list is rejected with
`MissionConfigError`. `items_total` counts work items: `split` parents are excluded.

An action fingerprint is the first 32 lowercase hex characters of the SHA-256 of the canonical JSON
`{"arguments": <arguments>, "tool": <tool name>}` (sorted keys, separators `,` and `:`, non-JSON
values stringified) ([`hitl/approvals.py`](../python/src/lha/hitl/approvals.py)). A worker in
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
parked, or opened a gate with a timeout) therefore does not replay on a worker of the other
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
among the last 64 lines of `events.ndjson` at `HEAD`. Commit messages:
`lha: initialize mission anchor`, `lha: complete|attempt|block|split <id> (<description>)`
(orchestrate appends ` [merged <branch>]` to an integration commit, which is a two-parent merge
commit), `lha: review reopened <id>`, `lha: unblock <ids> (human retry)`.

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

Tables after all three:

| Table | Columns |
|---|---|
| `missions` | `mission_id` PK, `title`, `description`, `acceptance`, `status` (default `RUNNING`), `workflow_id`, `run_id`, `latest_snapshot_id`, `latest_session_id`, `head_sha`, `schema_version`, `created_at`, `updated_at` |
| `checklist_items` | PK (`mission_id`, `item_id`), `description`, `status`, `verified_by` jsonb, `depends_on` jsonb, `attempts`, `schema_version`, `updated_at` |
| `episodic_events` | `id` bigserial PK, `mission_id`, `cycle_id`, `ts`, `kind`, `payload` jsonb, `payload_ref`, `schema_version`; index (`mission_id`, `ts`) |
| `semantic_memory` | `id` text PK, `mission_id`, `text`, `tsv` tsvector (GIN), `embedding` vector(1024) (HNSW cosine), `embedding_model`, `embedding_version`, `valid`, `source_event_id`, `created_at`, `schema_version` |
| `idempotency_keys` | `key` PK, `result_ref`, `created_at` |
| `cost_ledger` | `id` bigserial PK, `mission_id`, `cycle_id`, `ts`, `model`, `input_tokens`, `output_tokens`, `usd` numeric(12,6) nullable, `idempotency_key` unique, `role`, `cost_known` |
| `hitl_gates` | `gate_id` PK, `mission_id`, `question`, `risk`, `default_action`, `status` (`OPEN`), `deadline`, `decision`, `resolved_by`, `created_at`, `resolved_at` |
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

The runtime reads and writes only `cost_ledger` and `missions` through
[`persistence/repositories.py`](../python/src/lha/persistence/repositories.py) and
`semantic_memory` through [`memory/semantic_pg.py`](../python/src/lha/memory/semantic_pg.py), and
none of these is called by a CLI command or workflow yet.

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
