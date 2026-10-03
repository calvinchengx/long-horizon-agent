# Building a large project

A mission the size of a real product (fabric-emulator is about 225k lines of Go and Python,
built over roughly 1,000 commits) needs more than a task description and a unit-test gate. This
guide shows how to set LHA up for that kind of work. Every step uses a feature that is implemented
and tested. The end-to-end test
[`test_large_mission_e2e.py`](../python/tests/integration/test_large_mission_e2e.py) runs the first
seven rows of the table below and an approval together in one local mission on real Docker (the
approval goes to a simulated approver, not the durable gate); the other rows, including the
durable gates, the escalation ladder, the webhook and the mission store, have their own tests.

| What a large project needs | LHA feature | Where it is set |
|---|---|---|
| A plan you control, not one the model invents | Checklist import | `--checklist roadmap.md` |
| "Done" means the feature works, not that unit tests pass | Per-item witnesses | `(witness: ...)` in the roadmap |
| Tests that need a Docker daemon or the full stack | Trusted checks, run outside the sandbox | `LHA_TRUSTED_CHECKS` |
| The agent cannot rewrite what a gate runs | Protected paths | `LHA_HARNESS_PATHS` |
| A real toolchain and package downloads, nothing else | Sandbox image + egress allow-list | `LHA_SANDBOX_IMAGE`, `LHA_SANDBOX_EGRESS` |
| Knowledge of an API the model has not memorized | Vendored references | `lha vendor`, `--reference` |
| Coarse items that turn out too big | Replanning | `LHA_MAX_REPLANS`, `LHA_MAX_SPLIT_DEPTH` |
| Stalled items noticed before the failure limit (optional) | System One triage | `LHA_SYSTEM_ONE_BACKEND` |
| A map of the code before the first turn (optional) | ripwire code map | `LHA_CODE_MAP=ripwire` |
| Answers about the code only when the model asks (optional) | ripwire `code_query` tool | `LHA_CODE_QUERY=true` |
| Tests that would notice a bug, not just run the code (optional) | Mutation gate | `LHA_MUTATION_CHECK` |
| Pushes and releases stay a human decision | Durable approvals | `lha mission-approve` |
| Someone notices when a gate is waiting | Escalation ladder and webhook | `LHA_GATE_ESCALATION_SECONDS`, `LHA_GATE_WEBHOOK_URL` |
| Spend and status you can query after the fact | Mission store | `lha missions`, `lha costs` |

## 1. Write the roadmap as a checklist

fabric-emulator was driven by a phased roadmap. LHA imports the same shape: each `## ` section is
a phase, each `- [ ]` line is an item, and every item in a phase depends on every item of the
phase before it. Items within a phase are independent, so one blocked item does not stall its
neighbours.

```markdown
# Fabric emulator

A clean-room emulator of Microsoft Fabric that composes with entra-emulator.

## P0 — the spine

- [ ] Validate Entra bearer tokens against the issuer's JWKS (witness: go:TestTokenAcceptance)
- [ ] Workspaces CRUD with RBAC enforcement (witness: go:TestWorkspaceRBAC)

## P1 — CI/CD

- [ ] Item definitions round-trip verbatim (witness: go:TestDefinitionRoundTrip, trusted:fabric-cicd)
```

`- [x]` lines count as already done and are skipped; JSON checklists
(`{"title", "description", "references", "items": [...]}`) are accepted too. See
[the mission anchor](06-mission-anchor.md) for the fields an item carries.

## 2. Give every item a witness

Mission checks (`--check`) gate every item. A witness gates one item, on top of them, and the
item is done only when all of its witnesses pass. This is fabric-emulator's `witnesses.json`
idea moved into the checklist: every claim names a test that exists and ran.

| Witness | Runs | Passes when |
|---|---|---|
| `go:TestName` or `go:TestName@./pkg/...` | in the sandbox | `go test` exits 0 **and** printed `--- PASS: TestName` (a missing test fails, unlike a plain `go test -run`) |
| `pytest:path/test_x.py::test_y` | in the sandbox | the node was collected and passed (pytest exits 5 when nothing matched) |
| `cmd:<shell>` | in the sandbox | the command exits 0 |
| `trusted:<name>` (alias `ci:<name>`) | outside the sandbox | the operator's command for `<name>` exits 0 |

