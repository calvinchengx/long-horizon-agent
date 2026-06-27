# Honesty policy

LHA runs models that can produce plausible text about work they did not do. The project's rule is
that nothing it presents is fabricated, and that its documentation claims only what the code and
its tests show.

## The rules

1. **Real output or a labelled recording.** Anything shown as an agent run is genuine model output
   from a real provider, or a recording clearly labelled as such. Text written by a person to look
   like a run is labelled as a prediction or as illustrative, at the top of the file.
2. **The stub is a test double.** `StubModel` names itself `stub:<name>` in every log line and
   trace, returns deterministic echoes or scripted turns, and costs `$0`. It is used for tests and
   CI, never presented as a real run (see [13-models.md](13-models.md#stub-tests-and-ci-only)).
3. **Numbers come from the provider.** Token counts are the `usage` the provider returned. Cost is
   computed from those tokens and a stated price. If there is no price, the cost is recorded as
   *unknown*, never as `$0`, and the governor refuses to spend an unknown amount unless
   `LHA_ALLOW_UNPRICED_MODELS=true` (see [10-cost-and-budget.md](10-cost-and-budget.md)).
4. **The model never marks its own work done.** An item is `done` only when at least one gating
   check (a real command, run in the sandbox or, for an operator's trusted check, on the host)
   passes, and every one of the item's witnesses passes. The model's "done" only ends its turn
   (see [07-verification.md](07-verification.md)). Splitting a blocked item moves its witnesses to
   the last child, so replanning cannot lower the bar.
5. **Docs describe the code as it is.** Designed-but-unwired features are labelled **planned**.
   These pages were checked against the code; where they disagree with the code, the code wins
   and the page is a bug.

## Proven by tests

Each claim below is backed by tests that CI runs on every push and pull request (the Python
suites; see [20-testing.md](20-testing.md)):

| Claim | Evidence |
|---|---|
| A crashed cycle is retried without double-committing or committing partial work | `tests/durability/test_durable_spine.py`: `test_crash_after_commit_is_idempotent`, `test_crash_before_commit_discards_residue` |
| A mission survives Continue-As-New and long runs with bounded history | `test_continue_as_new_completes`, `tests/load/test_long_run.py` |
| An outage parks the mission and it resumes afterwards | `test_outage_parks_then_resumes` |
| Budget exhaustion ends the mission instead of retrying | `test_budget_exhaustion_ends_mission` |
| Deadlocks are reported as deadlocks, never as completion | `test_deadlocked_mission_is_reported_as_deadlocked` |
| Gate decisions: early decisions are kept, invalid ones rejected, defaults applied | `test_early_retry_decision_unblocks_and_completes`, `test_invalid_decision_is_rejected_and_default_applies` |
| Workflow changes that break recorded histories are caught | `tests/durability/test_replay.py` |
| The anchor reconstructs state from git after a restart | `tests/unit/test_mission_anchor.py` |
| Verification gates completion; harness tampering is reverted and fails the item | `tests/unit/test_agent_loop.py`, `test_loop_checklist.py`, `test_verify_contracts.py` |
| Command classification, egress checks and redaction behave as specified | `tests/unit/test_safety*.py`, `test_obs_redact.py`, `spec/` cases |
| Witnesses gate items (a missing Go test fails), trusted checks run outside the sandbox on the candidate commit, protected paths are reverted, blocked items are split within the replan budget | `tests/unit/test_large_missions.py`, `test_witnesses.py`, `test_trusted_runner.py` |
| Irreversible actions wait for a human on the durable path; an approval is used once, a rejection is not re-asked | `tests/durability/test_approvals.py`, `tests/unit/test_large_missions.py` |
| The sandbox egress proxy allows only listed hosts, refuses private addresses, and leaves no containers or networks behind | `tests/unit/test_egress_proxy.py`, `tests/integration/test_docker_egress.py` |
| Migrations, the idempotent cost ledger and the pgvector index work on real Postgres; the Docker sandbox enforces its limits; one mission exercises every large-mission feature together on real Docker | `tests/integration/` (CI job `python-services-integration`), including `test_large_mission_e2e.py` |

These tests use the stub model (scripted, including the replanner's split and the approver's
answer) and simulated failures. They prove that the system behaves correctly around the model;
they say nothing about how well any model does the work, including how good a model's splits of
a blocked item are.

## Not proven

- **Hands-off multi-week autonomy.** No test or recorded run shows a real model completing a
  multi-day mission unattended. LHA's claim is narrower: the system can run for weeks (sleep,
  survive crashes, resume) while a model drives it in verified steps. Whether a given model makes
  useful progress over that span is an open question.
- **The multi-agent organization helps.** `lha orchestrate` runs researchers, a lead and a
  reviewer, and is unit-tested with scripted models. There is no measurement showing it beats the
  single-agent loop on real tasks.
- **Operational features that are built but not wired.** Langfuse export, OpenTelemetry export
  from the CLI, the Postgres repositories (including the cost ledger), semantic and episodic
  memory, file ownership, the hash-chained decision log, sub-agent fan-out inside the durable
  workflow, saga compensation, orphan reconciliation, flaky-test quarantine, offline prompt
  evolution and the eval harness exist as tested library code with no command or workflow
  calling them (see [23-roadmap.md](23-roadmap.md)). The large-mission features (checklist
  import, witnesses, trusted checks, protected paths, replanning, sandbox image and egress,
  references, approvals) are wired into every Python run path.
- **Real-service paths without CI coverage.** The E2B sandbox is excluded from coverage and never
  run in CI. The Ollama, OpenAI-compatible and Claude backends are tested against mocked HTTP, not
  live endpoints.

## Predicted runs

[`predicted-runs/`](predicted-runs/) holds hand-written expected outputs, written before running
the commands, so you can compare them with your own real runs. They are not captured runs.

| File | Label at the top | Contents |
|---|---|---|
| [`predicted-runs/phase0-durable-spine.md`](predicted-runs/phase0-durable-spine.md) | "PREDICTED RUN", "THIS IS A PREDICTION, NOT A CAPTURED RUN" | expected check output, git history and checklist of a three-item mission |
| [`predicted-runs/lsp-weeklong-run.txt`](predicted-runs/lsp-weeklong-run.txt) | "illustrative log (fiction ...; NOT executed)" | an imagined week-long mission log building a small language server |

Compare the shape, not the details: timings, hashes and counts will differ. Both files predate
parts of the current code and do not match it everywhere:

- `phase0-durable-spine.md` names `test_crash_after_side_effect_is_idempotent` (now
  `test_crash_after_commit_is_idempotent`), predicts about 30 tests (the suite now collects over
  600), and shows a `phase0-placeholder` check name (check names are now derived from the check
  command).
- `lsp-weeklong-run.txt` shows features the code does not have: a worker Build ID, a separate
  `lha-research` task queue, research fan-out inside the durable run, a `--task @file` argument
  form, and a log format that `lha` does not print.

To produce a real run to compare against, configure a real model ([13-models.md](13-models.md))
and run `lha mission` or `lha mission-start`; the git history and `.lha/` files it leaves are the
record.
