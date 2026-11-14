-- Rollback of 0009_delivery_safety (reverse order of the up migration).
-- Drops canary releases and their events, webhook delivery history and image
-- scan results. Data in these tables is lost.
DROP INDEX IF EXISTS cicd_canary_events_idx;
DROP TABLE IF EXISTS cicd_canary_events;

DROP INDEX IF EXISTS cicd_canaries_active_uniq;
DROP INDEX IF EXISTS cicd_canaries_due_idx;
DROP INDEX IF EXISTS cicd_canaries_app_idx;
DROP TABLE IF EXISTS cicd_canaries;

DROP INDEX IF EXISTS cicd_webhook_deliveries_received_idx;
DROP INDEX IF EXISTS cicd_webhook_deliveries_app_idx;
DROP INDEX IF EXISTS cicd_webhook_deliveries_uniq;
DROP TABLE IF EXISTS cicd_webhook_deliveries;

DROP INDEX IF EXISTS cicd_image_scans_run_idx;
DROP TABLE IF EXISTS cicd_image_scans;
