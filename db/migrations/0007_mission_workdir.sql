-- 0007: where each mission's anchor (.lha/) is, so any reader (lha serve) can find its checklist
-- and history from the store alone (docs/27-mission-ui.md). An absolute path on the host that ran
-- the mission; NULL for rows written before this column existed.

ALTER TABLE missions ADD COLUMN IF NOT EXISTS workdir text;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0007_mission_workdir') ON CONFLICT DO NOTHING;
