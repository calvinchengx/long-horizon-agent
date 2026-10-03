-- 0006: the shared event record (docs/27-mission-ui.md). Every run path of every implementation
-- appends the trace events it emits, so a reader (lha serve, lha mission-report, SQL) can follow a
-- run without its logs. A reader pages forward by id; spec/state/mission_events.json pins the kinds.

CREATE TABLE IF NOT EXISTS mission_events (
    id              bigserial PRIMARY KEY,
    mission_id      text NOT NULL,
    cycle_id        text NOT NULL DEFAULT '',
    ts              timestamptz NOT NULL DEFAULT now(),
    kind            text NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}',
    schema_version  int NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS mission_events_mission ON mission_events (mission_id, id);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     text PRIMARY KEY,
    applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations (version) VALUES ('0006_mission_events') ON CONFLICT DO NOTHING;
