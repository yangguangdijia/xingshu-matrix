CREATE TABLE IF NOT EXISTS cluster_registry_changes (
    id          BIGSERIAL PRIMARY KEY,
    env         VARCHAR(64) NOT NULL,
    action      VARCHAR(16) NOT NULL,
    cluster_id BIGINT,
    cluster     VARCHAR(128) NOT NULL,
    applicant_id BIGINT NOT NULL,
    payload_enc TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + interval '24 hours',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cluster_registry_changes_expiry_idx
    ON cluster_registry_changes (expires_at);
