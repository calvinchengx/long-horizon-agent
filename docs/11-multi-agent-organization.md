# Multi-agent organization

LHA's organization is asymmetric. One Lead Engineer makes all coupled code writes on a single
thread. Other agents run only for work that parallelizes without write conflicts: reading the
codebase and reviewing the Lead's work in a fresh context. Code:
[`python/src/lha/agents/`](../python/src/lha/agents/),
[`python/src/lha/coordination/`](../python/src/lha/coordination/).

## Why asymmetric

Parallel writers to the same code make design decisions that conflict, and merging them costs
more than the parallelism saves. Reads have no side effects, so they fan out safely. A reviewer
that does not share the author's context can catch errors the author cannot. So parallelism is
limited to reads and review. The code enforces this split:

- `research_fanout` refuses a role that can mutate.
- The reviewer's dispatcher is read-only.
- Only the Lead's dispatcher has `allow_mutating=True`.

## What runs, by command

| Command | Agents involved |
|---|---|
| `lha run-local` | Lead (checklist given with `--item` or `--checklist`), Replanner when an item blocks |
| `lha mission` | Planner (skipped with `--checklist`), then Lead, Replanner when an item blocks |
| `lha orchestrate` | Planner, then per item: 2 Researchers, Lead, Reflection on failure, Reviewer on success; Replanner when an item blocks |
| `lha mission-start` + `lha worker` | Planner (at start, in the CLI process; skipped with `--checklist`), then Lead cycles inside `MissionWorkflow`, Replanner when an item blocks |

All run paths build the Lead the same way ([agent/assembly.py](../python/src/lha/agent/assembly.py)),
so the Replanner runs wherever the Lead does. The durable workflow otherwise runs only the Lead
loop. Research fan-out, review and reflection
exist only in the local `orchestrate` flow ([Durable execution](08-durable-execution.md)).

### `lha orchestrate` wiring

[cli/main.py](../python/src/lha/cli/main.py) builds one `CostMeter`, plans with the `Planner`, and
calls `Orchestrator.run_mission()` ([orchestrator.py](../python/src/lha/agents/orchestrator.py))
with the same meter. For each cycle, the orchestrator:

1. Calls `authorize_next`, reads the anchor snapshot, and stops if the mission is complete or
   deadlocked.
