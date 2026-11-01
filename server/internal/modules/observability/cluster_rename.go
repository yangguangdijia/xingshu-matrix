package observability

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// renameClusterReferences keeps datasource ownership in step with a cluster
// rename. It runs inside the cluster module's rename transaction.
func renameClusterReferences(ctx context.Context, tx pgx.Tx, id int64, oldName, newName string) error {
	idText := strconv.FormatInt(id, 10)
	if _, err := tx.Exec(ctx, `UPDATE obs_datasources
		SET options = jsonb_set(options, '{cluster_name}', to_jsonb($2::text)), updated_at = now()
		WHERE jsonb_typeof(options) = 'object' AND options->>'cluster_id' = $1 AND options ? 'cluster_name'`,
		idText, newName); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, options->'cluster_values' FROM obs_datasources
		WHERE jsonb_typeof(options->'cluster_values') = 'object' AND options->'cluster_values' ? $1
		FOR UPDATE`, oldName)
	if err != nil {
		return err
	}
	type pending struct {
		id     int64
		values map[string]any
	}
	var updates []pending
	for rows.Next() {
		var dsID int64
		var raw []byte
		if err := rows.Scan(&dsID, &raw); err != nil {
			rows.Close()
			return err
		}
		values := map[string]any{}
		if err := json.Unmarshal(raw, &values); err != nil {
			rows.Close()
			return err
		}
		renamed, err := renameCentralValue(values, idText, oldName, newName)
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, pending{id: dsID, values: renamed})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		encoded, err := json.Marshal(u.values)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE obs_datasources
			SET options = jsonb_set(options, '{cluster_values}', $2::jsonb), updated_at = now() WHERE id = $1`,
			u.id, encoded); err != nil {
			return err
		}
	}
	return nil
}

// renameCentralValue moves a name-keyed central mapping to the new name. The
// mapped label value is what the telemetry carries, so it never changes here.
func renameCentralValue(values map[string]any, idText, oldName, newName string) (map[string]any, error) {
	value, ok := values[oldName]
	if !ok {
		return values, nil
	}
	if existing, taken := values[newName]; taken && existing != value {
		return nil, httpx.Conflict("中央数据源已存在名为 " + newName + " 的集群映射，请先调整数据源映射")
	}
	if byID, has := values[idText]; has && byID != value {
		return nil, httpx.Conflict("中央数据源中该集群的 ID 与名称映射不一致，请先调整数据源映射")
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		if k != oldName {
			out[k] = v
		}
	}
	out[newName] = value
	return out, nil
}
