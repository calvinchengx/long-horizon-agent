# PREDICTED RUN — Phase 0 durable spine

> ⚠️ **THIS IS A PREDICTION, NOT A CAPTURED RUN.** It is the *expected* output of the commands
> below, written before running them on your machine, so you can compare actual vs. predicted.
> Anything here marked numeric (timings, shas) will differ; the **shape** and **assertions** are
> what to compare. Reproduce with the exact commands shown.

## A. Verification suite

```
$ uv run ruff check .
All checks passed!

$ uv run ty check
All checks passed!

$ uv run pytest -q
......................                                                   [100%]
~30 passed in ~25s
```

**What to compare:** ruff clean, ty clean, all tests green. The durability tests
(`tests/durability/test_durable_spine.py`) are the key ones:

- `test_mission_completes` — a 3-item mission runs to completion; exactly **3** `lha: complete …`
  commits appear in the git log.
- `test_crash_after_side_effect_is_idempotent` — an injected crash fires **after** a commit; on
  retry the mission still completes with **exactly 3** complete-commits (no double-apply). This is
  the literal "reboot loses nothing" guarantee.
- `test_continue_as_new_completes` — with Continue-As-New forced every cycle, the mission still
  completes.

## B. A mission cycle (stub model, local sandbox — $0, offline)

Predicted shape of the git history after a 3-item mission against a fresh workspace:

```
$ git -C .lha/workspaces/<mission> log --oneline
<sha> lha: complete 03 (task 3)
<sha> lha: complete 02 (task 2)
<sha> lha: complete 01 (task 1)
<sha> lha: initialize mission anchor
```

Predicted `.lha/checklist.json` at completion (every item verified):

```jsonc
{
  "items": [
    {"id": "01", "description": "task 1", "status": "done", "verified_by": ["phase0-placeholder"], "attempts": 1, "...": "..."},
    {"id": "02", "description": "task 2", "status": "done", "verified_by": ["phase0-placeholder"], "attempts": 1, "...": "..."},
    {"id": "03", "description": "task 3", "status": "done", "verified_by": ["phase0-placeholder"], "attempts": 1, "...": "..."}
  ],
  "schema_version": 1
}
```

**What to compare:** one `lha: complete <id>` commit per checklist item, all items `status:"done"`,
and a brand-new `GitMissionAnchor` over the same workspace reconstructing the same situational
awareness (this is exactly what `test_checkpoint_advances_and_survives_restart` asserts).

## C. With a real model (when configured)

Set `LHA_MODEL_BACKEND=ollama` (or `claude`) and the cycle's "act" step becomes a **real** model
turn: real generated text/tool-calls, real token usage, and a real (or $0 for local) cost in the
ledger. The durability/verification *shape* above is unchanged — only the content of the work
becomes real. Compare the cost ledger totals and the model output against this prediction.
