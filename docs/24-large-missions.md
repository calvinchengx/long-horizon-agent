# Building a large project

A mission the size of a real product (fabric-emulator is about 225k lines of Go and Python,
built over roughly 1,000 commits) needs more than a task description and a unit-test gate. This
guide shows how to set LHA up for that kind of work. Every step uses a feature that is implemented
and tested. The end-to-end test
[`test_large_mission_e2e.py`](../python/tests/integration/test_large_mission_e2e.py) runs the first
eight rows of the table below together in one local mission on real Docker (the approval goes
to a simulated approver, not the durable gate); the durable gates, the escalation ladder, the
webhook and the mission store have their own tests.

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

## 7. Operate it

```bash
lha mission-status <id>        # status, cycles, sleep time, the open gate and recent gate events
lha mission-approve <id> --decision approve   # or reject; retry | abort | impossible for a deadlock
lha mission-snooze <id> --seconds 3600        # sleep before the next cycle; --seconds 0 wakes it
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

## Optional: a code map each cycle

A cycle starts from a mostly fresh context, and the lead spends turns finding its way around the
code. With `LHA_CODE_MAP=ripwire`, the harness runs [ripwire](https://github.com/redhat-et/ripwire)
in the sandbox at the start of each cycle (`ripwire . --pack-task="<the item>"`, about 1k tokens
at the default `LHA_CODE_MAP_TOKEN_BUDGET=2000`) and puts the ranked definitions, callers and
tests it finds into the lead's first message. ripwire is offline, deterministic and writes nothing
into the workspace; the reference image in [`sandbox/`](../sandbox/) includes it. Without it, or
if it fails, the cycle runs without a map.

**Measured so far: it has not paid off.** On 27–28 September 2026 we ran two missions with and two
without the map, with `gemma4` as the lead, on three small changes in LHA's own Python package.
The map pointed at the right file first for two of the three changes and missed the third, whose
description never used the file's own words. The runs with the map used about 30% more input tokens
per cycle (44.5k against 34.2k) and cost about 14% more, because the map stays in the conversation
and is re-sent with every model turn; they made about 18% fewer navigation calls. Neither arm
finished any of the three changes within nine cycles. This says nothing yet about a stronger lead
model, and the same runs exposed the split-ordering starvation fixed above. Leave it off unless you
measure a gain on your own missions.

## What this does not solve

- **Model quality.** The harness refuses unverified work; it cannot make a weak model strong. In
  one real run with a local Ollama model (gemma4) on a toy mission its planner split into five items, four items were verified and the
  fifth ended blocked. Expect to use a strong model for a project of this size.
- **Cost.** Order of magnitude only: about 8 turns of roughly 30k input and 2k output tokens per
  cycle costs around $1 on Claude Sonnet before caching, so a thousand-cycle mission is in the
  $1–2k range. Set the budget ceiling deliberately.
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
