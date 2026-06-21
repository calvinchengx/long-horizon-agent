# LHA Architecture

A durable, self-improving agent **organization** for long-horizon software missions. The central
idea: **week/month durability is an engineering property, not a model capability.** The model
thinks in short bursts; the *system* runs for weeks by externalizing truth, journaling every step,
and verifying with real tests.

## Four planes

1. **Durable control plane (Temporal).** `MissionWorkflow` is a deterministic scheduler. Every
   non-deterministic effect (LLM/tool/git) is a journaled **activity** — recorded once, replayed
   from cache after a crash. Durable sleep parks idle time at zero cost; Continue-As-New bounds
   history; idempotency keys make retries safe. → `src/lha/durable/`
2. **Memory & state (truth outside the window).** The git **mission anchor**
   (`progress.md` + `checklist.json` + `decisions.ndjson` + `events.ndjson`) is re-read every
   cycle (“assume interruption”). Tiered memory (episodic / semantic / procedural) +
   skill library sit behind it. → `src/lha/state/`, `src/lha/memory/`
3. **Execution & tools.** A pluggable `Sandbox` (local → Docker → E2B) runs commands; the
   `AllowListDispatcher` gates every tool call (allow-list + default-deny egress + arg
   validation). Tools: fs / shell / git / verify / **web search + fetch (deep research)**.
   → `src/lha/execution/`
4. **Safety / governance / verification.** The `DeterministicVerifier` is the only merge gate
   (exit-code ground truth). The `BudgetGovernor` pre-emptively caps spend; the `LoopDetector`
   stops oscillation; HITL gates pause on irreversible actions with a timeout + default.
   → `src/lha/verify/`, `src/lha/governor/`, `src/lha/hitl/`

## The asymmetric org

Multi-agent is a *cost*, not a feature: it wins only for **breadth-first reads** and
**independent verification**, and loses on coupled coding. So:

- **One single-threaded Lead Engineer** owns all coupled writes (coherent design decisions).
- **Ephemeral sub-agents** fan out only for parallel, side-effect-free work (Researchers) and
  fresh-context checking (Reviewer/Tester) — spawned as Temporal child workflows.
- **One Integrator** is the sole writer to `main`; the **Planner** assigns single-writer-per-file
  ownership (`src/lha/coordination/ownership.py`) so parallel implementers never collide.

## The model layer

One `ModelProvider` interface, swappable by config: `stub` (tests), `ollama` (local/$0),
`openai_compat` (Groq/Gemini/OpenRouter), `claude` (Messages API). Every call returns real output
+ real token usage; cost is computed from real numbers. → `src/lha/model/`

## Honesty policy

Every output, tool call, token, and dollar shown is real. The `StubModel` and recorded replays
are for tests/CI only and are always labeled. Nothing fabricated is presented as a real run.
Predicted outputs (e.g. `docs/predicted-runs/`) are labeled **PREDICTED** and exist to be compared
against real runs — never passed off as captured runs.

## What's proven vs. frontier

- **Proven & tested:** durable crash-recovery + idempotency, externalized state, deterministic
  verification, the tool/egress gates, the budget governor, tiered memory retrieval.
- **Frontier (not over-claimed):** fully hands-off *multi-week* autonomy. The system runs for
  weeks (sleeps, survives, resumes); the model drives it in verified bursts with human gates on
  irreversible actions.
