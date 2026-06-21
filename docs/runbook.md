# Operations Runbook

How to operate a long-running mission. (Status values are in the `missions` table / workflow
query: `RUNNING | SLEEPING | WAITING_ON_HUMAN | DEGRADED_PARK | DONE | ABORTED | IMPOSSIBLE`.)

## Observe a mission
- Local run: read the printed summary + `git -C <workdir> log --oneline` and `.lha/progress.md`.
- Durable run: the Temporal UI (`http://localhost:8080`) shows the workflow; Langfuse
  (`http://localhost:3000`) shows traces + cost. Query the workflow for `cycles_done` / `last_item`.
- **SLEEPING vs WAITING_ON_HUMAN:** a healthy parked mission is `SLEEPING` (durable timer). If it's
  `WAITING_ON_HUMAN`, a gate is open — someone must approve/reject (or the timeout default fires).

## Common actions
- **Approve/reject a gate:** resolve the HITL gate (signal the workflow / `lha` approve command).
- **Raise the budget ceiling:** update `LHA_BUDGET_USD_CEILING` and resume; the governor re-reads it.
- **Add an egress domain mid-run:** add the host to the `EgressPolicy` allow-list and redeploy the
  worker (workers pick up config on restart; in-flight workflows resume).
- **Safe deploy during an in-flight run:** bump the worker Build ID, run the CI replay harness
  against recorded histories, then blue/green the new worker. Never deploy code that fails replay.
- **Re-embed after an embedding-model change:** stand up the new `(model, version)`, run the
  re-embed migration into new vector rows, then atomically flip `valid`. Never compare vectors
  across embedding versions.

## Degradation modes (what the agent does)
- **pgvector down:** semantic retrieval falls back to lexical (BM25) + `git grep`; mission continues.
- **Langfuse/OTel down:** spans buffer/drop; never blocks the agent.
- **Sandbox down:** activity retries with backoff; persistent failure → escalate.
- **Model provider down:** retry + `fallback_model`; hard-down → `DEGRADED_PARK` + alert.
- **git or model DOWN (critical):** `decide_safe_park` parks the mission (durable sleep) and alerts.

## Negative paths
- **Stuck item:** the loop detector / `should_declare_impossible` escalates after N consecutive
  failures → a `WAITING_ON_HUMAN` "declare impossible?" gate. On confirm → `IMPOSSIBLE`/`ABORTED`
  with a final checkpoint; the workspace snapshot is retained for forensics.
- **Dead operator:** every gate has a timeout + default action + escalation ladder, so an
  unattended run never hangs invisibly.

## Recovery
- **Worker/host crash:** restart the worker — Temporal replays the journal and resumes; completed
  activities are not re-run. CAN-orphan reconciliation adopts existing branches / re-spawns missing
  ones before continuing.
