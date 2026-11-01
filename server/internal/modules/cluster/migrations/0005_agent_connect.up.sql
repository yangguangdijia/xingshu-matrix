ALTER TABLE cluster_clusters
    ADD COLUMN IF NOT EXISTS connect_mode       VARCHAR(16)  NOT NULL DEFAULT 'kubeconfig',
    ADD COLUMN IF NOT EXISTS agent_token_hash   VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS agent_fingerprint  VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS agent_version      VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS agent_connected_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS agent_seen_at      TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cluster_clusters_connect_mode_check') THEN
        ALTER TABLE cluster_clusters ADD CONSTRAINT cluster_clusters_connect_mode_check
            CHECK (connect_mode IN ('kubeconfig', 'agent'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cluster_clusters_credentials_check') THEN
        ALTER TABLE cluster_clusters ADD CONSTRAINT cluster_clusters_credentials_check
            CHECK ((connect_mode = 'kubeconfig' AND kubeconfig <> '') OR (connect_mode = 'agent' AND kubeconfig = ''));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS cluster_clusters_agent_token_idx
    ON cluster_clusters (agent_token_hash) WHERE agent_token_hash <> '';

-- Shared secrets for server replicas (e.g. the tunnel relay HMAC key), created once.
CREATE TABLE IF NOT EXISTS cluster_agent_keys (
    name        VARCHAR(64) PRIMARY KEY,
    secret_enc  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which server replica currently holds each cluster's agent tunnel.
CREATE TABLE IF NOT EXISTS cluster_agent_sessions (
    cluster_id   BIGINT PRIMARY KEY REFERENCES cluster_clusters(id) ON DELETE CASCADE,
    session_id   VARCHAR(64)  NOT NULL,
    owner_addr   VARCHAR(256) NOT NULL DEFAULT '',
    secret_enc   TEXT         NOT NULL,
    claimed_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    seen_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
