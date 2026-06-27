# Verification

The deterministic verifier is the only component that can mark a checklist item `done`. It runs
commands in the sandbox and aggregates their exit codes. The model's own claim that it is
finished only ends its turn loop; it has no effect on the item's status.

Code: [`python/src/lha/contracts/verify.py`](../python/src/lha/contracts/verify.py) (types,
verdict rules, check naming) and [`python/src/lha/verify/`](../python/src/lha/verify/)
(`DeterministicVerifier`, harness integrity, flaky quarantine). Go mirror:
[`go/internal/verify/`](../go/internal/verify/).

## Checks

A `Check` is:

| Field | Meaning |
|---|---|
| `name` | Identifier recorded in results and in the item's `verified_by` |
| `command` | An argv list (at least one element), run in the sandbox session's workdir |
| `gating` | `true` (default): a non-zero exit blocks the item. `false`: advisory, recorded but never blocks |
| `timeout_s` | Per-check timeout; `null` uses the verifier default of 1200 seconds |

`DeterministicVerifier.verify` runs the checks in order through the sandbox's `exec`, so they
run where the code is (inside the container for `docker`), not on the orchestrating host. Each
produces a `CheckResult`: `name`, `passed`, `exit_code`, `gating`, `duration_s`, `timed_out`,
`output_tail`, `output_ref`. If the sandbox cannot execute a check, the result is a failure with
exit code `-1`, never a pass.

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

## When verification runs

Within a cycle (see [architecture](05-architecture.md#the-cycle)):

1. When the model signals done, the checks run. If the verdict is `failed` and turns remain, the
   failure report is sent back to the model and the loop continues.
2. At the end of the cycle, the checks run again if the workspace changed since the last run (or
   no run happened), and the harness-integrity result is added.
3. The final verdict decides the checkpoint: `passed` gives `record_success`; anything else gives
   `record_failure`, which blocks the item after 3 consecutive failures
   ([item lifecycle](06-mission-anchor.md#item-lifecycle)).

## Choosing checks

### Defaults

When no checks are given, the gate is the standard one for a uv-managed Python repository:

| Name | Command |
|---|---|
| `ruff` | `uv run ruff check .` |
| `ty` | `uv run ty check` |
| `pytest` | `uv run pytest -q` |

These only make sense for such a repository, and they need `uv` wherever the checks run. The
Docker sandbox's image is `python:3.12-slim`, which has no `uv` and no network, so in the
`docker` sandbox you need checks that exist in that image.

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

If a protected file that existed at cycle start was modified or deleted:

- a failing gating check named `harness_integrity` is added to the verdict, listing the files;
- the files that are tracked at `HEAD` are restored from it, so the change is not committed;
- the violation stays in effect for the rest of the cycle, even after the restore.

New test files are always allowed. An item whose `allow_harness_edits` is `true` skips this
check. The Planner's prompt asks the model to set it only for steps that must modify existing
tests or test configuration; `lha run-local` never sets it.

## Flaky-test quarantine

[`verify/flaky_quarantine.py`](../python/src/lha/verify/flaky_quarantine.py) implements a
quarantine that turns a flaky check into a non-gating one. It only accepts a check with
evidence: the check must have both passed and failed on the same revision (at least once each,
over at least 3 runs by default); `mark_flaky` otherwise raises `FlakeEvidenceError`. Even with
every check quarantined, the verdict would be `unverified`, not `passed`.

The quarantine is a tested library component but is not wired into any run path today: no CLI
command or activity records check history or marks checks flaky.

The same applies to the verifier-trust helpers in
[`verify/trust_bootstrap.py`](../python/src/lha/verify/trust_bootstrap.py) (coverage
measurement) and [`verify/mutation.py`](../python/src/lha/verify/mutation.py) (mutation-testing
wrapper): implemented and tested, not called by the mission loop.
