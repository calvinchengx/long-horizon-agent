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
5. **Docs describe the code as it is.** Code that exists but that no command or workflow calls
   is labelled **library**; designed features with no code are labelled **planned**. These pages
   were checked against the code; where they disagree with the code, the code wins and the page
   is a bug.

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
| Human gates escalate with reminders and then apply their default; the deadlock gate accepts retry, abort and impossible; a scheduled start and the pause between cycles report `SLEEPING` | `tests/durability/test_human_gates.py`, `tests/unit/test_hitl_ladder.py` |
| Web tools are registered only with an allow-list, deny other hosts and private addresses, bind brokered credentials to their hosts, fence their output as untrusted, and a run holding web tools plus private data is refused before it starts | `tests/unit/test_web_wiring.py`, `test_web_tools.py` |
| Web tools and `lha vendor` resolve each host once and connect only to the checked addresses: a DNS-rebinding resolver cannot reach a private IP, on the first request or any redirect hop; TLS SNI and the `Host` header keep the hostname | `tests/unit/test_web_pinning.py` |
| The fallback chain fails over on transient errors only; the health probe contacts the configured provider | `tests/unit/test_model_failover_health.py` |
| Every run path writes the mission row and every metered model call to the store; `lha missions` and `lha costs` read them back | `tests/unit/test_persistence_wiring.py`, `tests/integration/test_postgres_store.py` |
| Every human gate event is written idempotently to `hitl_gates` (durable `notify_gate` and the local terminal approver); `lha gates` lists them | `tests/unit/test_persistence_store.py`, `test_hitl_ladder.py`, `test_persistence_postgres_fake.py`, `tests/integration/test_postgres_store.py` |
| An abort during a cycle ends with the row `ABORTED`: the workflow waits for the cancelled cycle, a cycle that completes anyway cannot swallow the abort, and the store never moves a terminal status back | `tests/durability/test_mission_row.py`, `tests/unit/test_persistence_store.py`, the `mission_cancel_mid_cycle.json` replay history |
| Recorded decisions are hash-chained, shown in the next cycle's prompt, and an altered log stops the run | `tests/unit/test_decision_chain_wiring.py` |
| Parallel implementers cannot write files they do not own, and only verified, owned, conflict-free, re-verified branches are merged | `tests/unit/test_ownership_integration.py` |
| Tiered memory is recalled into the prompt, survives activity retries, and degrades to lexical-only retrieval instead of failing a cycle | `tests/unit/test_memory_service.py`, `test_persistence_wiring.py` |
| The Ollama embedder gates vectors by model and digest, pads to pgvector's width, and an absent or failing Ollama means lexical-only memory, not a failed cycle | `tests/unit/test_memory_ollama.py` (mocked Ollama API) |

These tests use the stub model (scripted, including the replanner's split and the approver's
answer) and simulated failures. They prove that the system behaves correctly around the model;
they say nothing about how well any model does the work, including how good a model's splits of
a blocked item are.

## Not proven

- **Hands-off multi-week autonomy.** No test or recorded run shows a real model completing a
  multi-day mission unattended. LHA's claim is narrower: the system can run for weeks (sleep,
  survive crashes, resume) while a model drives it in verified steps. Whether a given model makes
  useful progress over that span is an open question.
- **The multi-agent organization helps.** `lha orchestrate` runs researchers, a lead, parallel
  implementers with a branch integrator, and a reviewer, and is unit-tested with scripted models.
  There is no measurement showing it beats the single-agent loop on real tasks.
- **Library code that no command or workflow calls.** Langfuse export (`build_langfuse` only
  builds a client), an OpenTelemetry exporter (LHA installs no `TracerProvider`), sub-agent
  fan-out inside the durable workflow (the worker registers `SubAgentWorkflow` and
  `research_children` is tested, but `MissionWorkflow` never starts a child), saga compensation,
  orphan-branch reconciliation, the durable ticket ledgers, flaky-test quarantine, mutation checks
  and trust bootstrap, offline prompt evolution and the eval harness exist as tested library code
  only (see [23-roadmap.md](23-roadmap.md)). These, by contrast, are wired into the run paths: the
  large-mission features, human approval gates with the escalation ladder and webhook, the
  `SLEEPING` status, web tools under the egress policy and the Rule of Two, the fallback model
  chain and health probe, SQLite/Postgres persistence of mission rows, the cost ledger and human gates, tiered
  memory in the prompt, the hash-chained decision log, and, in `lha orchestrate` only, file
  ownership, tickets, the blackboard, parallel implementer waves and the branch integrator.
- **Known gaps in wired features.** The mission row and the `hitl_gates` rows are best-effort
  copies that lag when the store is down, and a durable gate's row cannot name the person who
  answered. A `LeaseRequest` for a foreign file is never granted. `lha orchestrate` does not
  resume an earlier run. The default `hash` embedder is lexical, not semantic (`ollama` is
  semantic but needs a running Ollama server). Every `openai_compat` model, primary or fallback,
  uses the one endpoint in `LHA_OPENAI_BASE_URL`. The Go port has no CLI and no Temporal worker
  yet.
- **Real-service paths without CI coverage.** The E2B sandbox is excluded from coverage and never
  run in CI. The Ollama, OpenAI-compatible and Claude backends and the Ollama embedder are tested
  against mocked HTTP, not live endpoints, so no test shows how much a real embedding model
  improves recall.

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
  1,000), and shows a `phase0-placeholder` check name (check names are now derived from the check
  command).
- `lsp-weeklong-run.txt` shows features the code does not have: a worker Build ID, a separate
  `lha-research` task queue, research fan-out inside the durable run, a `--task @file` argument
  form, and a log format that `lha` does not print.

To produce a real run to compare against, configure a real model ([13-models.md](13-models.md))
and run `lha mission` or `lha mission-start`; the git history and `.lha/` files it leaves are the
record.
