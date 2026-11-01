package observability

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type alertSource struct {
	ID    int64
	Scope sourceScope
}

func (m *Module) alertSources(ctx context.Context, p *rbac.Principal, clusterID string, dsID int64) ([]alertSource, error) {
	if clusterID == "" && dsID > 0 {
		ds, err := m.loadDS(ctx, nil, dsID)
		if err != nil {
			return nil, err
		}
		scope, err := m.resolveSourceScope(ctx, p, ds, "")
		if err != nil {
			return nil, err
		}
		clusterID = strconv.FormatInt(scope.Cluster.ID, 10)
	}
	if clusterID == "" {
		return nil, httpx.Validation("cluster_id is required for alert listing")
	}
	var out []alertSource
	for _, typ := range []string{"alertmanager", "kubernetes"} {
		ds, scope, err := m.selectSource(ctx, p, typ, clusterID)
		if err != nil {
			var apiErr *httpx.Error
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && strings.Contains(apiErr.Message, "no datasource") {
				continue
			}
			return nil, err
		}
		if dsID == 0 || ds.ID == dsID {
			out = append(out, alertSource{ID: ds.ID, Scope: scope})
		}
	}
	if len(out) == 0 {
		return nil, httpx.NotFound("no alert source belongs to this cluster")
	}
	return out, nil
}

// alertSourceSQL limits pagination and counts in the database, including
// legacy alerts whose datasource explicitly identifies exactly one cluster.
func alertSourceSQL(sources []alertSource, args *[]any) string {
	var conditions []string
	add := func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
	for _, source := range sources {
		id := add(source.ID)
		clusterName := add(source.Scope.Cluster.Name)
		parts := []string{"a.datasource_id=" + id}
		if !source.Scope.Central || source.Scope.Label != "cluster_id" {
			clusterID := add(strconv.FormatInt(source.Scope.Cluster.ID, 10))
			parts = append(parts, "(NULLIF(a.labels->>'cluster_id','') IS NULL OR a.labels->>'cluster_id'="+clusterID+")")
		}
		if source.Scope.Central {
			label := add(source.Scope.Label)
			value := add(source.Scope.Value)
			parts = append(parts, "a.labels->>"+label+"="+value)
			if source.Scope.Label != "cluster" {
				parts = append(parts, "(NULLIF(a.labels->>'cluster','') IS NULL OR a.labels->>'cluster'="+clusterName+")")
			}
		} else {
			parts = append(parts, "(NULLIF(a.labels->>'cluster','') IS NULL OR a.labels->>'cluster'="+clusterName+")")
		}
		conditions = append(conditions, "("+strings.Join(parts, " AND ")+")")
	}
	return "(" + strings.Join(conditions, " OR ") + ")"
}

func (m *Module) authorizeAlert(ctx context.Context, p *rbac.Principal, a *Alert, clusterID string) error {
	ds, err := m.loadDS(ctx, nil, a.DatasourceID)
	if err != nil {
		return err
	}
	scope, err := m.resolveSourceScope(ctx, p, ds, clusterID)
	if err != nil {
		return err
	}
	selected, _, err := m.selectSource(ctx, p, ds.Type, strconv.FormatInt(scope.Cluster.ID, 10))
	if err != nil {
		return err
	}
	if selected.ID != ds.ID || !scope.checkLabels(a.Labels) {
		return httpx.NotFound("alert not found for current cluster")
	}
	a.Env = scope.Cluster.Env
	return nil
}

// alertCluster resolves the immutable target before running any Kubernetes
// repair. A shared source must identify exactly one cluster from its labels.
func (m *Module) alertCluster(ctx context.Context, a *Alert) (clusterIdentity, error) {
	admin := &rbac.Principal{} // background diagnosis, never used to authorize an HTTP caller
	ds, err := m.loadDS(ctx, nil, a.DatasourceID)
	if err != nil {
		return clusterIdentity{}, err
	}
	ref := a.Labels["cluster_id"]
	if scopeKind(ds.Options) == "central" && optionString(ds.Options, "cluster_label") == "cluster_id" {
		ref = "" // an external label value need not be a Matrix cluster ID
	}
	if scopeKind(ds.Options) == "central" && ref == "" {
		label := optionString(ds.Options, "cluster_label")
		values, err := centralValues(ds.Options)
		if err != nil || a.Labels[label] == "" {
			return clusterIdentity{}, httpx.Validation("alert has no verified cluster mapping")
		}
		for key, value := range values {
			if value != a.Labels[label] {
				continue
			}
			var candidate clusterIdentity
			if id, parseErr := strconv.ParseInt(key, 10, 64); parseErr == nil {
				candidate, err = m.clusterByID(ctx, admin, id)
			} else {
				candidate, err = m.clusterByName(ctx, admin, key)
			}
			if err != nil {
				return clusterIdentity{}, err
			}
			if ref != "" && ref != strconv.FormatInt(candidate.ID, 10) {
				return clusterIdentity{}, httpx.Validation("alert cluster mapping is ambiguous")
			}
			ref = strconv.FormatInt(candidate.ID, 10)
		}
		if ref == "" {
			return clusterIdentity{}, httpx.Validation("alert cluster mapping is missing")
		}
	}
	scope, err := m.resolveSourceScope(ctx, admin, ds, ref)
	if err != nil {
		return clusterIdentity{}, err
	}
	if !scope.checkLabels(a.Labels) {
		return clusterIdentity{}, httpx.Validation("alert labels conflict with datasource cluster mapping")
	}
	selected, _, err := m.selectSource(ctx, admin, ds.Type, strconv.FormatInt(scope.Cluster.ID, 10))
	if err != nil {
		return clusterIdentity{}, err
	}
	if selected.ID != ds.ID {
		return clusterIdentity{}, httpx.Conflict("alert datasource is not selected for this cluster")
	}
	return scope.Cluster, nil
}
