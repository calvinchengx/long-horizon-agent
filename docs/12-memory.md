# Memory

The memory plane has three tiers: episodic (what happened), semantic (distilled facts retrieved
by similarity) and procedural (verified skills). It also has retrieval components: hybrid lexical
and dense search, and a reranker. Code: [`python/src/lha/memory/`](../python/src/lha/memory/),
contracts in [contracts/memory.py](../python/src/lha/contracts/memory.py). The wiring into a
mission is `MissionMemory` in [memory/service.py](../python/src/lha/memory/service.py).

**Integration status.** With `LHA_MEMORY_ENABLED=true` (the default), every run path gives the
lead tiered memory: `lha run-local` and `lha mission` (the local runner), `lha orchestrate` (the
Orchestrator's lead), and the Temporal `run_agent_cycle` activity. All three build the lead with
`build_lead_loop` ([agent/assembly.py](../python/src/lha/agent/assembly.py)), which takes the
run's memory as `memory=`. Sub-agents (researchers and parallel implementers), the reviewer and
the replanner do not get memory. Memory is persisted in the mission store
([Persistence](#persistence)): SQLite by default, Postgres when `LHA_POSTGRES_DSN` is set.

**In Go.** [`go/internal/memory`](../go/internal/memory/) is the same memory plane, wired into
the Go `lha run-local` and `lha mission`: the same tiers, retrieval, consolidation, skills,
degradation rules and settings, and the same rows in the same store, so either implementation
recalls what the other recorded. For the same inputs the memory block is byte-identical:
[`spec/memory/`](../spec/memory/) pins the hash embedder's vectors (Go reproduces CPython's
compensated `sum()`), BM25 scores, fusion order and the blocks recalled for a fixture. Go has the
`hash` and `ollama` embedders; `sentence_transformers` and the `cross_encoder` reranker are
Python extras, so in Go they degrade exactly as Python does without the extra (lexical-only
retrieval with a `memory_degraded` event; fusion order kept with a `memory_rerank_unavailable`
warning). On Postgres, Go sends vectors to pgvector as text literals, so it needs no adapter.

## What happens each cycle

`AgentLoop.run_cycle` ([agent/loop.py](../python/src/lha/agent/loop.py)) calls the memory plane
twice:

1. **Before the first turn, `recall`** builds one memory block for the active item. It is placed
   in the task (first user) message, after the item and its last failure and before the recent
   commits. In-session compaction keeps the task message verbatim, so the block survives it.
2. **After the checkpoint commit, `observe_cycle`** records the committed outcome.

The block has three sections, rendered by `render_memory_block`
([agent/prompt.py](../python/src/lha/agent/prompt.py)):

| Section | Source | Default count |
|---|---|---|
| Earlier attempts and outcomes | `cycle_outcome` events of this item, then the mission's most recent other outcomes | `LHA_MEMORY_EPISODIC_K=4` for this item, plus `k // 2` others (at least 1) |
| Skills that worked before | verified, unexpired skills in this repo's namespace or `global` | `LHA_MEMORY_SKILLS_K=2` |
| Related facts, progress, decisions and code | hybrid retrieval (below) | `LHA_MEMORY_SEMANTIC_K=4` |

The whole block is capped at `LHA_MEMORY_PROMPT_BUDGET_CHARS` (default 4000 characters, roughly
1000 tokens; `build_messages` also enforces an absolute cap of 20000). The budget is split
0.4 / 0.2 / 0.4 across the three sections; what an empty section leaves unused goes to the
sections after it. Lines keep their relevance order and are clipped, never reordered. With nothing
to show, no block is added and the prompt is exactly what it was without memory. Rendering and
retrieval are deterministic: the same store contents and checkout give the same block.

Each episode line gives the cycle, item, verdict, attempt number and status (`in_progress`,
`done`, `blocked`, or `split` when the replanner replaced the item with sub-items), the tools used, the
files changed in that commit (from `git diff --name-only`), and either "what worked" (the lead's
done summary on a verified cycle) or "claimed" (the done summary) plus the verification failure.

## What is recorded

`observe_cycle` runs after the checkpoint, so a memory failure can never undo or block a commit:

- **Episodic**: appends a `cycle_outcome` event (item, verdict, verified, status, attempts, tools,
  files changed, done summary, failure report tail, head sha) to `episodic_events`.
- **Semantic**: stores a `progress` record for the cycle in `semantic_memory` (with its vector
  when the dense channel is on).
- **Procedural**: when the item was VERIFIED, stores a skill: the item description, the done
  summary, the tools and the files changed, `verified=True`, provenance
  `<mission>:<cycle>:<head>`, expiring after 90 days. The namespace is the checkout's absolute
  path, so a later mission in the same workspace can recall it. Failed cycles never become
  skills, and the store refuses unverified skills (`SkillNotVerifiedError`).
- **Consolidation**: see below.

The git anchor's `.lha/events.ndjson` is still written by the checkpoint as before; the memory
plane does not read it.

## Hybrid retrieval

The semantic section ranks these candidates for the query (the item description):

- persisted records of this mission: `progress` notes and consolidated `fact`s (the newest 500
  valid ones). Progress notes of the active item are skipped, since the episodic section already
  shows them;
- the anchor's five most recent decisions (`.lha/decisions.ndjson`);
- when `LHA_MEMORY_INDEX_REPO_FILES=true` (default), the checkout's tracked text files
  (`git ls-files`, excluding `.lha/`, files up to 100 KB, the first `LHA_MEMORY_MAX_REPO_FILES=400`
  paths in sorted order) split into 40-line chunks;
- in lexical-only mode, `git grep -i -F` hits for the query's terms (one record per file, most
  matches first, top 10 files).

Rankings are fused with reciprocal rank fusion (`reciprocal_rank_fusion`, score = sum of
`1/(60 + rank)`):

- lexical: `BM25Index` over all candidates;
- dense (hybrid mode only): cosine between the embedder's vectors for the query and the repo
  chunks and decisions (computed in process, cached per run), and the store's dense index for the
  persisted records — `SqliteSemanticIndex` (vectors stored as JSON, exact cosine) on SQLite,
  `PgSemanticIndex` (pgvector, scoped to the mission) on Postgres;
- `git grep` (lexical-only mode only).

The fused list is then reranked (`NoopReranker` keeps fusion order; `LHA_MEMORY_RERANK=cross_encoder`
uses `CrossEncoderReranker`, which needs the `embeddings` extra and falls back to `NoopReranker`
with a warning if it cannot be built).

`HybridRetriever` in [hybrid.py](../python/src/lha/memory/hybrid.py) is not used by the service,
which combines `BM25Index` and `reciprocal_rank_fusion` itself because its dense side is split
between the store and in-process vectors. `InMemorySemanticIndex`, `InMemorySkillStore` and
`InMemoryEpisodicLog` are also not used at runtime; they remain libraries with tests.

Skills are ranked the same way: BM25 over `name + description`, plus embedder cosine in hybrid
mode, fused with RRF.

## Embedders

[embeddings.py](../python/src/lha/memory/embeddings.py). `LHA_MEMORY_EMBEDDER` picks the dense
channel:

| Setting | Embedder | `dim` | Notes |
|---|---|---|---|
| `hash` (default) | `HashEmbedder`, `hash` / `1` | 256 on SQLite, 1024 on Postgres | Hashed bag of tokens, normalized. Lexical, not semantic, but needs no extra, network or key |
| `ollama` | `OllamaEmbedder`, `ollama:<model>` / `<model>@<digest>` | the model's (measured) | A real semantic embedder served by a local [Ollama](https://ollama.com) at `LHA_OLLAMA_BASE_URL` (`POST /api/embed`): $0, no Python extra. `LHA_MEMORY_EMBEDDING_MODEL`, default `nomic-embed-text` (768 wide); pull it first (`ollama pull nomic-embed-text`). Unreachable or not pulled: lexical-only |
| `sentence_transformers` | `SentenceTransformerEmbedder`, `st:<model>` / `<model>` | the model's | A real local semantic embedder in the worker process (`LHA_MEMORY_EMBEDDING_MODEL`, default `BAAI/bge-m3`). Needs the `embeddings` extra; without it, retrieval falls back to lexical-only |
| `none` | none | — | Lexical-only (BM25 + `git grep`) by choice |

The default stays `hash` because it needs nothing running: with `ollama` and no Ollama server,
retrieval is lexical-only (BM25 + `git grep`, no dense channel at all), which recalls less than
`hash`'s hashed-token cosine. Choose `ollama` when an Ollama server with the model is available
to every process that runs cycles (the worker, or the machine running `lha mission`).

When the memory plane opens (once per run, and once per cycle in the durable activity),
`OllamaEmbedder.connect` checks `GET /api/tags` for the model, takes its digest into the version
and embeds a probe text to learn `dim`. Each call embeds up to 64 texts. The repository chunks
are embedded again each time the plane opens (vectors are cached only for the run), so on a
large checkout expect each durable cycle to spend time embedding with a local model.

`VoyageEmbedder` (`voyage:<model>`, 1024, calls the Voyage API with httpx) exists but no setting
selects it.

On Postgres, an embedder narrower than the `vector(1024)` column (for example
`nomic-embed-text`, 768) is wrapped in `PaddedEmbedder`, which appends zeros up to 1024. Zeros
change neither dot products nor norms, so cosine similarity is unchanged, and the HNSW index
works as for a 1024-wide model. An embedder wider than 1024 cannot be stored: retrieval is
lexical-only.

## Version gating

Every `MemoryRecord` carries `embedding_model` and `embedding_version`. The SQLite index, the
pgvector index and `InMemorySemanticIndex` only compare vectors from the same (model, version) as
the current embedder. After an embedder change, old rows are still returned lexically (BM25) but
not by the dense channel. The Ollama version includes the model's digest, so re-pulling a model
whose weights changed is an embedder change too. Re-embedding is not implemented.

`PgSemanticIndex` ([semantic_pg.py](../python/src/lha/memory/semantic_pg.py)) details:

- `semantic_memory.embedding` is `vector(1024)` with an HNSW cosine index; the embedder's `dim`
  must equal it, otherwise construction raises `EmbeddingDimensionError`. The memory service
  pads a narrower embedder to 1024 when it opens (`PaddedEmbedder`, above) and uses
  lexical-only retrieval for a wider one;
- `add()` upserts by id and writes `kind` and `metadata` (migration 0004); with `mission_id` set,
  rows are stamped with it and `query()` only sees that mission's rows;
- `query()` filters on `valid`, non-null embeddings and the current model and version, and orders
  by `embedding <=> %s::vector`;
- the `tsv` column and its GIN index exist in the schema, but nothing writes `tsv` (BM25 runs in
  process).

## Consolidation

Every `LHA_MEMORY_CONSOLIDATE_EVERY` (default 5; 0 disables) `cycle_outcome` events recorded
since the last pass, `observe_cycle` consolidates the new episodes into semantic `fact` records,
then soft-invalidates the progress notes they cover (`valid=false`; never deleted), and appends a
`memory_consolidation` event holding the watermark (`upto_id`). The count lives in the store, so
it spans Temporal activity attempts and process restarts.

- `LHA_MEMORY_CONSOLIDATION=extractive` (default): deterministic and free. One fact per item,
  rebuilt from all of that item's episodes so far ("verified in c5 after 3 attempt(s); what
  worked: ...; last failure: ...; files: ..."), upserted under a stable id so a newer pass
  replaces the older fact.
- `LHA_MEMORY_CONSOLIDATION=model`: `consolidate()` ([consolidation.py](../python/src/lha/memory/consolidation.py))
  sends the rendered episodes to the lead's provider, wrapped in the mission's `CostMeter` with
  role `librarian`, so the calls are budget-checked and land in the cost ledger. A batch whose
  reply cannot be parsed is kept verbatim (`failed_batches` in the event). If the call fails (for
  example `BudgetExceeded`), a `memory_error` is recorded, the watermark does not move, and the
  next cycle retries.

## Degradation

[ops/degradation.py](../python/src/lha/ops/degradation.py): `postgres`, `pgvector` and
`embeddings` are OPTIONAL dependencies (with `langfuse` and `egress_proxy`), so
`decide_safe_park` never parks for them. `decide_memory_mode` returns hybrid retrieval only when
all of the dense-channel dependencies are OK; otherwise lexical-only retrieval: BM25 plus
`git grep`. The service applies it:

| Condition | Detected | Result |
|---|---|---|
| `LHA_POSTGRES_DSN` set but Postgres unreachable, unmigrated or `psycopg` missing | the store falls back to SQLite (`degraded_reason`) | lexical-only |
| `LHA_MEMORY_EMBEDDER=ollama` and Ollama unreachable at `LHA_OLLAMA_BASE_URL`, the model not pulled, or the probe embedding fails | at open | lexical-only |
| `LHA_MEMORY_EMBEDDER=sentence_transformers` without the `embeddings` extra | at open | lexical-only |
| `LHA_MEMORY_EMBEDDER=none` | at open | lexical-only |
| Postgres store but `pgvector` not importable, or embedder `dim` is wider than 1024 | at open | lexical-only |
| the embedder (for example Ollama stops answering) or the vector index raises later in the run | at the failing call | lexical-only for the rest of the run |

Each case logs a structlog warning with the reason (and, on the local run paths, which have a
`TraceRecorder`, a `memory_degraded` trace event), and the mission continues. Any other memory error is recorded as `memory_error` and the cycle
proceeds with less or no memory; memory never fails a cycle. `probe_health` (the parked-mission
health check) still checks only `git`, `model` and `sandbox`.

## Persistence

Tables (SQLite mirrors of [db/migrations](../db/migrations/), see [Cost and budget](10-cost-and-budget.md#cost-ledger)
for the store itself):

| Table | Used for |
|---|---|
| `episodic_events` | `cycle_outcome` and `memory_consolidation` events |
| `semantic_memory` | `progress` and `fact` records; `kind` and `metadata` columns added by migration `0004_memory_skills` |
| `skills` | verified skills (migration `0004_memory_skills`) |

## Extras

| Component | Requires |
|---|---|
| Memory on SQLite, `HashEmbedder`, BM25, `git grep`, `NoopReranker`, extractive consolidation, skills | core install (and `git`) |
| Memory on Postgres with pgvector | `postgres` extra (`psycopg[binary,pool]`, `pgvector`), a Postgres with the `vector` extension, `lha db migrate` |
| `OllamaEmbedder` | core install (httpx) + a running Ollama with the embedding model pulled |
| `SentenceTransformerEmbedder`, `CrossEncoderReranker` | `embeddings` extra (`sentence-transformers`) |
| `VoyageEmbedder` | core install (httpx) + Voyage API key, passed in code |
| Go: everything above except `SentenceTransformerEmbedder`, `CrossEncoderReranker` and `VoyageEmbedder` | the `lha` binary (and `git`); Postgres needs no client extra |

Related: [Configuration](18-configuration.md), [Operations runbook](15-operations-runbook.md).
