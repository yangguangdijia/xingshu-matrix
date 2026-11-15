-- CICD migration 10: canary release execution.
-- cicd_canaries / cicd_canary_events were created by 0009_delivery_safety; this
-- migration only adds what the execution engine (canary.go) needs:
--   canary_deployment / canary_service / canary_ingress : names of the canary
--       resources actually created, so rollback / promote clean up exactly
--       those objects even if the naming rule changes later;
--   pending_action : the action waiting on approval_id (start|advance|promote).
-- New status value used by the engine: awaiting_confirm (a step marked manual
-- has reached its weight and waits for an operator to advance or promote).
ALTER TABLE cicd_canaries ADD COLUMN IF NOT EXISTS canary_deployment TEXT NOT NULL DEFAULT '';
ALTER TABLE cicd_canaries ADD COLUMN IF NOT EXISTS canary_service    TEXT NOT NULL DEFAULT '';
ALTER TABLE cicd_canaries ADD COLUMN IF NOT EXISTS canary_ingress    TEXT NOT NULL DEFAULT '';
ALTER TABLE cicd_canaries ADD COLUMN IF NOT EXISTS pending_action    TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS cicd_canaries_lease_idx ON cicd_canaries (status, lease_until);
