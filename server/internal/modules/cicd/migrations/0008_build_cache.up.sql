-- CICD migration 8: build cache.
--   cicd_pipelines.cache : pipeline-level cache configuration (dependency cache
--                          on a PVC / sandbox hostPath, Kaniko layer cache).
--   cicd_runs.cache      : what this run actually used and observed (hit/miss,
--                          cache key, usage reported by the cache container,
--                          Kaniko layer cache hits). NULL = cache not used.
ALTER TABLE cicd_pipelines ADD COLUMN IF NOT EXISTS cache JSONB;
ALTER TABLE cicd_runs ADD COLUMN IF NOT EXISTS cache JSONB;
