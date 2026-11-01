-- CICD migration 6: execution targets reference registered clusters by id.
-- New targets carry cluster_id and no kubeconfig; legacy rows keep their
-- encrypted kubeconfig until an operator links them to a registered cluster.
-- No ON DELETE action: a cluster still used by a target cannot be deleted.

ALTER TABLE cicd_targets
    ADD COLUMN IF NOT EXISTS cluster_id BIGINT REFERENCES cluster_clusters(id);

CREATE INDEX IF NOT EXISTS cicd_targets_cluster_idx ON cicd_targets (cluster_id);

ALTER TABLE cicd_targets ALTER COLUMN kubeconfig_enc DROP NOT NULL;

ALTER TABLE cicd_targets DROP CONSTRAINT IF EXISTS cicd_targets_cluster_or_kubeconfig_check;
ALTER TABLE cicd_targets ADD CONSTRAINT cicd_targets_cluster_or_kubeconfig_check
    CHECK ((cluster_id IS NOT NULL) <> (COALESCE(kubeconfig_enc, '') <> ''));
