CREATE TABLE IF NOT EXISTS cluster_clusters (
    id           BIGSERIAL PRIMARY KEY,
    name         VARCHAR(128) NOT NULL UNIQUE,
    env          VARCHAR(64)  NOT NULL DEFAULT '',
    description  TEXT         NOT NULL DEFAULT '',
    api_server   VARCHAR(512) NOT NULL DEFAULT '',
    kubeconfig   TEXT         NOT NULL,           -- AES-256-GCM envelope (d.Crypto)
    version      VARCHAR(64)  NOT NULL DEFAULT '',
    status       VARCHAR(32)  NOT NULL DEFAULT 'unknown',
    last_error   TEXT         NOT NULL DEFAULT '',
    checked_at   TIMESTAMPTZ,
    created_by   VARCHAR(128) NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cluster_clusters_env_idx ON cluster_clusters (env);
