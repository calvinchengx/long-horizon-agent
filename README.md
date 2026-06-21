<h1 align="center">LHA — Long-Horizon Agent</h1>

<p align="center">
  <em>A durable, self-improving agent organization that makes verified progress on
  software missions spanning days to weeks — surviving crashes, reboots, and
  context-window limits without losing the plot.</em>
</p>

<p align="center">
  <strong>Durable execution (Temporal)</strong> ·
  <strong>git as source of truth</strong> ·
  <strong>deterministic verification</strong> ·
  <strong>pluggable models</strong> ·
  <strong>runs at $0</strong>
</p>

---

> **Status: early / work-in-progress (Phase 0).** The durable core is being built first.
> See [the roadmap](#roadmap) and [what's proven vs. frontier](#honesty-what-is-proven-vs-frontier).
> This README describes the system as designed; features are landed incrementally and
> nothing here is marketing — see the honesty section.

## What this is

Most "autonomous agents" are a single LLM loop that dies the moment the process restarts,
the context window fills, or an API call fails. **LHA treats long-horizon autonomy as an
engineering problem, not a model capability.** The model thinks in short bursts; the *system*
runs for weeks by:

- **Externalizing the source of truth.** The context window is a lossy cache. The real state —
  a git repo + a structured progress/checklist/decision log — lives outside it and is re-read
  every cycle ("assume interruption"). A reboot on day 12 reconstructs situational awareness in
  seconds.
- **Durable execution.** The agent loop runs inside a [Temporal](https://temporal.io) workflow.
  Every LLM/tool call is a journaled, retried, replay-from-cache activity, so a crash resumes
  exactly where it left off — no work lost, no tokens re-spent. Idle time is spent in *durable
  sleep* at zero cost.
- **Deterministic verification.** A checklist item is only "done" when **real tests/lint/build/
  typecheck pass** — never on the model's say-so. This is what stops per-step errors compounding
  over thousands of steps.

## The architecture (in one picture)

An **asymmetric agent organization**: one single-threaded **Lead Engineer** owns all coupled
code-writes (so design decisions stay coherent), and specialized agents fan out **only** for the
two things that genuinely parallelize — **reading the codebase** and **independently reviewing
the work**. This is a deliberate, evidence-based choice (see [docs/architecture.md](docs/architecture.md)).

```
        HUMAN ── approve · steer · gate irreversible acts (durable signals)
                                │
   ┌────────────────────────────▼─────────────────────────────┐
   │  MissionWorkflow  (Temporal orchestrator — thin scheduler) │
   └──┬──────────┬───────────┬───────────┬───────────┬─────────┘
      ▼          ▼           ▼           ▼           ▼
  [Planner]  [LEAD ENG]  [Researchers] [Reviewer]  [Integrator]
  DAG +      sole writer  (read-only    (fresh ctx, single
  ownership  (SDK loop)   fan-out)      adversarial) write to main
  ─────────── GROUND TRUTH: git + checklist ───────────
  ─────────── VERIFIER: pytest + ruff + ty + build (the only merge gate) ───────────
```

## Runs at $0 — and never faked

The only real cost is LLM tokens, and the model layer is pluggable behind one interface:

| Backend | Cost | Notes |
|---|---|---|
| **Ollama** (local) | $0 | Real outputs, offline, fully yours. Needs a capable machine. |
| **Free-tier cloud** (OpenAI-compatible: Groq / Gemini / OpenRouter) | $0\* | Stronger than small local models; rate-limited. |
| **Claude** (API key, or Agent SDK via a Pro/Max subscription) | varies | Strongest output; subscription = $0 marginal within plan limits. |
| `stub` | $0 | Deterministic test double — **for tests/CI only, never shown as a real run.** |

**Honesty is the whole point of this project.** Every number you see (tokens, cost, latency) is
read from the real provider response. Anything presented as a real agent run uses genuine model
output or a clearly-labeled recorded replay — never fabricated text dressed up as live AI.

## <a name="honesty-what-is-proven-vs-frontier"></a>What's proven vs. frontier

- ✅ **Proven & tested:** durability/crash-recovery, externalized state, deterministic
  verification gating, the coordination protocol, observability.
- 🔬 **Frontier (do not over-claim):** *fully hands-off* multi-week autonomy. No model today does
  this reliably ([METR](https://metr.org) task horizons are still well under a day at high
  reliability). LHA's claim is that the **system** runs for weeks (sleeps, survives, resumes) while
  the **model** drives it in verified bursts, with human gates on irreversible actions.

## Quickstart

```bash
uv sync                 # install (provisions Python 3.12)
uv run lha version
uv run lha config       # show resolved config (secrets redacted)
```

More commands land with each phase. See [docs/predicted-runs/](docs/predicted-runs/) for
clearly-labeled **predicted** outputs you can compare against your own real runs.

## <a name="roadmap"></a>Roadmap

Built lean-baseline-first; the multi-agent org is layered on only where it provably helps.

- **Phase 0** — durable single-agent spine + crash-recovery test ← *in progress*
- **Phase 1** — deterministic verifier as a measured gate
- **Phase 2** — independent reviewer/critic + decision log
- **Phase 3** — read-only researcher fan-out
- **Phase 4+** — planner + integrator + conditional parallel writers; memory + skills; offline evolution

## Author & license

Created and maintained by **Calvin Cheng** ([calvin@calvinx.com](mailto:calvin@calvinx.com)).

MIT — Copyright (c) 2026 Calvin Cheng. See [LICENSE](LICENSE).
