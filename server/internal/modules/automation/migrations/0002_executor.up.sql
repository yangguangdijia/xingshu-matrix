-- Automation executor support: per-step records plus a worker lease so a
-- crashed worker cannot leave a run stuck in "running" forever.
ALTER TABLE automation_runs ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE automation_runs ADD COLUMN IF NOT EXISTS worker TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS automation_run_steps (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT      NOT NULL REFERENCES automation_runs(id) ON DELETE CASCADE,
    step_index  INTEGER     NOT NULL CHECK (step_index >= 0),
    name        TEXT        NOT NULL DEFAULT '',
    type        TEXT        NOT NULL,
    status      TEXT        NOT NULL CHECK (status IN ('running','succeeded','failed','skipped')),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    output      JSONB,
    error       TEXT        NOT NULL DEFAULT '',
    UNIQUE (run_id, step_index)
);
CREATE INDEX IF NOT EXISTS automation_run_steps_run_idx ON automation_run_steps (run_id, step_index);
CREATE INDEX IF NOT EXISTS automation_runs_lease_idx ON automation_runs (status, lease_until);
