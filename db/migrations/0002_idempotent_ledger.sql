-- 0002: idempotent cost ledger + text ids for semantic memory. Safe to re-run.

-- Every cost_ledger row carries a caller-derived idempotency key; a retried/replayed write of the
-- same logical model call is a no-op (INSERT ... ON CONFLICT (idempotency_key) DO NOTHING).
ALTER TABLE cost_ledger ADD COLUMN IF NOT EXISTS idempotency_key text;
ALTER TABLE cost_ledger ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT '';
ALTER TABLE cost_ledger ADD COLUMN IF NOT EXISTS cost_known boolean NOT NULL DEFAULT true;
CREATE UNIQUE INDEX IF NOT EXISTS cost_ledger_idempotency_key ON cost_ledger (idempotency_key);

-- Semantic-memory record ids are harness ids like 'fact_3f9a...' (not UUIDs).
ALTER TABLE semantic_memory ALTER COLUMN id TYPE text USING id::text;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0002_idempotent_ledger') ON CONFLICT DO NOTHING;