An unknown or malformed witness is a failing check, never a skipped one. The witness list is shown
to the agent in its prompt, so it knows what "done" means for the item.

## 3. Run what needs Docker outside the sandbox

fabric-emulator's 69 e2e suites need the whole compose stack, which means a Docker socket, and a
Docker socket inside the agent's sandbox would hand it root on the host. Trusted checks solve this:
the operator defines them, and items refer to them by name.

```bash
export LHA_TRUSTED_CHECKS='{"fabric-cicd": ["make", "e2e-fabric-cicd"],
                            "warehouse-tds": ["./scripts/ci-e2e.sh", "warehouse-tds"]}'
export LHA_HARNESS_PATHS='Makefile,e2e/**,scripts/**,.github/**'
```

When an item with `trusted:fabric-cicd` is verified, LHA snapshots the agent's uncommitted work as a
commit object (without touching `HEAD`), checks it out in a throwaway worktree, runs the command
there with `LHA_CHECK_COMMIT`, `LHA_CHECK_WORKTREE` and `LHA_CHECK_NAME` set, and removes the
worktree afterwards. The command can run the suite directly or hand off to CI.

`LHA_HARNESS_PATHS` adds files to [harness integrity](07-verification.md): if the agent edits the
`Makefile` target a trusted check runs, the check fails and the edit is reverted.

**Trade-off, stated plainly:** a trusted check runs agent-written code with the runner's
privileges. Run it on a dedicated machine or VM, or make the command a small script that submits
the commit to CI and waits for the verdict. Only operator-defined names can be run this way; a
model can never introduce a new trusted command.

## 4. Build the sandbox image and allow the package registries

The default sandbox image has Python and uv. For a Go + Python + TypeScript project, build the
reference image in [`sandbox/`](../sandbox/) (Go, uv, Node 22 and pnpm) and let the sandbox reach
only the registries it needs:

```bash
docker build -t lha-sandbox:dev sandbox
export LHA_SANDBOX_IMAGE=lha-sandbox:dev
export LHA_SANDBOX_EGRESS='proxy.golang.org,sum.golang.org,storage.googleapis.com,pypi.org,files.pythonhosted.org,registry.npmjs.org'
```

`storage.googleapis.com` is needed because `proxy.golang.org` redirects module downloads there.
Tests that fetch a tool at run time need its host too. Temporal's Python test environment
(`WorkflowEnvironment.start_time_skipping()`) downloads its test server from
`temporal.download`, which is not a package registry, so it goes in the extra-hosts list:

```bash
export LHA_SANDBOX_EGRESS_EXTRA_HOSTS=temporal.download
```

Without it, every test that starts that environment fails in the sandbox, and a model that runs
the whole suite burns turns on failures it cannot fix. With it, LHA's own Python suite passes in
the sandbox (1,372 passed; the 10 skipped need the Docker SDK or E2B, which the image does not
install).
A real project also needs a bigger sandbox than the defaults (2 GB of memory, a 1 GB `/tmp` for the
toolchain caches). For fabric-emulator, `go build` was killed at 2 GB and the module cache did not
fit in 1 GB:

```bash
export LHA_SANDBOX_MEMORY=8g LHA_SANDBOX_TMP_SIZE=4g LHA_SANDBOX_CPUS=6
```

With an allow-list set, the sandbox container sits on an internal Docker network whose only exit is
an LHA egress proxy. The proxy allows exactly those hosts (subdomains only for entries written with
a leading dot), refuses IP literals and anything that resolves to a non-public address, and connects
to the address it checked. `go mod download`, `uv add` and `pnpm add` work; everything else gets a
403. With no allow-list the sandbox has no network at all. See [the safety model](09-safety-model.md).

## 5. Vendor the reference material

A clean-room emulator depends on the real service's documentation. Snapshot the pages the agent
needs into the workspace, once, and list them in the mission:

```bash
cd <workspace>
lha vendor https://learn.microsoft.com/en-us/rest/api/fabric/core/items \
           https://learn.microsoft.com/en-us/rest/api/fabric/core/workspaces --into reference/fabric
```

