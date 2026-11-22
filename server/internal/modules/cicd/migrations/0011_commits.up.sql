-- CICD migration 11: keep push commit messages (sha/message/author/url, newest 20) on webhook
-- deliveries and the runs they triggered, for the devcenter value stream (REQ-/BUG- keys).
ALTER TABLE cicd_runs ADD COLUMN IF NOT EXISTS commits JSONB NOT NULL DEFAULT '[]';
ALTER TABLE cicd_webhook_deliveries ADD COLUMN IF NOT EXISTS commits JSONB NOT NULL DEFAULT '[]';
