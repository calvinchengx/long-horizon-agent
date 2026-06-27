# Memory

The memory plane has three tiers: episodic (what happened), semantic (distilled facts retrieved
by similarity) and procedural (verified skills). It also has retrieval components: hybrid lexical
and dense search, and a reranker. Code: [`python/src/lha/memory/`](../python/src/lha/memory/),
contracts in [contracts/memory.py](../python/src/lha/contracts/memory.py).

**Current integration status.** Only the git anchor's event log is written during a mission. The
semantic index, hybrid retriever, reranker, consolidation and skill store are libraries with
tests. The agent loop, the orchestrator, the durable activities and the CLI do not call them, so a
running mission neither stores nor retrieves semantic memories or skills. There is also no
setting that selects a memory backend.

## Tiers

| Tier | Durable record | In-process component |
|---|---|---|
| Episodic | `.lha/events.ndjson` in the mission anchor, committed with each checkpoint ([Mission anchor](06-mission-anchor.md)); Postgres table `episodic_events` exists in the schema but nothing writes it | `InMemoryEpisodicLog` ([episodic.py](../python/src/lha/memory/episodic.py)): `append` and `tail(n)` |
| Semantic | Postgres `semantic_memory` via `PgSemanticIndex` | `InMemorySemanticIndex` |
| Procedural | none | `InMemorySkillStore` |

## Embedders

[embeddings.py](../python/src/lha/memory/embeddings.py):

| Embedder | `name` / `version` | `dim` | Notes |
|---|---|---|---|
| `HashEmbedder` | `hash` / `1` | 256 by default | Hashed bag of tokens, normalized. Lexical, not semantic. Offline, $0, for dev and CI |
| `VoyageEmbedder` | `voyage:<model>` / `<model>` | 1024 by default | Calls `https://api.voyageai.com/v1/embeddings` with httpx. Needs an API key passed in code (there is no setting for it) |

## Semantic index and version gating

Every `MemoryRecord` carries `embedding_model` and `embedding_version`. Queries only compare
vectors from the same (model, version) as the current embedder, so a query never compares
vectors from different embedder versions.

- **`InMemorySemanticIndex`** ([semantic_memory.py](../python/src/lha/memory/semantic_memory.py)):
  - exact cosine similarity;
  - stamps records with the embedder's name and version on insert;
  - re-adding an id replaces the record;
  - optional `max_records` evicts the oldest records first;
  - `invalidate(ids)` sets `valid=False`, and invalid records are never returned;
  - `cosine()` raises on a length mismatch instead of returning 0.
- **`PgSemanticIndex`** ([semantic_pg.py](../python/src/lha/memory/semantic_pg.py)):
  - `semantic_memory` has `embedding vector(1024)` with an HNSW cosine index and an index on
    (`embedding_model`, `embedding_version`);
  - the embedder's `dim` must equal the column width (1024 by default), otherwise construction
    raises `EmbeddingDimensionError`, so `HashEmbedder()` at 256 is rejected unless built with
    `dim=1024`;
  - `add()` upserts by id; `query()` filters on `valid` and the current model and version, and
    orders by `embedding <=> %s::vector`;
  - it has no `invalidate` or `remove` method;
  - the `tsv` column and its GIN index exist in the schema, but nothing writes `tsv`.

Re-embedding after an embedder change is not implemented. Rows from the old version remain and
are ignored by queries from the new embedder.

## Hybrid retrieval

`HybridRetriever` ([hybrid.py](../python/src/lha/memory/hybrid.py)) combines a dense
`SemanticIndex` with `BM25Index`:

- `BM25Index` is an in-memory Okapi BM25 over lower-cased `[a-z0-9]+` tokens (k1 = 1.5,
  b = 0.75, idf = `log(1 + (N - df + 0.5)/(df + 0.5))`).
- `query(text, k=5, candidate_k=20)` takes the top `candidate_k` from each side and fuses them
  with reciprocal rank fusion. The score is the sum of `1/(60 + rank)`, so no score calibration is
  needed.
- Only records known to this retriever instance and still `valid` are returned. The retriever keeps
  its own in-process id-to-record map, so dense hits for ids it was never given (for example
  Postgres rows from an earlier process) are dropped.
- `add()` de-duplicates by id and writes to both sides. `invalidate()` marks records invalid,
  removes them from BM25, and forwards to the dense index if it supports `invalidate`.
  `max_records` evicts the oldest from both sides if the dense index supports `remove`.

## Reranking

[rerank.py](../python/src/lha/memory/rerank.py):

- `NoopReranker` keeps the fusion order.
- `CrossEncoderReranker` (default `BAAI/bge-reranker-v2-m3`, via `sentence_transformers`) scores
  (query, text) pairs in a worker thread and re-sorts.

`HybridRetriever` does not call a reranker. The caller applies it to the fused hits.

## Consolidation

`consolidate(model, episodes, mission_id, batch_size=200)`
([consolidation.py](../python/src/lha/memory/consolidation.py)) sends episodes to the model in
batches and asks for a JSON array of facts. Each fact becomes a semantic `MemoryRecord`
(`fact_<id>`). If a batch's reply cannot be parsed, its distinct episodes are kept verbatim and
tagged `metadata["consolidation"] = "verbatim"`, and `failed_batches` is incremented. Nothing is
dropped. `soft_invalidate(records, predicate)` sets `valid=False` and never deletes or rewrites.
Consolidation calls `model.complete` directly, so it is budget-checked only if the caller passes a
`MeteredModel`. No code schedules consolidation, for example during a park.

## Skills library

`InMemorySkillStore` ([skills.py](../python/src/lha/memory/skills.py)) stores `Skill` records.
Each has a name, description, code, preconditions, namespace, provenance, `expires_at`,
`verified` and `uses`.

- `add()` raises `SkillNotVerifiedError` unless `verified=True`. The store does not run tests
  itself; the caller sets the flag.
- `find(query, k, namespace)` ranks skills by description similarity under the store's
  embedder and, when a namespace is given, keeps skills from that namespace or `global`.
- `expires_at` and `preconditions` are stored but not checked by the store.

## Extras

| Component | Requires |
|---|---|
| In-memory index, BM25, hybrid, `HashEmbedder`, `NoopReranker`, consolidation, skills | core install |
| `VoyageEmbedder` | core install (httpx) + Voyage API key |
| `PgSemanticIndex` | `postgres` extra (`psycopg[binary,pool]`, `pgvector`), a Postgres with the `vector` extension, `lha db migrate` |
| `CrossEncoderReranker` | `embeddings` extra (`sentence-transformers`) |

## Degradation when pgvector is unavailable

[ops/degradation.py](../python/src/lha/ops/degradation.py) classifies `pgvector` (with `langfuse`
and `egress_proxy`) as an optional dependency. `decide_safe_park` does not park for an optional
dependency that is down. It only lists it in `degraded`. That is all the code does:

- `probe_health` checks only `git`, `model` and `sandbox`. It never reports on `pgvector`.
- No code catches a `PgSemanticIndex` failure and falls back to lexical search or `git grep`.
  `HybridRetriever.query()` does not catch errors from the dense side, so a database error
  propagates to the caller.

Since no run path uses semantic memory, a Postgres outage does not affect a running mission today.

Related: [Configuration](18-configuration.md), [Operations runbook](15-operations-runbook.md).
