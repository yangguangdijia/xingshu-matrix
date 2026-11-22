ALTER TABLE cicd_webhook_deliveries DROP COLUMN IF EXISTS commits;
ALTER TABLE cicd_runs DROP COLUMN IF EXISTS commits;
