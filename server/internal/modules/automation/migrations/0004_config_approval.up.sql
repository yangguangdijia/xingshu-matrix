CREATE TABLE IF NOT EXISTS automation_config_changes (
    id          BIGSERIAL PRIMARY KEY,
    payload_enc TEXT NOT NULL,
    ticket_env  TEXT NOT NULL DEFAULT '',
    ready       BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS automation_config_changes_created_idx
    ON automation_config_changes (created_at);
