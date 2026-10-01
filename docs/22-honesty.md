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
   passes, and every one of the item's witnesses passes. A check proven flaky (it passed and
   failed on the same code) must still pass on the code under test and never counts as that
   gating pass. The model's "done" only ends its turn (see
   [07-verification.md](07-verification.md)). Splitting a blocked item moves its witnesses to
   the last child, so replanning cannot lower the bar.
5. **Docs describe the code as it is.** Code that exists but that no command or workflow calls
   is labelled **library**; designed features with no code are labelled **planned**. These pages
   were checked against the code; where they disagree with the code, the code wins and the page
   is a bug.

## Evidence ledger

One row per claim, with its status, the tests that back it in each implementation, and the CI job
that runs them. Last checked against the code at commit `7881a97` (1 October 2026);
[`test_docs_evidence.py`](../python/tests/unit/test_docs_evidence.py) fails CI when a cited file
or test no longer exists.

| Status | Meaning |
|---|---|
| proven | tests show the behaviour against the real component (git, a Temporal test or dev server, a real sandbox, Postgres or Docker where the CI job provides them) |
| fake-only | the behaviour is tested, but the external service is a fake or mocked HTTP; the real service is never contacted in CI |
| measured | a live experiment with a real model, reported with its numbers; not a CI test |
| unproven | no test and no measurement |

