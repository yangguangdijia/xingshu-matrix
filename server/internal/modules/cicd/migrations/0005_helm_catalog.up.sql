ALTER TABLE cicd_helm_repos
    ADD COLUMN IF NOT EXISTS last_sync_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_sync_status TEXT NOT NULL DEFAULT 'never',
    ADD COLUMN IF NOT EXISTS last_sync_error TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS cicd_helm_charts (
    repo_id      BIGINT      NOT NULL REFERENCES cicd_helm_repos(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    version      TEXT        NOT NULL,
    app_version  TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, name, version)
);
CREATE INDEX IF NOT EXISTS cicd_helm_charts_name_idx ON cicd_helm_charts (name, version);
