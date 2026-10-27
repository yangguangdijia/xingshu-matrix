ALTER TABLE cicd_jenkins
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE TABLE IF NOT EXISTS cicd_config_changes (
    id          BIGSERIAL PRIMARY KEY,
    payload_enc TEXT NOT NULL,
    ticket_env  TEXT NOT NULL DEFAULT '',
    ready       BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cicd_config_changes_created_idx ON cicd_config_changes (created_at);