Every row uses the stub model or a scripted fake (including the replanner's split and the
approver's answer). The rows prove that the system behaves correctly around the model; they say
nothing about how well any model does the work. The CI jobs are described in
[20-testing.md](20-testing.md); `python-check` runs the unit, durability and load suites, and
`go` also runs the cross-implementation tests, which call the Python CLI.

| Claim | Status | Python evidence | Go evidence | CI job |
|---|---|---|---|---|
| A crashed cycle is retried without double-committing or committing partial work | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_crash_after_commit_is_idempotent`, `test_crash_before_commit_discards_residue` | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestCrashAfterCommitIsIdempotent`, `TestCrashBeforeCommitDiscardsResidue` | `python-check`, `go` |
| A mission survives Continue-As-New and long runs with bounded history | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_continue_as_new_completes`; [`test_long_run.py`](../python/tests/load/test_long_run.py) | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestContinueAsNewCompletes` | `python-check`, `go` |
| An outage parks the mission and it resumes afterwards | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_outage_parks_then_resumes` | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestOutageParksThenResumes` | `python-check`, `go` |
| Budget exhaustion ends the mission instead of retrying | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_budget_exhaustion_ends_mission` | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestBudgetExhaustionEndsMission` | `python-check`, `go` |
| Deadlocks are reported as deadlocks, never as completion | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_deadlocked_mission_is_reported_as_deadlocked` | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestDeadlockedMissionIsReportedAsDeadlocked`; [`runner_test.go`](../go/internal/agent/runner_test.go): `TestRunnerReportsDeadlockNeverComplete` | `python-check`, `go` |
| Gate decisions: early decisions are kept, invalid ones rejected, defaults applied | proven | [`test_durable_spine.py`](../python/tests/durability/test_durable_spine.py): `test_early_retry_decision_unblocks_and_completes`, `test_invalid_decision_is_rejected_and_default_applies` | [`spine_test.go`](../go/internal/durable/spine_test.go): `TestEarlyRetryDecisionUnblocksAndCompletes`, `TestInvalidDecisionIsRejectedAndDefaultApplies` | `python-check`, `go` |
| Workflow changes that break recorded histories are caught | proven | [`test_replay.py`](../python/tests/durability/test_replay.py): `test_recorded_histories_still_replay`, `test_replay_detects_a_changed_workflow` | [`replay_test.go`](../go/internal/durable/replay_test.go): `TestRecordedHistoriesStillReplay`, `TestReplayDetectsAChangedWorkflow` | `python-check`, `go` |
| The anchor reconstructs state from git after a restart | proven | [`test_mission_anchor.py`](../python/tests/unit/test_mission_anchor.py) | [`mission_anchor_test.go`](../go/internal/state/mission_anchor_test.go) | `python-check`, `go` |
| Verification gates completion; harness tampering is reverted and fails the item | proven | [`test_agent_loop.py`](../python/tests/unit/test_agent_loop.py); [`test_loop_checklist.py`](../python/tests/unit/test_loop_checklist.py); [`test_verify_contracts.py`](../python/tests/unit/test_verify_contracts.py) | [`verify_test.go`](../go/internal/verify/verify_test.go); [`harness_integrity_test.go`](../go/internal/verify/harness_integrity_test.go); [`loop_test.go`](../go/internal/agent/loop_test.go) | `python-check`, `go` |
| Command classification, egress checks and redaction behave as specified | proven | [`test_safety_commands.py`](../python/tests/unit/test_safety_commands.py); [`test_safety_egress.py`](../python/tests/unit/test_safety_egress.py); [`test_obs_redact.py`](../python/tests/unit/test_obs_redact.py) | [`commands_test.go`](../go/internal/safety/commands_test.go); [`egress_test.go`](../go/internal/safety/egress_test.go); [`obs_test.go`](../go/internal/obs/obs_test.go) | `python-check`, `go` |
| Witnesses gate items, trusted checks run outside the sandbox on the candidate commit with a minimal environment, protected paths are reverted, blocked items are split within the replan budget | proven | [`test_large_missions.py`](../python/tests/unit/test_large_missions.py); [`test_witnesses.py`](../python/tests/unit/test_witnesses.py); [`test_trusted_runner.py`](../python/tests/unit/test_trusted_runner.py) | [`witnesses_test.go`](../go/internal/verify/witnesses_test.go); [`trusted_env_test.go`](../go/internal/verify/trusted_env_test.go); [`loop_test.go`](../go/internal/agent/loop_test.go): `TestBlockedItemIsSplitInsteadOfDeadlocking` | `python-check`, `go` |
| Irreversible actions wait for a human on the durable path; an approval is used once, a rejection is not re-asked; a completing cycle opens no gate | proven | [`test_approvals.py`](../python/tests/durability/test_approvals.py): `test_a_cycle_that_completes_the_mission_opens_no_gate`; [`test_large_missions.py`](../python/tests/unit/test_large_missions.py) | [`gates_test.go`](../go/internal/durable/gates_test.go): `TestRealMissionApprovalRunsTheActionOnce`, `TestApprovedActionReachesTheNextCycleOnce`, `TestACycleThatCompletesTheMissionOpensNoGate` | `python-check`, `go` |
| Human gates escalate with reminders and then apply their default; the deadlock gate accepts retry, abort and impossible; a scheduled start and the pause between cycles report `SLEEPING` | proven | [`test_human_gates.py`](../python/tests/durability/test_human_gates.py); [`test_hitl_ladder.py`](../python/tests/unit/test_hitl_ladder.py) | [`gates_test.go`](../go/internal/durable/gates_test.go): `TestUnansweredGateEscalatesThenRejects`, `TestDeadlockGateHumanDeclaresImpossible`, `TestPauseBetweenCyclesIsSleeping`; [`approvals_test.go`](../go/internal/hitl/approvals_test.go) | `python-check`, `go` |
| An abort during a cycle ends with the row `ABORTED`: the workflow waits for the cancelled cycle, and the store never moves a terminal status back | proven | [`test_mission_row.py`](../python/tests/durability/test_mission_row.py); [`test_persistence_store.py`](../python/tests/unit/test_persistence_store.py) | [`row_test.go`](../go/internal/durable/row_test.go): `TestCancellingDuringACycleRecordsAbortedAndNeverParks` | `python-check`, `go` |
| Recorded decisions are hash-chained, shown in the next cycle's prompt, and an altered log stops the run | proven | [`test_decision_chain_wiring.py`](../python/tests/unit/test_decision_chain_wiring.py) | [`decision_chain_test.go`](../go/internal/state/decision_chain_test.go): `TestAnchorChainsDecisionsAndRefusesTampering` | `python-check`, `go` |
| After an embedder change, stale memory rows are re-embedded (a bounded batch each cycle, or `lha memory reembed`) and dense recall comes back | proven | [`test_memory_service.py`](../python/tests/unit/test_memory_service.py): `test_reembed_restores_dense_recall_after_an_embedder_change`, `test_recall_re_embeds_a_bounded_batch_each_cycle`; [`test_postgres_store.py`](../python/tests/integration/test_postgres_store.py): `test_pgvector_reembed_after_an_embedder_change` | [`service_test.go`](../go/internal/memory/service_test.go): `TestReembedRestoresDenseRecallAfterAnEmbedderChange`, `TestRecallReEmbedsABoundedBatchEachCycle` | `python-check`, `python-services-integration`, `go` |
| A failing check is re-run; one that passes and fails on the same work tree is quarantined with a committed event, and a quarantined check never makes an item green | proven | [`test_flaky_retry_verifier.py`](../python/tests/unit/test_flaky_retry_verifier.py) | [`flaky_retry_test.go`](../go/internal/verify/flaky_retry_test.go); [`flaky_test.go`](../go/internal/agent/flaky_test.go) | `python-check`, `go` |
| With `LHA_MUTATION_CHECK`, a green verdict must also survive a mutation run over the changed files; it never runs on a red or unverified verdict, and a failing run keeps the item red | proven | [`test_mutation_gate.py`](../python/tests/unit/test_mutation_gate.py): `test_a_local_mission_is_gated_by_the_mutation_check` | [`mutation_gate_test.go`](../go/internal/verify/mutation_gate_test.go); [`mutation_gate_test.go`](../go/internal/agent/mutation_gate_test.go): `TestALocalMissionIsGatedByTheMutationCheck` | `python-check`, `go` |
| Parallel implementers cannot write files they do not own, and only verified, owned, conflict-free, re-verified branches are merged | proven | [`test_ownership_integration.py`](../python/tests/unit/test_ownership_integration.py) | [`coordination_test.go`](../go/internal/coordination/coordination_test.go); [`org_test.go`](../go/internal/agents/org/org_test.go) | `python-check`, `go` |
| A lease is granted only for an unowned file or one whose owner has finished, and is committed with a `lease` event; `lha orchestrate --resume` continues the same mission without losing committed work | proven | [`test_leases_and_resume.py`](../python/tests/unit/test_leases_and_resume.py) | [`leases_test.go`](../go/internal/execution/tools/leases_test.go); [`orchestrate_test.go`](../go/cmd/lha/orchestrate_test.go) | `python-check`, `go` |
| A durable mission that opts in runs researcher child workflows, parallel implementer waves merged one checkpoint at a time, and a review that can reopen an item; an abort during a wave waits for the implementers; the org history replays | proven | [`test_org_workflow.py`](../python/tests/durability/test_org_workflow.py); [`test_replay.py`](../python/tests/durability/test_replay.py): `test_fresh_org_history_replays` | [`org_test.go`](../go/internal/durable/org_test.go): `TestParallelWaveWithResearchAndReviewCompletes`, `TestAbortDuringAnOrgWaveWaitsForTheImplementersAndEndsAborted` | `python-check`, `go` |
| Every run path writes the mission row and every metered model call to the store; `lha missions` and `lha costs` read them back | proven | [`test_persistence_wiring.py`](../python/tests/unit/test_persistence_wiring.py); [`test_postgres_store.py`](../python/tests/integration/test_postgres_store.py) | [`store_test.go`](../go/internal/persistence/store_test.go); [`store_cmds_test.go`](../go/cmd/lha/store_cmds_test.go) | `python-check`, `python-services-integration`, `go` |
| Every human gate event is written idempotently to `hitl_gates`; `lha gates` lists them | proven | [`test_persistence_store.py`](../python/tests/unit/test_persistence_store.py); [`test_hitl_ladder.py`](../python/tests/unit/test_hitl_ladder.py); [`test_persistence_postgres_fake.py`](../python/tests/unit/test_persistence_postgres_fake.py) | [`gate_store_test.go`](../go/internal/hitl/gate_store_test.go): `TestTerminalApproverRecordsEachGateInTheStore` | `python-check`, `go` |
| Migrations, the idempotent cost ledger and the pgvector index work on real Postgres; the Docker sandbox enforces its limits; one mission exercises every large-mission feature together on real Docker | proven | [`test_postgres.py`](../python/tests/integration/test_postgres.py); [`test_docker_sandbox.py`](../python/tests/integration/test_docker_sandbox.py); [`test_large_mission_e2e.py`](../python/tests/integration/test_large_mission_e2e.py) | Go's Postgres and Docker tests (`*_it_test.go`, `docker_integration_test.go`) run only locally, with `LHA_IT_POSTGRES_DSN` / `LHA_IT_DOCKER=1` | `python-services-integration` |
| The sandbox egress proxy allows only listed hosts, refuses private addresses, and leaves no containers or networks behind | proven | [`test_egress_proxy.py`](../python/tests/unit/test_egress_proxy.py); [`test_docker_egress.py`](../python/tests/integration/test_docker_egress.py) | [`proxy_test.go`](../go/internal/execution/egressproxy/proxy_test.go) | `python-check`, `python-services-integration`, `go` |
| Tiered memory is recalled into the prompt, survives activity retries, and degrades to lexical-only retrieval instead of failing a cycle | proven | [`test_memory_service.py`](../python/tests/unit/test_memory_service.py); [`test_persistence_wiring.py`](../python/tests/unit/test_persistence_wiring.py) | [`service_test.go`](../go/internal/memory/service_test.go) | `python-check`, `go` |
| Web tools are registered only with an allow-list, deny other hosts and private addresses, bind brokered credentials to their hosts, fence their output as untrusted, and a run holding web tools plus private data is refused before it starts | proven | [`test_web_wiring.py`](../python/tests/unit/test_web_wiring.py); [`test_web_tools.py`](../python/tests/unit/test_web_tools.py) | [`web_test.go`](../go/internal/execution/tools/web_test.go); [`main_test.go`](../go/cmd/lha/main_test.go) | `python-check`, `go` |
| Web tools and `lha vendor` resolve each host once and connect only to the checked addresses, on the first request and every redirect hop | proven | [`test_web_pinning.py`](../python/tests/unit/test_web_pinning.py): `test_lha_vendor_pins_every_hop_too`; [`test_large_missions.py`](../python/tests/unit/test_large_missions.py): `test_vendor_refuses_redirects_off_the_named_hosts` | [`web_test.go`](../go/internal/execution/tools/web_test.go): `TestFetchBlocksPrivateAddressesAndBadRedirects`; [`vendor_test.go`](../go/internal/state/vendor/vendor_test.go); [`vendor_test.go`](../go/cmd/lha/vendor_test.go) | `python-check`, `go` |
| For the same inputs, a Go run leaves the same checkpoint commits and `.lha/` anchor as a Python run, byte for byte (`run-local`, `mission`, `orchestrate`, including resuming the other's mission) | proven | run by the Go suite, which runs the Python CLI side by side | [`e2e_test.go`](../go/cmd/lha/e2e_test.go): `TestE2EScriptedRunMatchesPython`; [`orchestrate_test.go`](../go/cmd/lha/orchestrate_test.go): `TestE2EOrchestrateCrossImplementationResume` | `go` |
| Either CLI drives a durable mission served by either worker; a Go- and a Python-served org mission leave the same anchor | proven | run by the Go suite | [`durable_e2e_test.go`](../go/cmd/lha/durable_e2e_test.go): `TestGoMissionDrivenByThePythonCLI`, `TestPythonMissionDrivenByTheGoCLI`; [`durable_org_e2e_test.go`](../go/cmd/lha/durable_org_e2e_test.go): `TestGoAndPythonOrgMissionsLeaveTheSameAnchor` | `go` |
| The anchor, mission store, ClaimCheck payloads and spend journal written by one implementation are read correctly by the other | proven | run by the Go suite | [`crossimpl_test.go`](../go/internal/state/crossimpl_test.go); [`crossimpl_test.go`](../go/internal/persistence/crossimpl_test.go): `TestSQLiteStoreIsInterchangeableWithPython`; [`hardening_test.go`](../go/internal/durable/hardening_test.go): `TestClaimCheckIsInterchangeableWithPython`, `TestSpendJournalIsSharedWithPython` | `go` |
| Both implementations pass every `spec/` conformance case | proven | [`test_spec_conformance.py`](../python/tests/unit/test_spec_conformance.py) | [`spec/`](../go/internal/spec/) | `python-check`, `go` |
| A Go and a Python worker never serve the same task queue: each refuses at start, and a running worker stops when the other appears | proven | [`test_worker_guard.py`](../python/tests/durability/test_worker_guard.py): `test_real_server_a_worker_refuses_a_go_polled_queue`, `test_real_server_a_go_worker_that_appears_later_stops_the_python_worker` | [`hardening_test.go`](../go/internal/durable/hardening_test.go): `TestWorkerGuardRefusesPythonPollers`; [`durable_e2e_test.go`](../go/cmd/lha/durable_e2e_test.go): `TestWorkersRefuseMixedTaskQueues` | `python-check`, `go` |
| With worker versioning on, a mission stays on the worker build that started it when a newer build becomes current; a half-set build pair is refused | proven | [`test_worker_versioning.py`](../python/tests/durability/test_worker_versioning.py): `test_real_server_missions_stay_on_the_build_that_started_them`, `test_half_configured_versioning_is_refused` | [`versioning_test.go`](../go/internal/durable/versioning_test.go): `TestDeploymentOptionsFollowTheSettings`; [`versioning_e2e_test.go`](../go/cmd/lha/versioning_e2e_test.go): `TestMissionsStayOnTheBuildThatStartedThem` | `python-check`, `go` |
| A long local mission does not grow memory with every cycle: the bounded structures stay at their caps; `lha objects prune` deletes only ClaimCheck objects older than the retention | proven | [`test_bounded_memory.py`](../python/tests/unit/test_bounded_memory.py); [`test_memory_growth.py`](../python/tests/load/test_memory_growth.py): `test_a_long_local_mission_does_not_grow_with_every_cycle`; [`test_object_store_prune.py`](../python/tests/unit/test_object_store_prune.py): `test_prune_deletes_only_old_objects`, `test_cli_objects_prune` | [`growth_test.go`](../go/internal/agent/growth_test.go): `TestALongLocalMissionKeepsItsStructuresAtTheirCaps`; [`objectstore_prune_test.go`](../go/internal/durable/objectstore_prune_test.go): `TestPruneObjectsDeletesOnlyOldObjects` | `python-check`, `go` |
| The safety code (command classifier, egress policy, Rule of Two, redaction, path containment, sandbox egress lists) has no surviving mutant | proven | [`mutation_audit.sh`](../python/scripts/mutation_audit.sh) (mutmut) | [`mutation_audit.sh`](../go/scripts/mutation_audit.sh) (gremlins) | `python`, `go` in [`mutation.yml`](../.github/workflows/mutation.yml) (nightly) |
| The `claude_code` backend and lead engine: one `claude -p` session per cycle using LHA's tools over the MCP bridge, the harness still decides, the session is capped by the budget | fake-only | [`test_claude_code.py`](../python/tests/unit/test_claude_code.py): `test_a_cycle_is_one_claude_session_using_lha_tools`, `test_verify_reports_failures_and_the_harness_still_decides`, `test_the_session_is_refused_when_its_cap_would_break_the_budget` | [`claude_code_engine_test.go`](../go/internal/agent/claude_code_engine_test.go): `TestACycleIsOneClaudeSessionUsingLHATools`; [`bridge_test.go`](../go/internal/agent/mcpbridge/bridge_test.go) | `python-check`, `go` |
| `edit_file` replaces one exact snippet and is refused unless the snippet matches exactly once | proven | [`test_tools_dispatcher.py`](../python/tests/unit/test_tools_dispatcher.py): `test_edit_file_changes_one_exact_snippet_and_keeps_the_rest` | [`conformance_edit_file_test.go`](../go/internal/spec/conformance_edit_file_test.go): `TestExecutionEditFile` | `python-check`, `go` |
| A System One answer can only narrow what happens: triage never widens an item's authority, and reranking only reorders and drops | proven | [`test_spec_conformance.py`](../python/tests/unit/test_spec_conformance.py): `test_system_one_authority` | [`conformance_systemone_authority_test.go`](../go/internal/spec/conformance_systemone_authority_test.go): `TestSystemOneAuthority` | `python-check`, `go` |
| The cycle-start code map and `code_query`: read-only, and any ripwire failure means no map or a tool error, never a failed cycle | proven | [`test_code_map.py`](../python/tests/unit/test_code_map.py): `test_any_failure_means_no_map_not_a_failed_cycle`; [`test_code_query.py`](../python/tests/unit/test_code_query.py): `test_the_setting_registers_a_read_only_tool_for_every_role` | [`code_map_test.go`](../go/internal/agent/code_map_test.go): `TestCodeMapFailuresMeanNoMap`; [`code_query_test.go`](../go/internal/execution/tools/code_query_test.go): `TestCodeQueryFailuresAreToolErrors` | `python-check`, `go` |
| System One: requests match the API, unsafe endpoints are refused, and triage acts only on a confident answer; reranking falls back on failure | fake-only | [`test_system_one.py`](../python/tests/unit/test_system_one.py): `test_triage_acts_only_on_a_confident_scope_or_environment`; `test_system_one_reranker_ranks_by_relevance_and_falls_back_on_failure` | [`systemone_test.go`](../go/internal/systemone/systemone_test.go); [`systemone_test.go`](../go/internal/agent/systemone_test.go); [`rerank_systemone_test.go`](../go/internal/memory/rerank_systemone_test.go) | `python-check`, `go` |
| The fallback chain fails over on transient errors only; the health probe contacts the configured provider | fake-only | [`test_model_failover_health.py`](../python/tests/unit/test_model_failover_health.py) | [`failover_test.go`](../go/internal/model/failover_test.go); [`fallback_health_test.go`](../go/internal/model/fallback_health_test.go) | `python-check`, `go` |
| The Ollama, OpenAI-compatible and Claude backends | fake-only | [`test_model_backends.py`](../python/tests/unit/test_model_backends.py) | [`backends_test.go`](../go/internal/model/backends_test.go) | `python-check`, `go` |
| The Ollama embedder gates vectors by model and digest; the Voyage embedder batches and parses as specified; a failing embedder means lexical-only memory | fake-only | [`test_memory_ollama.py`](../python/tests/unit/test_memory_ollama.py); [`test_memory_voyage.py`](../python/tests/unit/test_memory_voyage.py) | [`service_test.go`](../go/internal/memory/service_test.go); [`voyage_test.go`](../go/internal/memory/voyage_test.go) | `python-check`, `go` |
| Trace export: missions, cycles, model calls and tool calls are spans with redacted attributes; a dead backend does not block the run | fake-only | [`test_otel_tracing.py`](../python/tests/unit/test_otel_tracing.py) | [`tracing_test.go`](../go/internal/obs/tracing/tracing_test.go); [`tracing_test.go`](../go/cmd/lha/tracing_test.go) | `python-check`, `go` |
| The E2B sandbox | fake-only | [`test_sandbox_e2b.py`](../python/tests/unit/test_sandbox_e2b.py); [`test_sandbox_e2b_sdk.py`](../python/tests/unit/test_sandbox_e2b_sdk.py) | n/a (Python only) | `python-check` |
| Opening a Docker sandbox first removes the `lha.owner`-labelled containers and networks whose owning process on this host has exited, and nothing else | fake-only | [`test_exec_sandboxes.py`](../python/tests/unit/test_exec_sandboxes.py): `test_open_removes_what_a_dead_process_left_and_labels_what_it_starts`, `test_an_owner_is_gone_only_when_its_process_on_this_host_has_exited` | [`sandbox_docker_test.go`](../go/internal/execution/sandbox_docker_test.go): `TestOpenRemovesWhatADeadProcessLeftAndLabelsWhatItStarts`, `TestAnOwnerIsGoneOnlyWhenItsProcessOnThisHostHasExited` | `python-check`, `go` |
| System One triage saves cycles | measured | [25: Measured](25-system-one.md#measured): 6 missions with a self-hosted Kev-4B, 27 September 2026; no saving, triage never acted | — | — |
| The ripwire code map saves work | measured | [24: code map](24-large-missions.md#optional-a-code-map-each-cycle): 4 missions, `gemma4` lead, 27–28 September 2026; about 14% more cost with the map, no change finished in either arm; untested with a stronger lead | — | — |
| `code_query` saves work | measured | [24: code_query](24-large-missions.md#optional-ask-the-code-instead-of-reading-it): two rounds of 4 missions on the Python package (29 and 30 September 2026) showed no difference; 8 missions of one hard-to-find change on the whole repository (1 October) cost about 30% less with it, every mission with the tool cheaper than every mission without; one change, one repository, Sonnet lead | — | — |
| The multi-agent organization beats the single-agent loop on real tasks | unproven | no measurement | — | — |
| Hands-off multi-week autonomy: a real model completes a multi-day mission unattended | unproven | no test or recorded run | — | — |

## Known gaps

Limits of features that are wired and tested:

- The mission row and the `hitl_gates` rows are best-effort copies that lag when the store is
  down, and a durable gate's row cannot name the person who answered.
- A contended lease is refused, not queued, and a lease lasts until its writer's item is done.
- The implementers of one durable wave run concurrently, so the budget ceiling can be overshot by
  up to one wave's spend.
- `lha orchestrate --resume` loses blackboard posts and reflections made after the interrupted
  run's last checkpoint.
- The default `hash` embedder is lexical, not semantic (`ollama` is semantic but needs a running
  Ollama server).
- Every `openai_compat` model, primary or fallback, uses the one endpoint in
  `LHA_OPENAI_BASE_URL`.
- Go implements everything except the E2B sandbox and the `sentence_transformers` /
  `cross_encoder` extras, and a Go and a Python worker cannot serve the same mission (each refuses
  a task queue the other polls).

Saga compensation, orphan-branch reconciliation, the Magentic-One ledgers, the old mutation-testing
wrapper, trust bootstrap, offline prompt evolution, the Claude Agent SDK lead, and the Auditor, Librarian, Tester
and model-backed Integrator runners were removed rather than kept as uncalled code. The accuracy
and calibration figures in [25-system-one.md](25-system-one.md) are published by TypeSafe, Kev's
authors and independent testers, not measured by LHA.

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
- `lsp-weeklong-run.txt` shows features the code does not have: a separate
  `lha-research` task queue (durable research, with `--research`, runs on the mission's own task
  queue), a `--task @file` argument form, and a log format that `lha` does not print.

To produce a real run to compare against, configure a real model ([13-models.md](13-models.md))
and run `lha mission` or `lha mission-start`; the git history and `.lha/` files it leaves are the
record.
