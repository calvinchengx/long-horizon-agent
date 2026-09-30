# Verification

The deterministic verifier is the only component that can mark a checklist item `done`. It runs
commands in the sandbox and aggregates their exit codes. The model's own claim that it is
finished only ends its turn loop; it has no effect on the item's status.

Code: [`python/src/lha/contracts/verify.py`](../python/src/lha/contracts/verify.py) (types,
verdict rules, check naming) and [`python/src/lha/verify/`](../python/src/lha/verify/)
(`DeterministicVerifier`, witnesses, trusted checks, harness integrity, flaky-check quarantine).
Go mirror: [`go/internal/verify/`](../go/internal/verify/), which has the verifier, witnesses,
trusted checks (with the same minimal environment), harness integrity, the flaky-check quarantine
(`FlakyRetryVerifier`) and the mutation gate.

## Checks

A `Check` is:

| Field | Meaning |
|---|---|
| `name` | Identifier recorded in results and in the item's `verified_by` |
| `command` | An argv list (at least one element), run in the sandbox session's workdir |
| `gating` | `true` (default): a non-zero exit blocks the item. `false`: advisory, recorded but never blocks |
| `timeout_s` | Per-check timeout; `null` uses the verifier default of 1200 seconds |
| `where` | `sandbox` (default) or `trusted`: an operator-defined check run outside the sandbox (see [trusted checks](#trusted-checks)) |

`DeterministicVerifier.verify` runs the checks in order through the sandbox's `exec`, so they
run where the code is (inside the container for `docker`), not on the orchestrating host. Each
produces a `CheckResult`: `name`, `passed`, `exit_code`, `gating`, `duration_s`, `timed_out`,
`output_tail`, `output_ref`. If the sandbox cannot execute a check, the result is a failure with
exit code `-1`, never a pass. `DeterministicVerifier` refuses to run a `where="trusted"` check
in the sandbox: it returns a failing result saying no trusted runner is configured.

## Verdict rules

`VerificationResult` derives its verdict from the results; a value passed in is overridden.

| Gating results | Verdict | `all_green` | Item can become `done` |
|---|---|---|---|
| None | `unverified` | `false` | No |
| At least one, all passed | `passed` | `true` | Yes |
| At least one, any failed | `failed` | `false` | No |

Non-gating results never affect the verdict. A run with zero gating checks is `unverified`, not
a vacuous pass: a mission cannot be configured so that items become `done` without evidence.
`Checklist.record_success` additionally refuses to mark an item done with an empty list of
passing gating checks.

Only these rules mark an item done. With [System One triage](25-system-one.md) on, a model's
answer can split or block an item that keeps failing, earlier than the failure limit. It can never
mark one done: a split's last child still carries the parent's witnesses.

## When verification runs

Within a cycle (see [architecture](05-architecture.md#the-cycle)):

1. When the model signals done, the mission's checks and the active item's
   [witnesses](#witnesses) run. If the verdict is `failed` and turns remain, the failure report
   is sent back to the model and the loop continues.
2. At the end of the cycle, the checks run again if the workspace changed since the last run (or
   no run happened), and the harness-integrity result is added.
3. The final verdict decides the checkpoint: `passed` gives `record_success`; anything else gives
   `record_failure`, which blocks the item after 3 consecutive failures; a newly blocked item
   may then be split by the replanner ([item lifecycle](06-mission-anchor.md#item-lifecycle)).

In a parallel wave (`lha orchestrate`, or a durable mission started with `--max-parallel`), each
implementer's branch is verified in its own git
worktree with the same checks and witnesses. The `BranchIntegrator` merges a verified branch
into the mission branch only after checking that the branch changed no file its writer does
not own, then runs the checks again on the merged result; the item becomes `done` only if that
post-merge verification passes. The checkpoint's `cycle` event reports the post-merge results
when a merge happened, otherwise the branch's own
([multi-agent organization](11-multi-agent-organization.md)).

## Choosing checks

### Defaults

When no checks are given, the gate is the standard one for a uv-managed Python repository:

| Name | Command |
|---|---|
| `ruff` | `uv run ruff check .` |
| `ty` | `uv run ty check` |
| `pytest` | `uv run pytest -q` |

These only make sense for such a repository, and they need `uv`, and the tools, wherever the
checks run. The Docker sandbox's default image (`LHA_SANDBOX_IMAGE`,
`ghcr.io/astral-sh/uv:python3.12-bookworm-slim`) has uv but no network, so `uv run` can only use
tools the workspace already has installed, unless `LHA_SANDBOX_EGRESS` allows the package index.
Choose checks the image can run, or build an image that has them
([installation](03-installation.md#sandbox-image-and-egress)).

### CLI flags

`lha mission`, `lha run-local`, `lha orchestrate` and `lha mission-start` accept:

- `--check "<command>"` (repeatable): a gating check, split with shell quoting rules (`shlex`).
  It is added to the defaults.
- `--no-default-checks`: drop the defaults. At least one non-empty `--check` is then required;
  otherwise the command fails with `--no-default-checks requires at least one non-empty --check`.

```bash
uv run lha mission --task "..." --no-default-checks \
  --check "uv run --with pytest pytest -q" --check "uv run ruff check ."
```

Every check given on the command line is gating. On Temporal, `MissionInput.check_commands` is
a list of argv lists: `null` means the defaults, and an explicit empty list is rejected as a
configuration error.

### Check names

Names are derived from the argv by `derive_check_name`: runner prefixes (`uv`, `uvx`, `run`,
`exec`, `poetry`, `pipenv`, `pdm`, `hatch`, `rye`, `npx`, `npm`, `pnpm`, `yarn`, `bunx`, `-m`),
Python interpreters and option-like tokens are skipped, and the first remaining token's base
name is used (unsafe characters replaced with `_`, at most 40 characters). For example,
`uv run pytest -q` gives `pytest`, `npx tsc` gives `tsc`, `python -c "..."` gives `python`, and
`./scripts/verify.sh` gives `verify.sh`. Duplicate names get `-2`, `-3`, ... suffixes, and the
name `harness_integrity` is reserved. The cases are pinned in
[`spec/contracts/check_names.json`](../spec/contracts/check_names.json).

## Witnesses

The mission's checks prove the repository still builds; they do not prove that a particular item
was delivered. An item's `witnesses` name its own acceptance checks, which gate that item in
addition to the mission's checks
([`verify/witnesses.py`](../python/src/lha/verify/witnesses.py)). Each witness becomes a gating
check named after the witness string:

| Witness | Runs | Passes when |
|---|---|---|
| `go:TestName` or `go:TestName@<packages>` | `go test -count=1 -run '^TestName$' -v <packages>` (default `./...`); subtests as `TestX/sub` | `go test` exits 0 and prints `--- PASS: TestName`, so a missing, skipped or filtered-out test fails |
| `pytest:<node id>` | `uv run pytest -q <node id>` | exit 0 (pytest exits 5 when nothing is collected) |
| `cmd:<shell command>` | `sh -c <command>` | exit 0 |
| `trusted:<name>` (alias `ci:<name>`) | the operator's command `<name>` from `LHA_TRUSTED_CHECKS`, outside the sandbox | exit 0 |

Witness syntax is validated when a checklist is imported (Go test names must be identifiers,
package patterns and pytest node ids may not contain shell metacharacters or start with `-`). A
witness that cannot be turned into a check at run time, for example `trusted:e2e` when no `e2e`
command is defined, is recorded as a failing check (exit code 2, `invalid witness: ...`), never
skipped. When the replanner splits an item, its witnesses move to the last child, so splitting
never weakens the item's acceptance.

The lead's prompt lists the item's witnesses with the command that runs each one in the sandbox,
for example `- pytest:tests/test_x.py::test_y (run: uv run pytest -q tests/test_x.py::test_y)`.
A `trusted:` witness is listed without one, since only the verifier can run it. Without the
command, models guessed `python -m pytest`, which fails in an image where pytest is installed only
in the project's environment.

Witnesses are written in a [checklist file](06-mission-anchor.md#importing-a-checklist); the
Planner does not produce them.

## Trusted checks

Some evidence cannot be produced in the sandbox: an end-to-end suite that needs a Docker daemon, a
real database, a GPU or a CI job. The operator defines such commands in `LHA_TRUSTED_CHECKS`, a
JSON object of name to argv:

```bash
export LHA_TRUSTED_CHECKS='{"e2e": ["make", "e2e"], "ci": ["./scripts/ci-and-wait.sh"]}'
```

Items reference them by name only (`trusted:e2e`); an item can never define the argv. The lead's
verifier ([`verify/trusted.py`](../python/src/lha/verify/trusted.py), `TrustedAwareVerifier`)
runs sandbox checks in the sandbox and trusted checks with `CommandTrustedRunner`:

1. It makes a candidate commit of the current working tree (tracked and untracked, non-ignored
   files) using a temporary index, without moving `HEAD` or touching the real index or working
   tree.
2. For each trusted check, it adds a detached `git worktree` of that commit, runs the argv on the
   host from the worktree directory that corresponds to the workspace, and removes the worktree
   afterwards.
3. The check gets a minimal environment, not the operator's: `PATH`, `LANG`/`LC_ALL`/`LC_CTYPE`
   and `TZ` if set (plus `SYSTEMROOT`, `COMSPEC`, `PATHEXT` and the like on Windows), a fresh empty
   `HOME` and `TMPDIR` (`HOME/tmp`) that are deleted afterwards, `GIT_TERMINAL_PROMPT=0`,
   `LHA_CHECK_COMMIT`, `LHA_CHECK_WORKTREE` and `LHA_CHECK_NAME`, and the variables named in
   `LHA_TRUSTED_CHECK_ENV`.
4. The default timeout is 3600 seconds (or the check's `timeout_s`); on timeout the whole process
   group is killed (on Windows, the process). A failure to build the commit, create the worktree
   or run the command is a failing result.

`LHA_TRUSTED_CHECK_ENV` is a comma-separated list of host variables to pass through, for
example `GOFLAGS,GOPROXY,GOMODCACHE` so a Go suite reuses the host's module cache instead of
downloading into the empty `HOME`. Names are passed verbatim when set on the host and override
the defaults above (listing `HOME` gives the check the real home). Names starting with `LHA_`
are refused (LHA's own settings hold its API keys and database DSN); any other name is the
operator's explicit choice, including secret-looking ones such as a CI token for a hand-off
script. A listed variable is readable by agent-written code, so list only what the check needs.

### Threat model

A trusted check runs code the agent wrote (tests, build scripts, `conftest.py`, `Makefile`
targets) outside the sandbox, as the user running the mission (the local CLI, or the Temporal
worker). The sandbox's network and filesystem limits do not apply. Defining
`LHA_TRUSTED_CHECKS` is the opt-in: with it unset nothing runs on the host, and there is no
separate switch, because every trusted check is a command the operator wrote.

What the runner prevents:

- inherited credentials: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `AWS_*`, `GITHUB_TOKEN`,
  `SSH_AUTH_SOCK`, every `LHA_*` setting and anything else in the operator's environment are not
  passed unless listed in `LHA_TRUSTED_CHECK_ENV`;
- dotfiles through `HOME`: tools that read `~/.netrc`, `~/.npmrc`, `~/.pypirc`, `~/.docker/config.json`
  or `~/.config/gh` find an empty home;
- checks that outlive their timeout, and worktrees or temporary homes left behind.

What it does not prevent: the process runs as the operator's user, so code that knows where to
look can still read the real home by absolute path (`/home/me/.aws/credentials`), use a Docker
socket it can reach (root-equivalent), or reach the network. Run missions that use trusted checks
on a dedicated, disposable machine or VM with no secrets, or make the trusted command a small
script that hands `LHA_CHECK_COMMIT` to CI and waits for the verdict. Protect the files a trusted
check executes with `LHA_HARNESS_PATHS` (next section) so the agent cannot rewrite what the gate
runs.

Malformed `LHA_TRUSTED_CHECKS`, or an invalid or `LHA_*` name in `LHA_TRUSTED_CHECK_ENV`, is a
configuration error: local runs stop, and the durable activity fails with a non-retryable
`MissionConfigError`.

## Output tails

Each result keeps the last 4,000 characters of the combined output (stdout, then
`--- stderr ---`, then stderr), prefixed with `...[truncated]...` when clipped. The failure
report shown to the model includes, for each failed gating check, its name, exit code (or
`timed out`) and the last 1,500 characters of its tail. The report is stored in the item's
`last_failure` and the next cycle's prompt includes up to its last 3,000 characters.

An `unverified` verdict produces the report `UNVERIFIED: no gating checks ran, so the item
cannot be marked done.`

## Harness integrity

The checks run the repository's own tests and configuration, which the agent can edit. To stop
it passing the gate by weakening the gate, the loop hashes (SHA-256) the protected files that
exist at cycle start and compares them before verification is accepted.

Protected files:

| Rule | Examples |
|---|---|
| Any file under a directory named `tests` or `test`, at any depth | `tests/test_a.py`, `test/helpers.py`, `src/pkg/tests/x.py` |
| `pyproject.toml`, `setup.cfg`, `.coveragerc` at the workspace root | |
| `conftest.py`, `tox.ini`, `pytest.ini`, `noxfile.py` anywhere | `src/conftest.py`, `sub/pytest.ini` |
| Files matching `test_*.py` or `*_test.py` anywhere | `pkg/test_models.py`, `pkg/models_test.py` |

Directories such as `.git`, `.lha`, `.venv`, `node_modules`, caches, `build` and `dist` are not
scanned, and symlinks are ignored. The rules are pinned in
[`spec/verify/harness_files.json`](../spec/verify/harness_files.json).

Operators can protect more with `LHA_HARNESS_PATHS`, a comma-separated list of
workspace-relative globs, for example `Makefile,e2e/**,.github/**`. `**` spans directories;
`*` and `?` do not cross `/`. Use it for the files that define the checks, above all the targets
that trusted checks execute.

If a protected file that existed at cycle start was modified or deleted:

- a failing gating check named `harness_integrity` is added to the verdict, listing the files;
- the files that are tracked at `HEAD` are restored from it, so the change is not committed;
- the violation stays in effect for the rest of the cycle, even after the restore.

New test files are always allowed. An item whose `allow_harness_edits` is `true` skips this
check. The Planner's prompt asks the model to set it only for steps that must modify existing
tests or test configuration; `lha run-local` never sets it.

## Flaky-check quarantine

Every run path's verifier ([`lead_verifier`](../python/src/lha/agent/assembly.py): the local
runners, the durable cycle activity, and in `orchestrate` the Lead, the implementers and the
integrator) is wrapped in `FlakyRetryVerifier`
([`verify/flaky_quarantine.py`](../python/src/lha/verify/flaky_quarantine.py)). Its rules:

1. **Re-run on failure.** A gating check that fails, and did not time out, is re-run on the same
   work tree up to `LHA_FLAKY_RETRIES` more times (default 1, at most 5), stopping at the first
   pass. A genuine failure therefore costs one extra run of that check by default.
   `LHA_FLAKY_RETRIES=0` turns re-runs and quarantine off.
2. **Evidence before quarantine.** A check is quarantined only after it both passed and failed on
   the same revision: the git tree id of the work tree, uncommitted changes included. Failing on
   one revision and passing on another is a change in behaviour, not a flake.
3. **A consistent failure always gates.** A check that fails every attempt on the revision under
   test is a failing gating check, whether or not it is quarantined. The item is red.
4. **A quarantined check is never the evidence for green.** From the moment it is quarantined,
   its results are recorded as non-gating, passes included. The item then needs at least one
   other gating check to pass, and with none it is `unverified`. With rule 3 this means a
   quarantined check must still pass at least once on the code under test, and something else
   must gate: it can keep an item red, never make one green on its own.
5. **Recorded, and for the rest of the mission.** Quarantining a check emits a
   `check_quarantined` event (`check`, `revision`, `passes`, `fails`); a quarantined check that
   fails every attempt emits `quarantined_check_failed`. The agent loop commits both to
   `.lha/events.ndjson` with the cycle's checkpoint and records them as trace events. Later
   cycles, including durable cycles in a new worker process, read the quarantined set from the
   committed log at `HEAD`, so an uncommitted edit to `.lha/` cannot quarantine a check. Nothing
   lifts a quarantine automatically.

A consequence of rule 4: if the only gating check of a mission is quarantined, no item can be
verified any more. Items end `unverified`, block after 3 consecutive failed attempts (or are
split by the replanner), and the mission reaches the deadlock gate, where an operator can fix
the check (or add a second one) and retry. Configure more than one gating check if a flaky suite
is likely.

Timed-out checks are not re-run (a timeout would cost its full duration again). Advisory
(non-gating) checks are not re-run. Witness checks follow the same rules as mission checks, so a
witness that fails every attempt still fails its item. In `orchestrate`, a quarantine decided
while verifying an implementer's worktree or the integrated result is logged (structlog
`check_quarantined`) and applied to that verification, but only the Lead's cycles commit the
event to the anchor.

The cycle event's `checks` list shows each result's `gating` flag, so a quarantined check is
visible in every checkpoint (`"gating": false`), and its output starts with `[flaky]`.

## Mutation gate

Passing tests prove the code does what the tests check, not that the tests check anything. When a
model writes the code and its tests in the same cycle, it can write tests that run the code and
assert nothing, and a gate of ruff, ty and pytest passes them. The opt-in mutation gate
([`verify/mutation_gate.py`](../python/src/lha/verify/mutation_gate.py), Go:
[`verify/mutation_gate.go`](../go/internal/verify/mutation_gate.go)) asks the question mutation
testing answers: if this code had a small bug, would a test fail?

Set `LHA_MUTATION_CHECK` to a shell command that runs a mutation tool on the changed code and
exits non-zero when mutants survive. Every run path's verifier (`lead_verifier`, so the Lead, the
implementers and the integrator) then applies these rules:

1. **Only on a green verdict.** The command runs after every gating check passed. On a red or
   `unverified` verdict it does not run: mutating code whose tests fail, or that has none, tells
   nothing and costs a full mutation run.
2. **Scoped to the item.** It runs with `sh -c` in the sandbox, like a `cmd:` witness, and gets the
   files the item changed (`git diff HEAD` plus untracked files, outside `.lha/`) in
   `LHA_CHANGED_FILES`, one per line. With no changed files it does not run. A failed attempt is
   rolled back, so these are the current attempt's changes.
3. **It can only keep an item red.** Its result is the gating check `mutation`: exit 0 passes, any
   other exit fails the item, and the command's output tail is in the failure report the model
   reads, so the surviving mutants tell it which behaviour no test pins. It is not re-run as a
   possible flake. If the changed files cannot be listed, it fails.
4. **Bounded.** `LHA_MUTATION_TIMEOUT_S` (default 1800) caps each run; a timeout fails the check.

The command names the tool, so the gate works for any language. For a Go project, gremlins
(v0.6) mutates only the lines changed since `HEAD`; print the mutants that lived or timed out and
fail if there are any (its `--threshold-efficacy` does not set the exit code):

```bash
LHA_MUTATION_CHECK="! gremlins unleash --diff HEAD --timeout-coefficient 20 -S lt | grep -E 'LIVED|TIMED OUT'"
```

For a Python project, mutmut reads its scope from `[tool.mutmut]` in `pyproject.toml`, and
`mutmut results` lists only the mutants that were not killed; print them and fail if there are any:

```bash
LHA_MUTATION_CHECK='uv run mutmut run >/dev/null; ! uv run mutmut results | grep .'
```

The tool must be in the sandbox image. A full mutation run re-runs the tests once per mutant, so
scope it to the changed code, and budget for it: it runs on every green verification.
LHA's own safety code has a nightly mutation audit of its own
([20-testing.md](20-testing.md#mutation-audit)).