2. Runs `research_fanout` with two queries ("Find context relevant to…", "Find existing
   files/code related to…"). Each query goes to a separate `SubAgent` concurrently
   (`asyncio.gather`), using a read-only dispatcher over the default local tools. Failed
   researchers come back with `error` set. Auth errors (401/403), `BudgetExceeded` and
   cancellation are raised.
3. Builds the anchor text from the immutable mission spec, any reflection for the item, and the
   research briefs, then runs one Lead `AgentLoop` cycle.
4. If verification failed and the item is not blocked, it calls `reflect_on_failure` with the
   Lead's model and stores the post-mortem for the next attempt.
5. If verification passed, the `Reviewer` reviews `git diff base..head` (excluding `.lha`, capped
   at 20,000 chars, then 8,000 inside the reviewer). A blocking verdict reopens the item: status
   `todo`, `verified_by` cleared, the review notes attached, and a checkpoint committed.

The command-line knobs are `--check`, `--no-default-checks`, `--sandbox`, `--unsafe-local`,
`--reference` and `--approve-interactive`; the rest comes from `LHA_*` settings. The CLI does not
expose `allow_egress`, so researchers have no network tools even though their role spec sets
`allow_egress=True`. The Lead gets `fetch_url` when `LHA_WEB_ALLOW_HOSTS` is set.

## Roles

`ROLES` in [roles.py](../python/src/lha/agents/roles.py) is the org chart as data: model tier,
system prompt, `allow_mutating`, `allow_egress` and `max_turns` (8). With the `claude` backend,
`model_for_role()` ([router.py](../python/src/lha/agents/router.py)) maps opus to
`claude-opus-4-8`, sonnet to `claude-sonnet-4-6` and haiku to `claude-haiku-4-5-20251001`. Every
other backend uses the single configured model for all roles.

| Role / module | Tier | Mutating | What the code does | Used by a CLI path |
|---|---|---|---|---|
| Planner ([planner.py](../python/src/lha/agents/planner.py)) | opus | no | One model call. It parses a JSON array into `ChecklistItem`s with ids `01`, `02`, … and keeps only dependencies on earlier steps (dropped ones are noted). If nothing parses, it creates a single item from the description | `mission`, `orchestrate`, `mission-start` |
| Lead (`AgentLoop`, [agent/loop.py](../python/src/lha/agent/loop.py)) | opus | yes | Works one item per cycle: tools, gating checks and witnesses, checkpoint commit | all |
| Replanner ([replanner.py](../python/src/lha/agents/replanner.py)) | lead's model | no tools | One model call when an item has just become `blocked`: given the mission, the item, its witnesses and the latest failure report (last 3,000 chars), returns a JSON array of 2 to 6 smaller steps. `Checklist.split` replaces the item with them; fewer than 2 usable steps means no split. Bounded by `LHA_MAX_REPLANS` and `LHA_MAX_SPLIT_DEPTH` | all (unless `LHA_MAX_REPLANS=0`) |
| Researcher ([team.py](../python/src/lha/agents/team.py)) | haiku | no | Read-only `SubAgent` that returns a brief (capped at 8,000 chars) | `orchestrate` |
| Reviewer ([reviewer.py](../python/src/lha/agents/reviewer.py)) | opus | no | Fresh-context review that returns JSON `verdict` / `blocking_issues` / `advisory`. An unparseable reply counts as blocking, and "approve" with issues counts as "block" | `orchestrate` |
| Reflection ([reflection.py](../python/src/lha/agents/reflection.py)) | lead's model | no tools | Short post-mortem (at most 2,000 chars) prepended to the next attempt | `orchestrate` |
| Tester (`reviewer.py`) | sonnet | yes | Asks a `SubAgent` to add new test files | no |
| Integrator, Auditor, Librarian, Implementer ([specialists.py](../python/src/lha/agents/specialists.py)) | sonnet | integrator/implementer yes | Thin `SubAgent` wrappers with the role prompt. No merge, audit or lease logic beyond the prompt | no |
| Judge ([judge.py](../python/src/lha/agents/judge.py)) | caller's model | no tools | Strict JSON `{pass, score, rationale}`. Anything malformed is a fail | no (used by `evals/harness.py` and the evolution gate) |
| Evolver + gate ([evolver.py](../python/src/lha/agents/evolver.py), [evolution.py](../python/src/lha/agents/evolution.py)) | caller's model | no tools | Proposes a new system prompt from failure traces. Promotes only if the judged pass rate on the held-out cases beats baseline by more than `min_improvement`. Refuses an empty case set | no (library) |
| `ClaudeSdkLead` ([claude_sdk_lead.py](../python/src/lha/agents/claude_sdk_lead.py)) | configurable | SDK tools | Runs a prompt through `claude_agent_sdk.query` and folds the streamed messages into text, session id and cost | no |

`SubAgent` ([subagent.py](../python/src/lha/agents/subagent.py)) is the shared loop for
non-Lead roles. It shows the role only the tools that pass both the dispatcher and the role
policy, refuses any other call without dispatching it, and supports native tool calls as well
as the text action protocol.

`ClaudeSdkLead` uses the Agent SDK's own tools. It does not go through `AllowListDispatcher`,
the sandbox or the `CostMeter`. It is not wired into any run path.

## File ownership

[ownership.py](../python/src/lha/coordination/ownership.py) implements single-writer-per-file:

- `permits(writer, path)`:
  - shared files: only `lead`;
  - assigned files: only their owner;
  - unassigned files: only `lead`;
  - absolute or root-escaping paths: never.
- Shared files are matched case-insensitively at any depth:
  - `pyproject.toml`, `setup.py`, `setup.cfg`, `uv.lock`, `poetry.lock`;
  - `__init__.py`, `conftest.py`, `settings.py`;
  - `package.json`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`;
  - `go.mod`, `go.sum`, `Cargo.toml`, `Cargo.lock`;
  - any `*.lock`, any `requirements*.txt`, anything under `migrations/`.

  `assign()` to anyone but the lead raises. The cases are pinned in
  [`spec/coordination/shared_paths.json`](../spec/coordination/shared_paths.json).
- Map keys are normalized (`\` becomes `/`, `.` and `..` resolved lexically, leading dots kept)
  and case-folded, so `Models.py` and `models.py` have one owner.
- No silent reassignment: `assign()` of a file owned by another writer raises
  `OwnershipConflictError`. Only `reassign()` transfers ownership, and it returns the previous
  owner.
- `violations(writer, paths)` lists writes to paths the writer does not own. `LeaseRequest` is
  the typed request for a foreign file.

Nothing in the run paths builds a `FileOwnershipMap` or calls `violations()` today. There is one
writer (the Lead), so single-writer holds trivially. The Planner's prompt mentions ownership, but
`parse_checklist` does not read any ownership output.

## Tickets and blackboard

- [ticket.py](../python/src/lha/coordination/ticket.py): `TaskContract` holds the objective, role,
  output schema, boundaries, `write_set`, token and tool budgets, and acceptance check names.
  `Ticket` has the lifecycle `created → in_progress → awaiting_verify → awaiting_merge → done`.
  Verify and merge can send a ticket back to `in_progress`, and any non-terminal state can go to
  `failed`. `done` and `failed` are terminal. An illegal edge raises `IllegalTransitionError`.
  Entering `in_progress` increments `attempts`.
- [blackboard.py](../python/src/lha/coordination/blackboard.py): entries posted during a round go
  to a response board. `commit_round()` promotes them to the main board, so agents in the same
  round do not see each other's output.

Both are in-memory library components. `durable/ledgers.py` uses `Ticket`, but no workflow or
CLI path creates tickets or uses the blackboard.

## Hash-chained decision log

[decision_log.py](../python/src/lha/coordination/decision_log.py) is an append-only JSONL file of
`DecisionRecord`s (`decision`, `rationale`, `alternatives_rejected`, `affected`, `cycle_id`):

- **Envelope.** Each line is `{"prev": <hash>, "hash": <hash>, "record": {...}}`. `hash` is
  SHA-256 of `prev + "\n" + canonical(record)`. Canonical JSON uses sorted keys, `(",", ":")`
  separators and `ensure_ascii=False`. The genesis `prev` is 64 zeros. The bytes and hashes are
  pinned in [`spec/coordination/decision_chain.json`](../spec/coordination/decision_chain.json).
- **Durability.** `append()` writes, flushes and fsyncs before returning.
- **Torn tail.** A final line without a trailing newline, or an unparseable final line, is treated
  as a crash mid-write. `read()` skips and logs it. The next `append()` truncates the file to the
  last intact byte and continues the chain. Corruption before the final line raises
  `DecisionLogCorruptError`.
- **Verify.** `verify()` recomputes the chain over the intact prefix. It fails on an unreadable
  line, an unchained (legacy) line, a `prev` mismatch or a hash mismatch, and reports whether a
  torn tail was present. Lines are split on `\n` only, because `ensure_ascii=False` can leave
  U+2028/U+2029 inside strings.

The mission anchor's `.lha/decisions.ndjson` is a separate file with a different format: bare
`DecisionRecord` lines, not chained. `GitMissionAnchor` reads the last five into the situation
snapshot. No run path populates `Checkpoint.decisions` today, so that file stays empty, and no
run path uses `DecisionLog`. See [Mission anchor](06-mission-anchor.md).

## Not implemented

These are described in role prompts or docstrings but have no implementation:

- parallel implementers in separate worktrees;
- ownership enforcement at the git layer;
- the Integrator as sole writer to `main`;
- lease handling.

Related: [Architecture](05-architecture.md), [CLI](17-cli.md).
