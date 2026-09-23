-- 0004: tiered memory wiring. Additive only (safe to re-run; existing columns/tables untouched).

-- Semantic-memory rows carry what KIND of memory they are (fact / progress / ...) and a small
-- metadata map, so a retrieved row can be rendered and filtered without a side table.
ALTER TABLE semantic_memory ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'semantic';
ALTER TABLE semantic_memory ADD COLUMN IF NOT EXISTS metadata jsonb NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS semantic_memory_mission ON semantic_memory (mission_id);

-- The procedural tier: verified, reusable skills (admitted only after the deterministic gate).
CREATE TABLE IF NOT EXISTS skills (
    id              text PRIMARY KEY,
    namespace       text NOT NULL DEFAULT 'global',
    name            text NOT NULL,
    description     text NOT NULL,
    code            text NOT NULL DEFAULT '',
    preconditions   jsonb NOT NULL DEFAULT '[]',
    provenance      text NOT NULL DEFAULT '',
    expires_at      text,            -- ISO date; re-validate after this
    verified        boolean NOT NULL DEFAULT false,
    uses            int NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    schema_version  int NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS skills_namespace ON skills (namespace);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0004_memory_skills') ON CONFLICT DO NOTHING;
