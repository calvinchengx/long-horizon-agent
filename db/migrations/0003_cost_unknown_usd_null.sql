-- 0003: an unpriced model call's cost is UNKNOWN, not $0. Store usd = NULL for those rows so
-- SUM(usd) is "known spend" and unknown-cost calls are countable (cost_known = false).
-- Safe to re-run.
ALTER TABLE cost_ledger ALTER COLUMN usd DROP NOT NULL;
ALTER TABLE cost_ledger ALTER COLUMN usd DROP DEFAULT;
UPDATE cost_ledger SET usd = NULL WHERE NOT cost_known AND usd IS NOT NULL;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0003_cost_unknown_usd_null') ON CONFLICT DO NOTHING;
