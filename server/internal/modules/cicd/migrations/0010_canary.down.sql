-- Rollback of 0010_canary. Canary resources already created in clusters are
-- not touched; roll back or promote running canaries before applying this.
DROP INDEX IF EXISTS cicd_canaries_lease_idx;
ALTER TABLE cicd_canaries DROP COLUMN IF EXISTS pending_action;
ALTER TABLE cicd_canaries DROP COLUMN IF EXISTS canary_ingress;
ALTER TABLE cicd_canaries DROP COLUMN IF EXISTS canary_service;
ALTER TABLE cicd_canaries DROP COLUMN IF EXISTS canary_deployment;
