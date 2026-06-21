-- LHA initial schema (Postgres 16 + pgvector). Apply against the app DB (LHA_POSTGRES_DSN).
-- Every evolvable row carries schema_version; every vector carries its embedding model+version
-- so cosine is never compared across versions.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS missions (
    mission_id          text PRIMARY KEY,
    title               text NOT NULL,
    description         text NOT NULL DEFAULT '',
    acceptance          text NOT NULL DEFAULT '',
    status              text NOT NULL DEFAULT 'RUNNING',  -- RUNNING|SLEEPING|WAITING_ON_HUMAN|DEGRADED_PARK|DONE|ABORTED|IMPOSSIBLE
    workflow_id         text,
    run_id              text,
    latest_snapshot_id  text,
    latest_session_id   text,
    head_sha            text,
    schema_version      int NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS checklist_items (
    mission_id      text NOT NULL,
    item_id         text NOT NULL,
    description     text NOT NULL,
    status          text NOT NULL DEFAULT 'todo',
    verified_by     jsonb NOT NULL DEFAULT '[]',
    depends_on      jsonb NOT NULL DEFAULT '[]',
    attempts        int NOT NULL DEFAULT 0,
    schema_version  int NOT NULL DEFAULT 1,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mission_id, item_id)
);

CREATE TABLE IF NOT EXISTS episodic_events (
    id              bigserial PRIMARY KEY,
    mission_id      text NOT NULL,
    cycle_id        text NOT NULL DEFAULT '',
    ts              timestamptz NOT NULL DEFAULT now(),
    kind            text NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}',
    payload_ref     text,
    schema_version  int NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS episodic_events_mission_ts ON episodic_events (mission_id, ts);

CREATE TABLE IF NOT EXISTS semantic_memory (
    id                 uuid PRIMARY KEY,
    mission_id         text,
    text               text NOT NULL,
    tsv                tsvector,
    embedding          vector(1024),
    embedding_model    text NOT NULL,
    embedding_version  text NOT NULL,
    valid              boolean NOT NULL DEFAULT true,
    source_event_id    bigint,
    created_at         timestamptz NOT NULL DEFAULT now(),
    schema_version     int NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS semantic_memory_tsv ON semantic_memory USING gin (tsv);
CREATE INDEX IF NOT EXISTS semantic_memory_hnsw ON semantic_memory USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS semantic_memory_modelver ON semantic_memory (embedding_model, embedding_version);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key         text PRIMARY KEY,
    result_ref  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cost_ledger (
    id              bigserial PRIMARY KEY,
    mission_id      text NOT NULL,
    cycle_id        text NOT NULL DEFAULT '',
    ts              timestamptz NOT NULL DEFAULT now(),
    model           text NOT NULL,
    input_tokens    int NOT NULL DEFAULT 0,
    output_tokens   int NOT NULL DEFAULT 0,
    usd             numeric(12, 6) NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS cost_ledger_mission_ts ON cost_ledger (mission_id, ts);

CREATE TABLE IF NOT EXISTS hitl_gates (
    gate_id        text PRIMARY KEY,
    mission_id     text NOT NULL,
    question       text NOT NULL,
    risk           text NOT NULL,
    default_action text NOT NULL,
    status         text NOT NULL DEFAULT 'OPEN',  -- OPEN|RESOLVED|DEFAULTED|ESCALATED
    deadline       timestamptz,
    decision       text,
    resolved_by    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    resolved_at    timestamptz
);

CREATE TABLE IF NOT EXISTS snapshots (
    snapshot_id  text PRIMARY KEY,
    mission_id   text NOT NULL,
    sandbox_kind text NOT NULL,
    meta         jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS schema_registry (
    artifact    text NOT NULL,
    version     int NOT NULL,
    applied_at  timestamptz NOT NULL DEFAULT now(),
    notes       text,
    PRIMARY KEY (artifact, version)
);

INSERT INTO schema_registry (artifact, version, notes)
VALUES ('lha-core', 1, 'initial schema')
ON CONFLICT DO NOTHING;

-- Migration bookkeeping (also created by lha.persistence.db.apply_migrations). Recording the
-- version here too means a DB initialized via docker-entrypoint-initdb.d counts as migrated.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0001_init') ON CONFLICT DO NOTHING;
