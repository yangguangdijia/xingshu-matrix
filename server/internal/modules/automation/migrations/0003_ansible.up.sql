ALTER TABLE automation_runs
    ADD COLUMN IF NOT EXISTS env TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS payload_enc TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approval_id BIGINT,
    ADD COLUMN IF NOT EXISTS logs JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE automation_workflow_versions
    ADD COLUMN IF NOT EXISTS playbook TEXT NOT NULL DEFAULT '';

ALTER TABLE automation_runs DROP CONSTRAINT IF EXISTS automation_runs_status_check;
ALTER TABLE automation_runs ADD CONSTRAINT automation_runs_status_check
    CHECK (status IN ('queued','awaiting_approval','running','cancelling','succeeded','failed','cancelled'));

CREATE INDEX IF NOT EXISTS automation_runs_env_idx ON automation_runs (env, id DESC);

CREATE TABLE IF NOT EXISTS automation_ansible_preflights (
    token_hash TEXT PRIMARY KEY,
    env TEXT NOT NULL,
    created_by BIGINT NOT NULL,
    payload_enc TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS automation_ansible_preflights_expiry_idx
    ON automation_ansible_preflights (expires_at);
