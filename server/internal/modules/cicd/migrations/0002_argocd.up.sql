-- cicd: Argo CD connections. Tokens are stored encrypted by the module
-- (Deps.Crypto) and never returned by the API.
CREATE TABLE IF NOT EXISTS cicd_argocd (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    url        TEXT NOT NULL,
    token_enc  TEXT NOT NULL DEFAULT '',
    env        TEXT NOT NULL CHECK (env <> ''),
    project    TEXT NOT NULL DEFAULT 'default',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cicd_argocd_env_idx ON cicd_argocd (env, id);
