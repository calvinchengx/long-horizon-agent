# Multi-agent organization

LHA's organization is asymmetric. By default one Lead Engineer makes all coupled code writes on a
single thread. Other agents run for work that parallelizes without write conflicts: reading the
codebase, reviewing work in a fresh context, and implementing checklist items whose files the
Planner has proven disjoint. Those parallel implementers never write the mission branch
themselves; a deterministic integrator merges their verified branches. `lha orchestrate` runs
this organization locally; a durable mission (`lha mission-start`) runs it on Temporal when it
opts in with `--research`, `--review` or `--max-parallel`. Code:
[`python/src/lha/agents/`](../python/src/lha/agents/),
[`python/src/lha/coordination/`](../python/src/lha/coordination/),
[`python/src/lha/durable/org_round.py`](../python/src/lha/durable/org_round.py).

The Go implementation runs the same organization for `lha orchestrate`
([`go/internal/agents/org/`](../go/internal/agents/org/),
[`go/internal/coordination/`](../go/internal/coordination/)): the same prompts (pinned in
[`spec/agent/org.json`](../spec/agent/org.json)), ownership refusals, commit messages, event
kinds and payloads, so a mission started by one implementation can be resumed by the other. Its
durable organization rounds ([`go/internal/durable/org_round.go`](../go/internal/durable/org_round.go),
[`org_activities.go`](../go/internal/durable/org_activities.go)) run the same wave functions
(`ImplementInWorktree`, `IntegrateRun`, `BranchIntegrator`, `Reviewer.Review`,
`coordination.LeaseBroker`) as Temporal activities, with Python's activity names, payloads,
events and gate-log lines: a Go-served and a Python-served org mission on the same scripted
inputs leave the same commits and anchor.

## Why asymmetric

Parallel writers to the same code make design decisions that conflict, and merging them costs
more than the parallelism saves. Reads have no side effects, so they fan out safely. A reviewer
that does not share the author's context can catch errors the author cannot. Parallel writes are
allowed only when each writer owns a disjoint set of files, and that is enforced rather than
trusted. The code enforces this split:

- `research_fanout` refuses a role that can mutate.
- The reviewer's dispatcher is read-only.
- Only the Lead and the implementers get mutating dispatchers. Implementers always write through
  an `OwnershipGuard` in their own git worktree; so does the Lead whenever the mission has an
  ownership map (`orchestrate`, and a durable mission planned with `--max-parallel`).
- Only the integrator changes the mission branch with another agent's work, and only after
  verification.

## What runs, by command

