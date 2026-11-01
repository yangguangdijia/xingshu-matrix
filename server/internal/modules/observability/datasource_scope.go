package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type clusterIdentity struct {
	ID   int64
	Name string
	Env  string
}

type sourceScope struct {
	Cluster clusterIdentity
	Central bool
	Label   string
	Value   string
}

var clusterLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,127}$`)

func optionString(opts map[string]any, key string) string {
	if opts == nil {
		return ""
	}
	switch v := opts[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func scopeKind(opts map[string]any) string {
	scope := optionString(opts, "scope")
	if scope == "" && (optionString(opts, "cluster_id") != "" || optionString(opts, "cluster_name") != "") {
		return "cluster" // explicit legacy ownership only
	}
	return scope
}

func (m *Module) clusterByID(ctx context.Context, p *rbac.Principal, id int64) (clusterIdentity, error) {
	var cl clusterIdentity
	if id <= 0 {
		return cl, httpx.Validation("cluster_id must be a positive integer")
	}
	err := m.d.DB.QueryRow(ctx, `SELECT id,name,env FROM cluster_clusters WHERE id=$1`, id).Scan(&cl.ID, &cl.Name, &cl.Env)
	if err == pgx.ErrNoRows {
		return cl, httpx.NotFound("cluster not found")
	}
	if err != nil {
		return cl, err
	}
	if p == nil || !p.CanEnv(cl.Env) {
		return clusterIdentity{}, httpx.Forbidden("no access to cluster environment")
	}
	return cl, nil
}

func (m *Module) clusterByName(ctx context.Context, p *rbac.Principal, name string) (clusterIdentity, error) {
	var cl clusterIdentity
	err := m.d.DB.QueryRow(ctx, `SELECT id,name,env FROM cluster_clusters WHERE name=$1`, name).Scan(&cl.ID, &cl.Name, &cl.Env)
	if err == pgx.ErrNoRows {
		return cl, httpx.NotFound("cluster not found")
	}
	if err != nil {
		return cl, err
	}
	if p == nil || !p.CanEnv(cl.Env) {
		return clusterIdentity{}, httpx.Forbidden("no access to cluster environment")
	}
	return cl, nil
}

func (m *Module) clusterFromOptions(ctx context.Context, p *rbac.Principal, opts map[string]any) (clusterIdentity, error) {
	idText, name := optionString(opts, "cluster_id"), optionString(opts, "cluster_name")
	if idText == "" && name == "" {
		return clusterIdentity{}, httpx.Validation("cluster datasource requires cluster_id or cluster_name")
	}
	if idText != "" {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			return clusterIdentity{}, httpx.Validation("invalid cluster_id")
		}
		cl, err := m.clusterByID(ctx, p, id)
		if err != nil {
			return cl, err
		}
		if name != "" && name != cl.Name {
			return clusterIdentity{}, httpx.Validation("cluster_id and cluster_name disagree")
		}
		return cl, nil
	}
	return m.clusterByName(ctx, p, name)
}

func centralValues(opts map[string]any) (map[string]string, error) {
	raw, ok := opts["cluster_values"].(map[string]any)
	if !ok {
		if values, valid := opts["cluster_values"].(map[string]string); valid {
			raw = make(map[string]any, len(values))
			for k, v := range values {
				raw[k] = v
			}
		}
	}
	if len(raw) == 0 {
		return nil, httpx.Validation("central datasource requires cluster_values")
	}
	values := make(map[string]string, len(raw))
	for ref, v := range raw {
		label, ok := v.(string)
		if !ok || strings.TrimSpace(ref) == "" || strings.TrimSpace(label) == "" || len(label) > 256 {
			return nil, httpx.Validation("cluster_values must map cluster IDs or names to non-empty label values")
		}
		values[ref] = label
	}
	return values, nil
}

func (m *Module) validateDSScope(ctx context.Context, p *rbac.Principal, in *dsInput) error {
	if optionString(in.Options, "scope") == "" && scopeKind(in.Options) == "cluster" {
		in.Options["scope"] = "cluster" // safe legacy input with an explicit owner
	}
	switch optionString(in.Options, "scope") {
	case "cluster":
		cl, err := m.clusterFromOptions(ctx, p, in.Options)
		if err != nil {
			return err
		}
		if in.Env != cl.Env {
			return httpx.Validation("datasource environment and cluster environment disagree")
		}
		if _, exists := in.Options["cluster_values"]; exists {
			return httpx.Validation("cluster datasource cannot declare central cluster_values")
		}
		in.Options["cluster_id"] = strconv.FormatInt(cl.ID, 10)
		in.Options["cluster_name"] = cl.Name
	case "central":
		if optionString(in.Options, "cluster_id") != "" || optionString(in.Options, "cluster_name") != "" {
			return httpx.Validation("central datasource cannot bind one cluster")
		}
		label := optionString(in.Options, "cluster_label")
		if !clusterLabelName.MatchString(label) {
			return httpx.Validation("central datasource requires a valid cluster_label")
		}
		values, err := centralValues(in.Options)
		if err != nil {
			return err
		}
		canonical := map[string]string{}
		labelOwners := map[string]string{}
		for ref, value := range values {
			var cl clusterIdentity
			if id, parseErr := strconv.ParseInt(ref, 10, 64); parseErr == nil {
				cl, err = m.clusterByID(ctx, p, id)
			} else {
				cl, err = m.clusterByName(ctx, p, ref)
			}
			if err != nil {
				return err
			}
			if in.Env != "" && in.Env != cl.Env {
				return httpx.Validation("central mapping crosses datasource environment")
			}
			id := strconv.FormatInt(cl.ID, 10)
			if prior, exists := canonical[id]; exists && prior != value {
				return httpx.Validation("conflicting central cluster_values")
			}
			if owner := labelOwners[value]; owner != "" && owner != id {
				return httpx.Validation("central cluster label values must be unique")
			}
			labelOwners[value] = id
			canonical[id] = value
		}
		in.Options["cluster_values"] = canonical
	case "":
		// Legacy explicitly owned sources remain readable, but new writes must opt in.
		return httpx.Validation("datasource options.scope must be cluster or central")
	default:
		return httpx.Validation("datasource options.scope must be cluster or central")
	}
	return nil
}

// resolveSourceScope authorizes the requested cluster and checks ownership, not just environment.
func (m *Module) resolveSourceScope(ctx context.Context, p *rbac.Principal, ds *DataSource, requested string) (sourceScope, error) {
	var out sourceScope
	if p == nil {
		return out, httpx.Unauthorized("authentication required")
	}
	kind := scopeKind(ds.Options)
	if kind != "cluster" && kind != "central" {
		return out, httpx.Validation("datasource has no explicit cluster ownership")
	}
	if kind == "cluster" {
		cl, err := m.clusterFromOptions(ctx, p, ds.Options)
		if err != nil {
			return out, err
		}
		if ds.Env != cl.Env {
			return out, httpx.Forbidden("datasource and cluster environments disagree")
		}
		if requested != "" && requested != strconv.FormatInt(cl.ID, 10) {
			return out, httpx.Forbidden("datasource does not belong to the requested cluster")
		}
		out.Cluster = cl
	} else {
		if requested == "" {
			return out, httpx.Validation("cluster_id is required for central datasource")
		}
		id, err := strconv.ParseInt(requested, 10, 64)
		if err != nil {
			return out, httpx.Validation("invalid cluster_id")
		}
		cl, err := m.clusterByID(ctx, p, id)
		if err != nil {
			return out, err
		}
		if ds.Env != "" && ds.Env != cl.Env {
			return out, httpx.Forbidden("central datasource does not serve this environment")
		}
		label := optionString(ds.Options, "cluster_label")
		values, err := centralValues(ds.Options)
		if err != nil || !clusterLabelName.MatchString(label) {
			return out, httpx.Validation("central datasource cluster mapping is invalid")
		}
		value := values[strconv.FormatInt(cl.ID, 10)]
		if nameValue := values[cl.Name]; nameValue != "" {
			if value != "" && value != nameValue {
				return out, httpx.Validation("central cluster mapping is ambiguous")
			}
			value = nameValue
		}
		if value == "" {
			return out, httpx.Forbidden("central datasource has no mapping for this cluster")
		}
		for otherRef, otherValue := range values {
			if otherValue == value && otherRef != cl.Name && otherRef != strconv.FormatInt(cl.ID, 10) {
				return out, httpx.Validation("central cluster label value is not unique")
			}
		}
		out = sourceScope{Cluster: cl, Central: true, Label: label, Value: value}
	}
	return out, nil
}

func (s sourceScope) checkLabels(labels map[string]string) bool {
	if s.Central && labels[s.Label] != s.Value {
		return false
	}
	if value := labels["cluster_id"]; value != "" && value != strconv.FormatInt(s.Cluster.ID, 10) &&
		!(s.Central && s.Label == "cluster_id" && value == s.Value) {
		return false
	}
	if value := labels["cluster"]; value != "" && value != s.Cluster.Name && !(s.Central && s.Label == "cluster" && value == s.Value) {
		return false
	}
	return true
}

func (s sourceScope) String() string {
	return fmt.Sprintf("cluster %d (%s)", s.Cluster.ID, s.Cluster.Name)
}

// selectSource resolves one source deterministically. An unowned legacy source
// is never interpreted as a shared source merely because it is the only one.
func (m *Module) selectSource(ctx context.Context, p *rbac.Principal, typ, clusterID string) (*DataSource, sourceScope, error) {
	var empty sourceScope
	id, err := strconv.ParseInt(clusterID, 10, 64)
	if err != nil {
		return nil, empty, httpx.Validation("cluster_id is required")
	}
	cl, err := m.clusterByID(ctx, p, id)
	if err != nil {
		return nil, empty, err
	}
	rows, err := m.d.DB.Query(ctx, `SELECT `+dsCols+` FROM obs_datasources WHERE type=$1 AND (env=$2 OR env='') ORDER BY id`, typ, cl.Env)
	if err != nil {
		return nil, empty, err
	}
	var candidates []*DataSource
	for rows.Next() {
		ds, scanErr := m.scanDS(rows)
		if scanErr != nil {
			rows.Close()
			return nil, empty, scanErr
		}
		candidates = append(candidates, ds)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, empty, err
	}
	rows.Close()
	var dedicated, shared *DataSource
	var dedicatedScope, sharedScope sourceScope
	dedicatedCount, sharedCount := 0, 0
	for _, ds := range candidates {
		scope, err := m.resolveSourceScope(ctx, p, ds, clusterID)
		if err != nil {
			continue // malformed/unowned/unrelated sources cannot win resolution
		}
		if scope.Central {
			sharedCount++
			shared, sharedScope = ds, scope
		} else {
			dedicatedCount++
			dedicated, dedicatedScope = ds, scope
		}
	}
	if dedicatedCount > 1 {
		return nil, empty, httpx.Conflict("multiple cluster datasources serve this cluster")
	}
	if dedicated != nil {
		return dedicated, dedicatedScope, nil
	}
	if sharedCount > 1 {
		return nil, empty, httpx.Conflict("multiple central datasources serve this cluster")
	}
	if shared != nil {
		return shared, sharedScope, nil
	}
	return nil, empty, httpx.NotFound("no datasource is bound to this cluster")
}

func (m *Module) resolveDS(c *gin.Context) {
	typ := strings.TrimSpace(c.Query("type"))
	if !dsTypes[typ] {
		httpx.Fail(c, httpx.Validation("valid datasource type is required"))
		return
	}
	ds, scope, err := m.selectSource(c, rbac.PrincipalFrom(c), typ, c.Query("cluster_id"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	visible := *ds
	if scope.Central {
		visible.Options = make(map[string]any, len(ds.Options))
		for key, value := range ds.Options {
			visible.Options[key] = value
		}
		visible.Options["cluster_values"] = map[string]string{strconv.FormatInt(scope.Cluster.ID, 10): scope.Value}
	}
	httpx.OK(c, gin.H{"datasource": &visible, "cluster_id": scope.Cluster.ID, "scope": map[bool]string{true: "central", false: "cluster"}[scope.Central]})
}
