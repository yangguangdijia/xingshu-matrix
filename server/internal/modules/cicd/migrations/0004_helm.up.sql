-- CICD migration 4: Helm repositories and target environment binding.
-- Repositories are explicitly configured by an administrator; there are no
-- built-in public/test chart repositories.

ALTER TABLE cicd_targets ADD COLUMN IF NOT EXISTS env TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS cicd_helm_repos (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    url          TEXT        NOT NULL,
    username     TEXT        NOT NULL DEFAULT '',
    password_enc TEXT        NOT NULL DEFAULT '',
    env          TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cicd_helm_repos_env_idx ON cicd_helm_repos (env);

CREATE TABLE IF NOT EXISTS cicd_helm_operations (
    id          BIGSERIAL PRIMARY KEY,
    repo_id     BIGINT      NOT NULL REFERENCES cicd_helm_repos(id) ON DELETE RESTRICT,
    target_id   BIGINT      NOT NULL REFERENCES cicd_targets(id) ON DELETE RESTRICT,
    env         TEXT        NOT NULL DEFAULT '',
    release     TEXT        NOT NULL,
    namespace   TEXT        NOT NULL,
    chart       TEXT        NOT NULL,
    version     TEXT        NOT NULL DEFAULT '',
    action      TEXT        NOT NULL DEFAULT 'upgrade',
    revision    INT         NOT NULL DEFAULT 0,
    values_enc  TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'awaiting_approval',
    message     TEXT        NOT NULL DEFAULT '',
    log         TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    approval_id BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS cicd_helm_operations_env_idx ON cicd_helm_operations (env, id DESC);
