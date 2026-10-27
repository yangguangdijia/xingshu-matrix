CREATE TABLE IF NOT EXISTS provision_jobs (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    env TEXT NOT NULL,
    plan JSONB NOT NULL,
    request_enc TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'awaiting_approval',
    logs JSONB NOT NULL DEFAULT '[]',
    error TEXT NOT NULL DEFAULT '',
    cluster_id BIGINT,
    created_by BIGINT NOT NULL,
    created_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS provision_jobs_queue ON provision_jobs(status,id);
CREATE UNIQUE INDEX IF NOT EXISTS provision_jobs_active_name ON provision_jobs(name)
 WHERE status IN ('awaiting_approval','queued','running','cancelling');
CREATE TABLE IF NOT EXISTS provision_host_locks (
    ip TEXT PRIMARY KEY,
    job_id BIGINT NOT NULL REFERENCES provision_jobs(id) ON DELETE CASCADE
);
