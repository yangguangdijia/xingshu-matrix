UPDATE obs_datasources AS ds
SET options = ds.options || jsonb_build_object(
        'scope', 'cluster',
        'cluster_id', cl.id::text,
        'cluster_name', cl.name
    ),
    updated_at = now()
FROM cluster_clusters AS cl
WHERE ds.env = cl.env
  AND btrim(ds.env) <> ''
  AND btrim(ds.name) <> ''
  AND btrim(ds.url) <> ''
  AND ds.url ~* '^https?://[^[:space:]/?#]+'
  AND ds.url !~ '[[:space:]]'
  AND jsonb_typeof(ds.options) = 'object'
  AND NULLIF(btrim(ds.options->>'scope'), '') IS NULL
  AND NULLIF(btrim(ds.options->>'cluster_id'), '') IS NULL
  AND NULLIF(btrim(ds.options->>'cluster_name'), '') IS NULL
  AND NOT (ds.options ?| ARRAY[
      'cluster_label',
      'cluster_values',
      'shared',
      'is_shared',
      'cluster_mapping',
      'cluster_mappings'
  ])
  AND NOT EXISTS (
      SELECT 1
      FROM cluster_clusters AS other_cluster
      WHERE other_cluster.env = ds.env
        AND other_cluster.id <> cl.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM obs_datasources AS sibling
      WHERE sibling.id <> ds.id
        AND rtrim(sibling.url, '/') = rtrim(ds.url, '/')
  );
