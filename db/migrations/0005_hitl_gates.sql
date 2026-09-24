-- 0005: human gates are written to hitl_gates (by the durable notify_gate activity and the local
-- terminal approver). A gate id is unique within a mission, not globally ("deadlock-3" recurs in
-- every mission), so the key becomes (mission_id, gate_id). Nothing wrote this table before 0005.

ALTER TABLE hitl_gates DROP CONSTRAINT IF EXISTS hitl_gates_pkey;
ALTER TABLE hitl_gates ADD PRIMARY KEY (mission_id, gate_id);

ALTER TABLE hitl_gates ADD COLUMN IF NOT EXISTS kind       text NOT NULL DEFAULT '';
ALTER TABLE hitl_gates ADD COLUMN IF NOT EXISTS options    jsonb NOT NULL DEFAULT '[]';
ALTER TABLE hitl_gates ADD COLUMN IF NOT EXISTS request    jsonb;
ALTER TABLE hitl_gates ADD COLUMN IF NOT EXISTS reminders  int NOT NULL DEFAULT 0;
ALTER TABLE hitl_gates ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS hitl_gates_opened ON hitl_gates (created_at);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0005_hitl_gates') ON CONFLICT DO NOTHING;
