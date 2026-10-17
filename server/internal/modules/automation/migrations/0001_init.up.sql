CREATE TABLE IF NOT EXISTS automation_workflows (
 id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', trigger TEXT NOT NULL DEFAULT 'manual', status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')), current_version INTEGER NOT NULL DEFAULT 1, created_by BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS automation_workflows_name_uq ON automation_workflows(name);
CREATE TABLE IF NOT EXISTS automation_workflow_versions (
 id BIGSERIAL PRIMARY KEY, workflow_id BIGINT NOT NULL REFERENCES automation_workflows(id) ON DELETE CASCADE, version INTEGER NOT NULL, steps JSONB NOT NULL, created_by BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(workflow_id,version)
);
CREATE TABLE IF NOT EXISTS automation_runs (
 id BIGSERIAL PRIMARY KEY, workflow_id BIGINT NOT NULL REFERENCES automation_workflows(id) ON DELETE CASCADE, version INTEGER NOT NULL, status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled')), attempt INTEGER NOT NULL DEFAULT 1, triggered_by BIGINT NOT NULL, started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, error TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS automation_runs_status_idx ON automation_runs(status,created_at DESC);
