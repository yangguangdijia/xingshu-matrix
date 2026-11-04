-- CICD migration 7: reusable credentials (Git token / basic / SSH key /
-- image registry). Secrets are encrypted with the platform key; applications
-- reference a Git credential and build stages reference a registry
-- credential by id. No ON DELETE action: a credential in use cannot be deleted.

CREATE TABLE IF NOT EXISTS cicd_credentials (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL CHECK (type IN ('git_token','git_basic','ssh_key','registry')),
    username    TEXT NOT NULL DEFAULT '',
    secret_enc  TEXT NOT NULL,
    host        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE cicd_applications
    ADD COLUMN IF NOT EXISTS credential_id BIGINT REFERENCES cicd_credentials(id);
