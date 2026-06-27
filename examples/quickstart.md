# Quickstart

Run a real mission end-to-end on your machine. Everything below works at **$0** with the local
stub or Ollama — no API key required.

## 1. Install

The commands below use the Python implementation; run them from `python/`. The Go
implementation (`go/`) accepts the same commands once its phase has landed.

```bash
cd python
uv sync
uv run lha version
uv run lha config        # resolved settings (secrets redacted)
```

## 2. Run a mission locally (no Temporal needed)

The Planner decomposes your task into a checklist, then the agent loop works each item (using
tools, gating on the deterministic verifier) and checkpoints to git:

```bash
uv run lha mission --task "Create a hello.py that prints hello and a test for it" \
                   --workdir .lha/workspaces/demo
```

You'll see a summary: items done / total, cycles, and **real cost** (── $0 on stub/Ollama).
Inspect the work — it's all real git history:

```bash
git -C .lha/workspaces/demo log --oneline
cat .lha/workspaces/demo/.lha/progress.md
cat .lha/workspaces/demo/.lha/checklist.json
```

## 3. Use a real model

```bash
# Local, $0, offline (needs Ollama running):
LHA_MODEL_BACKEND=ollama LHA_MODEL_NAME=qwen2.5-coder:7b \
  uv run lha mission --task "..."

# Free-tier cloud (OpenAI-compatible, e.g. Groq):
LHA_MODEL_BACKEND=openai_compat LHA_OPENAI_BASE_URL=https://api.groq.com/openai/v1 \
  LHA_OPENAI_API_KEY=... LHA_MODEL_NAME=llama-3.3-70b-versatile uv run lha mission --task "..."

# Claude:
LHA_MODEL_BACKEND=claude LHA_ANTHROPIC_API_KEY=... LHA_MODEL_NAME=claude-sonnet-4-6 \
  uv run lha mission --task "..."
```

## 4. Run on the durable spine (survives crashes)

```bash
docker compose up -d           # Temporal + Postgres(pgvector) + Langfuse
uv run lha worker              # serve missions
# (submit a mission to the MissionWorkflow from another process / a forthcoming `lha mission start`)
```

Kill the worker mid-run and restart it: Temporal replays the journal and resumes exactly — no work
lost, no double commits. That's the durability guarantee, provable via `uv run pytest -q
tests/durability`.