| Command | Agents involved |
|---|---|
| `lha run-local` | Lead (checklist given with `--item` or `--checklist`), Replanner when an item blocks |
| `lha mission` | Planner (skipped with `--checklist`), then Lead, Replanner when an item blocks |
| `lha orchestrate` | Planner (checklist + file ownership; skipped with `--checklist`, which leaves no ownership map), then rounds. A serial round runs 2 Researchers, then the Lead on one item. A parallel wave runs 2 Researchers per item, then one Implementer per item in its own worktree, then the Integrator. Reflection runs on failure, the Reviewer on success, and the Replanner when an item blocks |
| `lha mission-start` + `lha worker` | Planner (at start, in the CLI process; skipped with `--checklist`), then Lead cycles inside `MissionWorkflow`, Replanner when an item blocks. Opt-in, per mission: `--research N` Researchers per item before each round (child workflows), `--review` the Reviewer after every verified item, `--max-parallel N` parallel waves of Implementers (one activity each, in its own worktree) and the Integrator (one activity per branch); see [Durable execution](08-durable-execution.md#the-multi-agent-organization-opt-in) |

All run paths build the Lead the same way ([agent/assembly.py](../python/src/lha/agent/assembly.py)),
so the Replanner runs, and the `record_decision` tool is available
([mission anchor](06-mission-anchor.md#decisionsndjson)), wherever the Lead runs. Both
organization paths build a wave from the same code
([waves.py](../python/src/lha/agents/waves.py): the batch, the ticket, the implementer in its
worktree, the integration checkpoint), so a wave means the same thing on either path. What only
`orchestrate` has: reflection after a failure and the blackboard. What only the durable path
has: every step is a journaled activity
(crash-safe and retried), and researcher and review spend counts against the mission's
spend journal.

### `lha orchestrate` wiring

[cli/main.py](../python/src/lha/cli/main.py) builds one `CostMeter`, calls
`Planner.plan_mission()` (checklist plus `FileOwnershipMap`), and calls
`Orchestrator.run_mission(..., ownership=...)`
([orchestrator.py](../python/src/lha/agents/orchestrator.py)) with the same meter. The
orchestrator initializes the anchor with the ownership map (`.lha/ownership.json`), or, with
`--resume`, keeps the existing one (see [Resuming](#resuming-lha-orchestrate)), and removes
implementer worktrees and `lha/implementer-*` branches left behind by an interrupted run. Each
round then:

1. Calls `authorize_next`, reads the anchor snapshot, and stops if the mission is complete or
   deadlocked.
2. Reads the committed ownership map and releases the files of items that are already `done`.
   The release is staged for the next checkpoint and recorded as an `ownership_released` trace
   event.
3. Picks the round kind. If at least two actionable items (not blocked, dependencies done) each
   own a non-empty write-set, it runs a **parallel wave** over up to
   `LHA_MAX_PARALLEL_IMPLEMENTERS` of them (default 3; below 2 disables waves). Otherwise it
   runs a **serial round** on the next actionable item.
4. Promotes the blackboard's response board into the main board (`commit_round`).

Both round kinds start with `research_fanout`: two queries per item ("Find context relevant
to…", "Find existing files/code related to…"). Each query goes to a separate `SubAgent`
concurrently (`asyncio.gather`), using a read-only dispatcher over the run's tools
(`build_run_dispatcher(..., allow_mutating=False)`). When the web allow-list
(`LHA_WEB_ALLOW_HOSTS` plus any `--allow-host`) is non-empty, that dispatcher includes the web
tools (`fetch_url`, and `web_search` when configured), and the researcher role, which sets
`allow_egress`, sees them. Failed researchers come back with `error` set. Auth errors (401/403),
`BudgetExceeded` and cancellation are raised.

**Serial round.** The Lead is built by `build_lead_loop`
([agent/assembly.py](../python/src/lha/agent/assembly.py)), as in every run path: the verifier
that runs trusted checks outside the sandbox, item witnesses, the replanner, the human gate and
`record_decision`. The one difference is its dispatcher: `lead_dispatcher` wrapped in an
`OwnershipGuard` with writer identities `lead` and the item's own implementer id. So
`write_file` may touch unassigned files, shared files and this item's own files, but not files
leased to another open item. The Lead's anchor text is built from the immutable mission spec,
any reflection for the item, the research briefs and the newest blackboard entries. One Lead
`AgentLoop` cycle runs. Afterwards the files changed
in the cycle's commit are checked against the map. Violations, which can only come from the
shell because the guard refuses `write_file`, are recorded as an `ownership_violation` trace
event. This check only records: no other writer is active in a serial round, so such a write
cannot conflict. On failure (item neither blocked nor split), `reflect_on_failure` writes a
post-mortem for
the next attempt. On success the Reviewer reviews `git diff base..head` (excluding `.lha`,
capped at 20,000 chars, then 8,000 inside the reviewer). A blocking verdict reopens the item:
status `todo`, `verified_by` cleared, the review notes attached, and a checkpoint committed.

**Parallel wave.** For each item, concurrently:

1. A `Ticket` is created whose `TaskContract` holds the objective, role `implementer`, the
   write-set, the boundaries, a tool budget (`LHA_MAX_TURNS_PER_CYCLE`) and the gating check
   names as acceptance.
2. A worktree is added at `.git/lha-worktrees/<branch>` on a new branch
   `lha/implementer-<item>/<cycle_id>`, starting from the mission branch's `HEAD`. A sandbox
   session is opened on that worktree the same way as the Lead's (`open_lead_sandbox`: the
   configured image and sandbox egress allow-list). The worktree's `.git` is a `gitdir:` file;
   the Docker sandbox mounts it read-only, and before the branch is committed the host checks it
   still leads to the mission repository ([safety model](09-safety-model.md#7-host-side-git)).
3. An `Implementer` ([specialists.py](../python/src/lha/agents/specialists.py)) runs. Its
   dispatcher has four layers: `record_decision` (into the implementer's own buffer), then
   `request_lease` ([Leases](#leases)), then an `OwnershipGuard` (writer: its implementer id),
   then the Lead's dispatcher (`lead_dispatcher`: the same tools and the same human gate). The
   implementer role does not set `allow_egress`, so
   the implementer is never shown the web tools, even when the allow-list is set. The prompt
   contains the contract, the mission spec, the item's last failure and reflection, the
   recorded decisions, the research briefs and the blackboard.
4. The worktree is verified the way a Lead cycle would verify the item. The mission checks and
   the item's witnesses run on the Lead's verifier (trusted checks outside the sandbox, and
   the [flaky-check quarantine](07-verification.md#flaky-check-quarantine)), and an invalid
   witness counts as a failing check. The harness-integrity check (including
   `LHA_HARNESS_PATHS`) is added unless the item sets `allow_harness_edits`. The work is
   committed on the branch, with `.lha/` restored first so harness files never travel. The files
   the branch changed (`git diff --name-only base..head`, excluding `.lha`) are checked against
   the map (as it is after any lease the implementer was granted).

Then the integrator ([integrator.py](../python/src/lha/agents/integrator.py),
`BranchIntegrator`) takes the items one at a time, in checklist order, and commits one
checkpoint per item. A branch is merged only if it verified in its worktree, changed no file it
does not own, merges without conflict (`git merge --no-ff --no-commit`), and passes the mission
checks and the item's witnesses again on the merged mission workspace. If any step fails, the
merge is aborted (`git merge --abort`), the mission branch is left as it was, and the item
records a failed attempt with the reason in `last_failure`. After 3 consecutive failures the item
is blocked. If replanning is on, the replanner then splits the item within the same bounds as the
Lead's (`LHA_MAX_REPLANS`, `LHA_MAX_SPLIT_DEPTH`). The children have no write-set, so the Lead
works them serially. Reflection follows as in a serial round. On success the item is recorded `done`, the implementer's
lease is released, and the implementer's decisions are chained into the decision log. The anchor
checkpoint is committed while the merge is still in progress, so the merge commit is the
checkpoint (`lha: complete <id> (<description>) [merged <branch>]`). The Reviewer then reviews
that commit's diff and can reopen the item. A reopened item's files have been released, so it is
redone serially by the Lead. Each checkpoint carries a `cycle` event (with `writer` and `branch`)
and a `ticket` event. All worktrees and branches of the wave are removed at the end of the wave,
whatever the outcome.

The command-line knobs are `--check`, `--no-default-checks`, `--sandbox`, `--unsafe-local`,
`--reference`, `--approve-interactive`, `--allow-host`, `--checklist` (import a checklist instead
of planning; no ownership map) and `--resume` (continue the mission anchored in `--workdir`; not
with `--checklist`); the rest comes from `LHA_*` settings. Parallel waves add no CLI option. They are tuned by
one setting, `LHA_MAX_PARALLEL_IMPLEMENTERS`. With a non-empty web allow-list the Lead and the
Researchers get the web tools; the Reviewer and the implementers are not shown them because
their roles do not set `allow_egress`. A run with web tools and private data
(`LHA_PRIVATE_DATA=true` or `--sandbox local`) is refused before planning (Rule of Two).

### Resuming `lha orchestrate`

Resuming is explicit: `lha orchestrate --resume --workdir DIR` continues the mission anchored in
`DIR`, and without `--resume` `orchestrate` refuses a workdir that already holds a mission
(`.lha/mission.json` committed) rather than replacing its checklist. `--resume` on a workdir with
no anchor is refused too. A resumed run
([`run_mission(resume=True)`](../python/src/lha/agents/orchestrator.py)):

- does not plan and does not re-initialize: the committed mission spec, checklist, ownership map
  (with its leases), decision chain and events are used as they are (the decision chain is
  verified first). `--task`, `--title` and `--reference` are ignored;
- discards what the interrupted run left uncommitted: a half-finished merge is aborted, tracked
  edits and untracked (not ignored) files are removed (`git_ops.discard_changes`), and leftover
  implementer worktrees and branches are pruned. Nothing that was committed is lost;
- keeps the mission id (from the newest committed `orchestrate` event), so `lha missions` shows
  one mission; its ledger keys get a `run<N>` prefix so the new run's spend is recorded;
- rebuilds the blackboard's main board from the committed `blackboard` events and the latest
  reflection of each open item from the committed `reflection` events;
- continues the cycle ids after the highest committed `c<N>`.

Every run records an `orchestrate` event (`mission_id`, `resumed`, `run`). Board posts and
reflections are recorded as events when they are made and committed with the next checkpoint, so
a post made after the last checkpoint of an interrupted run is lost with it. The budget ceiling
and `LHA_MAX_CYCLES` apply to each invocation.

## Roles

`ROLES` in [roles.py](../python/src/lha/agents/roles.py) is the org chart as data: model tier,
system prompt, `allow_mutating`, `allow_egress` and `max_turns` (8, the turn budget of a
`SubAgent` role such as the researcher or reviewer; the Lead and implementers use
`LHA_MAX_TURNS_PER_CYCLE`, default 20). With the `claude` backend,
`model_for_role()` ([router.py](../python/src/lha/agents/router.py)) maps opus to
`claude-opus-4-8`, sonnet to `claude-sonnet-4-6` and haiku to `claude-haiku-4-5-20251001`. Every
other backend uses the single configured model for all roles.

| Role / module | Tier | Mutating | What the code does | Used by a CLI path |
|---|---|---|---|---|
| Planner ([planner.py](../python/src/lha/agents/planner.py)) | opus | no | One model call. It parses a JSON array into `ChecklistItem`s with ids `01`, `02`, … and keeps only dependencies on earlier steps (dropped ones are noted). `plan_mission` also reads each step's `files` and assigns ownership (see below). If nothing parses, it creates a single serial item from the description | `mission`, `orchestrate`, `mission-start` (the ownership map is used by `orchestrate` and by `mission-start --max-parallel 2` or more) |
| Lead (`AgentLoop`, [agent/loop.py](../python/src/lha/agent/loop.py)) | opus | yes | Works one item per cycle: tools (plus `record_decision`), gating checks and witnesses, checkpoint commit | all |
| Replanner ([replanner.py](../python/src/lha/agents/replanner.py)) | lead's model | no tools | One model call when an item has just become `blocked`: given the mission, the item, its witnesses and the latest failure report (last 3,000 chars), returns a JSON array of 2 to 6 smaller steps. `Checklist.split` replaces the item with them; fewer than 2 usable steps means no split. Bounded by `LHA_MAX_REPLANS` and `LHA_MAX_SPLIT_DEPTH`. In `orchestrate` it also splits items blocked in a parallel wave | all (unless `LHA_MAX_REPLANS=0`) |
| Researcher ([team.py](../python/src/lha/agents/team.py)) | haiku | no | Read-only `SubAgent` that returns a brief (capped at 8,000 chars); sees the web tools when the allow-list is set | `orchestrate`, `mission-start --research N` (one `SubAgentWorkflow` child each) |
| Implementer ([specialists.py](../python/src/lha/agents/specialists.py)) | sonnet | yes | `SubAgent` with the implementer prompt, run in its own worktree behind an `OwnershipGuard`, with `request_lease` | `orchestrate` (parallel waves), `mission-start --max-parallel N` (one `run_implementer` activity each) |
| Integrator (`BranchIntegrator`, [integrator.py](../python/src/lha/agents/integrator.py)) | none (deterministic) | merges only | Verified, owned, conflict-free, re-verified merge of an implementer branch | `orchestrate` (parallel waves), `mission-start --max-parallel N` (`integrate_branch`) |
| Reviewer ([reviewer.py](../python/src/lha/agents/reviewer.py)) | opus | no | Fresh-context review that returns JSON `verdict` / `blocking_issues` / `advisory`. An unparseable reply counts as blocking, and "approve" with issues counts as "block" | `orchestrate`, `mission-start --review` (`review_cycle`) |
| Reflection ([reflection.py](../python/src/lha/agents/reflection.py)) | lead's model | no tools | Short post-mortem (at most 2,000 chars) prepended to the next attempt | `orchestrate` |

`SubAgent` ([subagent.py](../python/src/lha/agents/subagent.py)) is the shared loop for
non-Lead roles. It shows the role only the tools that pass both the dispatcher and the role
policy, refuses any other call without dispatching it, and supports native tool calls as well
as the text action protocol. An implementer's turn budget is `LHA_MAX_TURNS_PER_CYCLE`. It gets
no "verification failed, try again" turn inside the wave. A failed attempt is retried in a later
round with the failure report and a reflection.

The Auditor, Librarian, Tester and model-backed Integrator runners, the prompt evolver with its
judge and eval harness, and the Claude Agent SDK lead were removed: none had a caller. Their
jobs are done by deterministic code (the verifier and harness integrity audit the lead's claims;
`BranchIntegrator` integrates), by memory consolidation (the `librarian` label in the cost
ledger), and by the `claude_code` lead engine ([13-models.md](13-models.md)).

## File ownership

[ownership.py](../python/src/lha/coordination/ownership.py) implements single-writer-per-file:

- `permits(writer, path)`:
  - shared files: only `lead`;
  - assigned files: only their owner;
  - unassigned files: only `lead`;
  - absolute or root-escaping paths: never.

  `permits_any` and `violations_any` apply the same rule to an agent acting under several
  identities (the serial Lead: `lead` plus the item's implementer id).
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
  owner. `release(writer)` explicitly returns all of a writer's files to the lead.
- `violations(writer, paths)` lists writes to paths the writer does not own. `LeaseRequest` is
  the typed request for a foreign file; see [Leases](#leases).

**Assignment** (`assign_ownership` in [planner.py](../python/src/lha/agents/planner.py)). The
Planner asks for a `files` list per step. Each item's files go to `implementer-<id>`, all or
nothing. The item stays serial (no write-set, worked by the Lead), with the reason in `notes`,
when:

- it declares no files;
- it declares an invalid path, or one under `.lha/` or `.git/`;
- it touches a shared file;
- it overlaps files an earlier item declared. In this case it also gains a `depends_on` edge to
  that earlier item, so the two never run at once. Only earlier items are referenced, so no cycle
  can appear.

**Enforcement** ([enforcement.py](../python/src/lha/coordination/enforcement.py)):

- `OwnershipGuard` wraps a `ToolDispatcher`. A mutating tool with `path_args` (`write_file`)
  aimed at a path its writer identities may not write is refused before it runs. The message
  names the owner (or says the file is shared or outside the write-set) and tells the agent to
  call `request_lease` (an implementer) or to stop and say so in its summary (the Lead, which
  has no lease tool). The guard is a wrapper, so `AllowListDispatcher` is unchanged.
- `run_command` names no paths, so the guard cannot check it. Shell writes are caught at the git
  layer: `changed_paths` lists what a branch or cycle changed, and `violations` checks it. For
  implementers a violation blocks the merge. For the serial Lead it is recorded as a trace event.

**Persistence.** The map lives in `.lha/ownership.json` and is read from `HEAD` at every round.
A finished item's lease is released explicitly. After a parallel merge the release is committed
in the same checkpoint. After a serial cycle it is staged at the start of the next round and
committed with that round's checkpoint, so the committed map can still list a just-finished
serial item's files (they are released in memory before any use).

## Leases

An implementer that finds it needs a file outside its write-set calls the `request_lease` tool
(`path`, `reason`; [execution/tools/leases.py](../python/src/lha/execution/tools/leases.py)).
The `LeaseBroker` ([coordination/leases.py](../python/src/lha/coordination/leases.py)) decides
it against the COMMITTED checklist and ownership map (`decide_lease`):

| The file is | Decision |
|---|---|
| unassigned (the lead's space; the lead never writes during a wave) | granted |
| already the requester's | granted (no change) |
| owned by a finished writer (its item is `done` or `split`) | granted |
| owned by another writer whose item is still open | refused |
| a shared file (build manifest, lockfile, `__init__.py`, ...) | refused |
| under `.lha/` or `.git/`, absolute, or escaping the repository | refused |
| requested by the lead | refused (the lead needs no lease) |

Each decision is committed at once, by itself, as a `lease` event (`writer`, `path`, `reason`,
`granted`, `previous_owner`, `why`); a grant also rewrites `.lha/ownership.json` in that commit
(`reassign`). The commit holds only `.lha/` files (`GitMissionAnchor.commit_anchor_update`), so
nothing else in the checkout is committed or discarded. Decisions are serialized by an
in-process lock and the checkout's `flock` (`.git/lha-cycle.lock`), so two implementers are never
both granted one file. A grant is applied to the implementer's live map at once, so its
`OwnershipGuard` lets it write the file and its git-layer check accepts the change; the
integrator's check reads the committed map, which has the lease. The tool's reply tells the
agent the decision and why; after a refusal it must not write the file and says what still needs
it in its summary. A leased file is released with the writer's other files when its item is done.
In `orchestrate` every decision is also a `lease` trace event, and the ticket event lists the
run's leases. `ownership.json` keeps its format (`{"owners": {...}}`), and the Go `LeaseBroker` takes the
same `flock`, so a Python and a Go process never both grant one file.

## Tickets and blackboard

- [ticket.py](../python/src/lha/coordination/ticket.py): `TaskContract` holds the objective, role,
  output schema, boundaries, `write_set`, token and tool budgets, and acceptance check names.
  `Ticket` has the lifecycle `created → in_progress → awaiting_verify → awaiting_merge → done`.
  Verify and merge can send a ticket back to `in_progress`, and any non-terminal state can go to
  `failed`. `done` and `failed` are terminal. An illegal edge raises `IllegalTransitionError`.
  Entering `in_progress` increments `attempts`. In a parallel wave each implementer gets one
  ticket per attempt, and its contract is what the implementer's prompt is built from. The
  ticket moves `created → in_progress` (worktree ready) `→ awaiting_verify` (implementer done)
  `→ awaiting_merge` (verified) `→ done` (merged), or to `failed` at the step that failed. The
  final status and the full history are committed as a `ticket` event in the item's checkpoint.
  A retry is a new ticket. A durable wave keeps the same lifecycle: `run_implementer` advances
  the ticket to `awaiting_verify` and returns it, and `integrate_branch` finishes it and commits
  the `ticket` event.
- [blackboard.py](../python/src/lha/coordination/blackboard.py): entries posted during a round go
  to a response board. `commit_round()` promotes them to the main board, so agents in the same
  round do not see each other's output. In `orchestrate`, research briefs, implementer
  summaries and blocking review notes are posted to the response board. The orchestrator
  promotes them after every round, and the newest 6 main-board entries (each capped at 1,500
  chars) go into the context of later rounds' Lead and implementers. The board is in memory; each
  post is also recorded as a `blackboard` event (capped at 1,500 chars) so `--resume` can rebuild
  the main board. The durable organization has no blackboard.

## Hash-chained decision log

[decision_log.py](../python/src/lha/coordination/decision_log.py) defines the format of the
mission anchor's `.lha/decisions.ndjson`. It is written by `GitMissionAnchor` and verified on
every read ([Mission anchor](06-mission-anchor.md#decisionsndjson)).

- **Envelope.** Each line is `{"prev": <hash>, "hash": <hash>, "record": {...}}`. `hash` is
  SHA-256 of `prev + "\n" + canonical(record)`. Canonical JSON uses sorted keys, `(",", ":")`
  separators and `ensure_ascii=False`. The genesis `prev` is 64 zeros. A leading prefix of bare
  (pre-chain) `DecisionRecord` lines is folded into the running hash and sealed by the first
  chained line. The bytes and hashes, including a legacy-prefix log and its tamper cases, are
  pinned in
  [`spec/coordination/decision_chain.json`](../spec/coordination/decision_chain.json).
- **Functions.** `encode_link` builds a line. `parse_chain` reads a log. `verify_chain`
  recomputes the chain and fails on an unreadable line, a bare line after the chain began, a
  `prev` mismatch or a hash mismatch. Lines are split on `\n` only, because
  `ensure_ascii=False` can leave U+2028/U+2029 inside strings.
- **`DecisionLog`** is the same format as a standalone file. `append()` writes, flushes and
  fsyncs before returning. A final line without a trailing newline, or an unparseable final
  line, is treated as a crash mid-write: `read()` skips and logs it, and the next `append()`
  truncates it. Corruption before the final line raises `DecisionLogCorruptError`. The anchor
  does not use this crash tolerance: it writes whole files, so an incomplete final line in a
  committed log counts as tampering.
- **Recording.** The `record_decision` tool
  ([execution/tools/decisions.py](../python/src/lha/execution/tools/decisions.py)) and
  `lha decisions [--verify]` ([CLI](17-cli.md#lha-decisions)).

## Not implemented

These are described in role prompts or docstrings, or would be needed for the organization to
run durably, but have no implementation:

- a model-backed integrator that resolves merge conflicts or rebases outstanding work (the
  integrator refuses a conflicting branch instead);
- reflection, the blackboard and tiered memory for the implementers in the durable organization
  (only `orchestrate` has reflection and the blackboard; tiered memory reaches only the Lead);
- a lease that is released before its writer's item is done, or one that waits for a busy file
  (a contended request is refused, not queued);
- Auditor, Librarian and Tester roles (their runners were removed rather than kept as uncalled
  code).

Related: [Architecture](05-architecture.md), [CLI](17-cli.md).