Each page is stored raw (HTML also gets a `.txt` rendering) with a `MANIFEST.json` of URL, SHA-256
and fetch time. Fetching follows the egress rules: only the hosts you named, public addresses only,
every redirect re-checked. Pass `--reference reference/fabric` when starting the mission and the
paths are recited to the agent every cycle. Protecting them with `LHA_HARNESS_PATHS=reference/**`
stops the agent from editing its own sources.

Alternatively, `LHA_WEB_ALLOW_HOSTS=learn.microsoft.com` gives the lead a `fetch_url` tool for live
reads of those hosts only (and `web_search`, if `LHA_WEB_SEARCH_PROVIDER` and
`LHA_WEB_SEARCH_API_KEY` are set). Fetched text is marked as untrusted data in the prompt. A run
with web tools may not also hold private data: with `LHA_PRIVATE_DATA=true` or the `local`
sandbox, LHA refuses to start it. Vendoring is the more reproducible choice.

## 6. Start the durable mission

```bash
lha mission-start --checklist roadmap.md --reference reference/fabric \
  --workdir ~/missions/fabric-emulator \
  --no-default-checks --check "go build ./..." --check "go vet ./..." \
  --deadlock-gate-hours 24 --approval-timeout-hours 24
```

`--start-in-seconds N` delays the first cycle and `--cycle-pause-seconds N` pauses between cycles;
the mission reports `SLEEPING` while it waits.

`--checklist` skips the Planner. Run `lha worker` with the same environment (the settings above,
plus a model and a budget: `LHA_BUDGET_USD_CEILING` is $10 by default, far too low for a mission of
this size). See [running on Temporal](14-running-on-temporal.md).

## Optional: one mission over several repositories

A change that spans repositories (a service and the client library it ships, a schema and its
generated bindings) runs as one mission on a **multi-repo workspace**: a git repository that holds
each member repository as a git submodule.

```bash
lha workspace init ~/missions/platform \
  --repo git@github.com:org/svc.git@main \
  --repo client=git@github.com:org/svc-client.git \
  --repo /srv/mirrors/schema            # a local path works too
lha mission-start --checklist roadmap.md --workdir ~/missions/platform \
  --no-default-checks \
  --check "sh -c 'cd svc && go test ./...'" --check "sh -c 'cd client && uv run pytest -q'"
```

