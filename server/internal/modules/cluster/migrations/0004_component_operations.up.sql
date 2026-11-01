CREATE TABLE IF NOT EXISTS cluster_component_operations (
    id                  UUID PRIMARY KEY,
    cluster_id          BIGINT NOT NULL REFERENCES cluster_clusters(id) ON DELETE CASCADE,
    cluster_name        VARCHAR(128) NOT NULL,
    environment         VARCHAR(64) NOT NULL,
    component           VARCHAR(64) NOT NULL,
    namespace           VARCHAR(63) NOT NULL,
    release_name        VARCHAR(53) NOT NULL,
    chart_version       VARCHAR(64) NOT NULL,
    chart_digest        VARCHAR(64) NOT NULL,
    config_encrypted    TEXT NOT NULL,
    status              VARCHAR(24) NOT NULL CHECK (status IN
                        ('preflight', 'pending_approval', 'running', 'succeeded', 'audit_pending', 'unknown', 'failed', 'rejected')),
    helm_status         VARCHAR(32) NOT NULL DEFAULT '',
    approval_id         BIGINT,
    error_message       VARCHAR(256) NOT NULL DEFAULT '',
    log_excerpt         VARCHAR(4096) NOT NULL DEFAULT '',
    created_by          BIGINT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cluster_component_operations_scope_idx
    ON cluster_component_operations (cluster_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS cluster_component_operations_active_release_idx
    ON cluster_component_operations (cluster_id, namespace, release_name)
    WHERE status IN ('preflight', 'pending_approval', 'running', 'audit_pending', 'unknown');
