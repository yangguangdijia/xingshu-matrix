CREATE TABLE IF NOT EXISTS cluster_integration_discovery (
    cluster_id          BIGINT PRIMARY KEY REFERENCES cluster_clusters(id) ON DELETE CASCADE,
    cluster_updated_at  TIMESTAMPTZ NOT NULL,
    status              VARCHAR(16) NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'running', 'ready', 'partial', 'error')),
    snapshot            JSONB NOT NULL DEFAULT '{}'::jsonb,
    discovered_at       TIMESTAMPTZ,
    next_refresh_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until         TIMESTAMPTZ,
    generation          BIGINT NOT NULL DEFAULT 0,
    attempts            INTEGER NOT NULL DEFAULT 0,
    last_error          VARCHAR(128) NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cluster_integration_discovery_due_idx
    ON cluster_integration_discovery (next_refresh_at, cluster_id);

-- Existing registrations are discovered after the migration, without
-- rewriting cluster credentials or creating observability data sources.
INSERT INTO cluster_integration_discovery (cluster_id, cluster_updated_at)
SELECT id, updated_at FROM cluster_clusters WHERE true ON CONFLICT (cluster_id) DO NOTHING;