What changes, and what does not ([17-cli.md](17-cli.md#lha-workspace-init)):

- **One anchor, one history.** `.lha/` and every checkpoint live in the workspace repository.
  A cycle that changed files in a member gets a commit inside that member first (same message,
  on the branch the member has checked out), and the workspace commit records the member's new
  commit; `git log` in the workspace is the mission's history, `git log` in a member is that
  member's part of it. Uncommitted member changes of a failed or crashed attempt are discarded
  like the workspace's own (the saved `refs/lha/attempts/...` snapshot holds the workspace's
  files only).
- **Checks and witnesses run from the workspace root.** Name the member in the command
  (`cmd:sh -c 'cd svc && make test'`); `pytest:` and `go:` witnesses take paths from the root
  (`pytest:client/tests/test_x.py::test_y`). Protected paths, witness scripts and harness files
  are protected inside members too.
- **The reviewer sees the code.** A review diff expands each member's own changes with the
  member's path prefixed, instead of showing two commit ids.
- **The lead is told.** Every cycle's prompt lists the members and says paths are relative to
  the workspace root.
- **A member's `.git` pointer is checked** before every harness git command, like a linked
  worktree's: it must lead into the workspace's `.git/modules/` and link back to the member;
  one re-pointed at the workspace itself or at another member is refused
  ([09-safety-model.md](09-safety-model.md#7-host-side-git)).
- **Not across members: parallel waves.** `--max-parallel 2` or more is refused on a
  workspace (implementer worktrees do not carry the members); items are worked serially, with
  researchers and the reviewer as usual.
- **Pushing is yours.** LHA pushes nothing. When the mission is done, push each member's branch
  from inside the member, then the workspace if you keep it.

`lha workspace init` on an existing workspace adds the members it lacks and keeps the rest; it
refuses a directory that already anchors a mission.

## 7. Operate it

```bash
lha mission-status <id>        # status, cycles, sleep time, the open gate and recent gate events
lha mission-approve <id> --decision approve   # or reject; retry | abort | impossible for a deadlock
lha mission-snooze <id> --seconds 3600        # sleep before the next cycle; --seconds 0 wakes it
lha mission-edit <id> --remove 07 --add "Document the API (witness: cmd:make docs)"   # change the plan between cycles
lha costs <id>                 # every metered model call and the totals
lha decisions --workdir ~/missions/fabric-emulator --verify   # the design decisions, hash-chain checked
```

- **Approvals.** A gated command (`git push`, a publish, an upload) is not refused and not run: it
  is queued. The workflow parks as `WAITING_ON_HUMAN` with the exact command in its question. An
  approval lets that exact call (matched by a fingerprint of the tool and its arguments) run once in
  a later cycle; a rejection is remembered and not asked again. While a gate is open, reminders
  go out at the offsets in `LHA_GATE_ESCALATION_SECONDS` (default 15 min, 45 min, 4 h, 12 h),
  each committed as a `gate_reminder` event and posted to `LHA_GATE_WEBHOOK_URL` if set.
  Unanswered requests are rejected after `--approval-timeout-hours`.
- **Replanning.** When an item fails verification three times in a row, the model splits it into
  2–6 smaller children (`03.1`, `03.2`, ...). The parent's witnesses move to the last child, so
  splitting can make work tractable but never makes it pass more easily. At most `LHA_MAX_REPLANS`
  splits per mission (default 20), nested at most `LHA_MAX_SPLIT_DEPTH` levels (default 2). The
  children queue behind the items already waiting, so one hard item cannot starve the others.
- **Triage (optional).** With a System One model configured (`LHA_SYSTEM_ONE_BACKEND`; a
  self-hosted Kev keeps the mission's text on your machine), an item that fails twice is triaged.
  If the model is confident it is too big, it is split then; if the environment is at fault (a
  missing tool, no network), it is blocked for a human then. On a mission with hundreds of items
  this saves the attempts that cannot succeed. Each answer is committed as a `system_one` event.
  See [System One decision models](25-system-one.md).
- **Deadlock.** If nothing is actionable, the mission waits up to `--deadlock-gate-hours` for
  `retry` (unblock and continue), `abort`, or `impossible` (a final checkpoint records the mission
  as impossible). Unanswered, it applies `--deadlock-default` (`LHA_DEADLOCK_GATE_DEFAULT`,
  default `abort`).
- **Redeploys.** A mission that runs for weeks outlives several worker deploys. Set
  `LHA_WORKER_DEPLOYMENT` and `LHA_WORKER_BUILD_ID` on the workers, and each mission stays on the
  build that started it (`LHA_WORKER_VERSIONING_BEHAVIOR`, default `pinned`);
  `LHA_WORKER_PROMOTE=true` makes a new build current for new missions. See
  [versioned deploys](14-running-on-temporal.md#versioned-deploys-worker-build-ids).
- **Disk.** Durable cycles write large payloads to the ClaimCheck object store
  (`LHA_OBJECT_STORE_ROOT`), and nothing removes them on its own. Run
  `lha objects prune --older-than-days N --dry-run`, then without `--dry-run`, with `N` longer
  than your longest mission plus the Temporal namespace's history retention.

## Optional: a code map each cycle

A cycle starts from a mostly fresh context, and the lead spends turns finding its way around the
code. With `LHA_CODE_MAP=ripwire`, the harness runs [ripwire](https://github.com/redhat-et/ripwire)
in the sandbox at the start of each cycle (`ripwire . --pack-task="<the item>"`, about 1k tokens
at the default `LHA_CODE_MAP_TOKEN_BUDGET=2000`) and puts the ranked definitions, callers and
tests it finds into the lead's first message. ripwire is offline, deterministic and writes nothing
into the workspace; the reference image in [`sandbox/`](../sandbox/) includes it. Without it, or
if it fails, the cycle runs without a map.

Two things make the map find the right code more often:

- **The query includes the item's witnesses.** Test names often use the code's own words when the
  description does not. For a task described as "mask GitLab tokens", the description alone missed
  `redact.py`; with its witness `test_ab_redact.py` added, the map found it.
- **A retry maps from the failure.** When the item has a failure report, the map comes from
  `ripwire . --from-trace=-` on its last 4,000 characters instead. On four real LHA failure
  reports for that task, it ranked `redact.py` first every time. If the trace finds no code, the
  task query is used. The report reaches ripwire through an environment variable and stdin, never
  the command line or a file in the workspace.

**Measured so far: it has not paid off.** On 27–28 September 2026 we ran two missions with and two
without the map, with `gemma4` as the lead, on three small changes in LHA's own Python package.
The map pointed at the right file first for two of the three changes and missed the third, whose
description never used the file's own words. The runs with the map used about 30% more input tokens
per cycle (44.5k against 34.2k) and cost about 14% more, because the map stays in the conversation
and is re-sent with every model turn; they made about 18% fewer navigation calls. Neither arm
finished any of the three changes within nine cycles. This says nothing yet about a stronger lead
model, and the same runs exposed the split-ordering starvation fixed above. The witness-aware query
and the trace-first retries came after these runs and are not measured yet. Leave it off unless you
measure a gain on your own missions.

The reviewer's effect was measured separately, on three small changes to this repository
([11-multi-agent-organization.md](11-multi-agent-organization.md#measured-the-reviewer-and-the-gates-2-october-2026)).

## Optional: ask the code instead of reading it

`LHA_CODE_QUERY=true` gives every role (the lead, researchers, the reviewer and implementers) a
read-only `code_query` tool backed by the same ripwire. Unlike the code map, the model pays for an
answer only when it asks, and each answer is small:

| `kind` | `target` | Answers |
|---|---|---|
| `find` | a task in words | the code most relevant to it, ranked |
| `definition` | a symbol | its full body |
| `callers` | a symbol | what calls it |
| `uses` | a symbol | where it is used |
| `impact` | a symbol | what reaches it: the blast radius of changing it |

On LHA's own code, `callers`, `uses`, `impact` and `definition` answers for `redact_text` were 1–4 KB
each. ripwire's own measurements put the `grep`-and-read equivalent at several times that; LHA has
not measured it. An
unknown symbol comes back as a short tool error (`symbol not found`), and a sandbox without
ripwire as a clear one. Models write a method as `Class.method`; ripwire spells it
`Class::method`, so a symbol question that misses on a dotted name is asked again as
`Class::method`, then as the bare `method`, and the answer says which name it matched.

Measured: no effect. Four missions on 29 September 2026 (two with, two without) ran the same three
changes to LHA's own code with a Sonnet lead (`LHA_MODEL_BACKEND=claude_code`), 20 turns per
cycle and the Docker sandbox. All four finished every change on its first attempt in three
cycles. Sonnet called `code_query` once or twice per mission (two of the three calls failed) and
kept using `grep` and `read_file`. Missions with the tool cost $2.77 and $2.90 in Claude Code's
reported cost; missions without it cost $2.81 and $3.29. That difference is within the noise of
two runs per arm. A second round on 30 September, after the fixes in the next section, gave the
same answer: Sonnet called `code_query` once per mission, and missions with it cost $1.03 and
$0.84 against $1.09 and $1.33 without it. One call cannot account for that gap.

Both rounds ran on LHA's Python package alone. On the whole repository (Python, Go, `spec/` and
the docs), the "sorted blocked ids" change got harder to find: a search for `deadlock` returns
512 matches there against 207 in `python/`, more than one `grep` result holds, and one mission
without the tool spent two full cycles searching. So on 1 October that change alone ran eight
times on the whole repository, alternating four missions without the tool and four with it (Sonnet
lead, 20 turns, after the `Class.method` retry above):

| | Cost per mission | Model calls | `grep` calls | Time |
|---|---|---|---|---|
| without `code_query` | $0.84–1.34 (mean $1.03) | 15–20 (mean 18) | 7–11 | 85–128 s |
| with `code_query` | $0.61–0.80 (mean $0.72) | 14–18 (mean 15.5) | 5–9 | 74–93 s |

Every mission finished the change on its first attempt in one cycle. Sonnet called `code_query`
once or twice per mission and every call succeeded. Every mission with the tool cost less than
every mission without it (a rank test on the eight costs gives p ≈ 0.03), about 30% less on
average.

A second change run the same way the same day says to read that cautiously. It asked for a
budget refusal message in a user's words, on the whole repository, eight missions alternating
the two arms. Sonnet found the code within about seven tool calls in every mission and never
called `code_query`, yet the four missions with the tool on cost $3.56 on average against $4.50
without it: a 20% gap the tool did not cause. Both rounds always ran the arm without the tool
first, and the second also asked for a message an existing unit test contradicted, so every
mission spent a cycle on a rolled-back attempt. The first round's gap may be partly the same
noise. So the result is suggestive, not shown: the tool may help where the code is hard to find
by searching, and made no measurable difference where it was easy. It stays off by default;
measure it on your own missions, with the arm order alternated.

## Give a strong model room: turns per cycle

`LHA_MAX_TURNS_PER_CYCLE` caps the model turns in one cycle. The default is 20; it was 8 until
the runs below. A strong model on an
unfamiliar codebase spends many of them finding the code before it edits anything. In the same
three changes with 8 turns, Sonnet used every turn reading and searching on one change and never
wrote it; on another it rewrote a whole file with `write_file` and dropped 118 lines, which the
verifier rolled back. Each run finished only one of the three changes (plus parts of a split one)
in nine cycles. With `edit_file` (a snippet replacement, now in the default toolset) and 20 turns,
every run finished all three in three cycles at about the same cost. The two changes landed
together, so the runs do not say which one mattered more.

A cycle that runs out of turns is logged as `turns_exhausted`. Most cycles in the 20-turn runs
still did, and failed tool calls explained much of it. Recording why each call failed showed
three causes. The model wrote tool arguments beside `"tool"` instead of under `"arguments"`,
which LHA dropped, so the same `grep` failed again and again. It guessed `python -m pytest`,
which fails where pytest is installed only in the project's environment. And after its edit it
ran unrelated test suites instead of signalling done. LHA now accepts the flat form, shows the
command beside each witness, and tells the lead to signal done once its acceptance checks pass
(the loop verifies then and returns any failure while turns remain).

Measured on the single "sorted blocked ids" change, one Sonnet run per step (29 September 2026):

| After | Model calls | Failed tool calls | Out of turns | Cost |
|---|---|---|---|---|
| `edit_file` and 20 turns | 20 | 7 | yes | $1.00 |
| flat arguments and witness commands | 20 | 2 | yes | $1.05 |
| "signal done once the checks pass" | 11 | 0 | no | $0.42 |

Each run passed on its first attempt. One run per step is a small sample, so the three-change
missions were run again with all the fixes (four missions each round, Sonnet lead, 20 turns):

| Round | Model calls per mission | Cycles out of turns | Cost per mission |
|---|---|---|---|
| `edit_file` and 20 turns (29 September) | 54–60 | 2–3 of 3 | $2.77–3.29 (mean $2.94) |
| all three fixes (30 September) | 23–29 | none | $0.84–1.33 (mean $1.07) |

Every mission in both rounds finished all three changes on the first attempt in three cycles.
The few calls that still failed were the model's own: a wrong test path, or a wider test suite
that needs network access the sandbox does not allow. If `turns_exhausted` still shows on most cycles,
look at the failed `tool_call` events first; raise the limit only if cycles end before the edit.
Each turn costs a model call, and the budget ceiling still applies.

## What this does not solve

- **Model quality.** The harness refuses unverified work; it cannot make a weak model strong. In
  one real run with a local Ollama model (gemma4) on a toy mission its planner split into five items, four items were verified and the
  fifth ended blocked. Expect to use a strong model for a project of this size.
- **Cost.** Order of magnitude only: in the Sonnet runs above, a cycle cost $0.30–0.45 in Claude
  Code's reported cost once the fixes landed, so a thousand-cycle mission is in the $300–450
  range, more with a larger codebase or harder items. Set the budget ceiling deliberately.
- **Egress is by hostname.** TLS is not intercepted, so allowing a host allows everything on it.
  `fetch_url` connects only to addresses it checked, so DNS rebinding cannot redirect it to a
  private address.
- **Status in the store can lag the workflow.** The workflow writes `SLEEPING`,
  `DEGRADED_PARK`, open gates and every ending to the row, but best effort: if the store is down
  `lha missions` shows an older status. Use `lha mission-status` for the live state and
  `lha gates` for the recorded gates.
- **Witness schemes.** fabric-emulator's own manifest also uses `sdk:`, `py:` and `boundary:`
  witnesses; LHA rejects them, so translate them to `pytest:`, `cmd:` or `trusted:` first.
- **Skipped Go tests fail their witness**: a `--- SKIP` is not a `--- PASS`. Tests that only run in
  CI belong behind a `trusted:` witness.
