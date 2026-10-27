ALTER TABLE obs_datasources DROP CONSTRAINT IF EXISTS obs_datasources_type_check;
ALTER TABLE obs_datasources ADD CONSTRAINT obs_datasources_type_check
 CHECK (type IN ('prometheus','loki','elasticsearch','alertmanager','jaeger','tempo','kubernetes'));

CREATE TABLE IF NOT EXISTS obs_diagnoses (
    id BIGSERIAL PRIMARY KEY,
    alert_id BIGINT NOT NULL REFERENCES obs_alerts(id) ON DELETE CASCADE,
    occurrence INT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    evidence JSONB NOT NULL DEFAULT '{}',
    analysis JSONB NOT NULL DEFAULT '{}',
    target JSONB NOT NULL DEFAULT '{}',
    decision_by BIGINT,
    decision_name TEXT NOT NULL DEFAULT '',
    approval_id BIGINT,
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(alert_id,occurrence)
);
CREATE INDEX IF NOT EXISTS obs_diagnoses_queue ON obs_diagnoses(status,id);
