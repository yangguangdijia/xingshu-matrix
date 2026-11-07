package rbac

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// scopeCacheTTL bounds how long a membership / binding change may take to apply
// on other replicas; local changes invalidate immediately.
const scopeCacheTTL = 15 * time.Second

// BindingRow is one role binding as loaded for scope resolution.
type BindingRow struct {
	Scope       string
	ScopeValue  string
	RoleCode    string
	Permissions []string
}

// MemberRow is one workspace membership (direct or through a user group).
type MemberRow struct {
	WorkspaceID int64
	Role        string
}

// ProjectRow is one project assigned to a workspace.
type ProjectRow struct {
	WorkspaceID int64
	ClusterID   int64
	Namespace   string
}

// ScopeInput is everything BuildScopedPrincipal needs; it is loaded from the
// database by Service.loadScopeInput and is pure data so it can be unit tested.
type ScopeInput struct {
	Bindings []BindingRow
	Members  []MemberRow
	Projects []ProjectRow
	// ClusterEnvs maps a cluster id to its environment (for restricted users,
	// whose env scope becomes the environments of their visible clusters).
	ClusterEnvs map[int64]string
}

type scopeEntry struct {
	in  *ScopeInput // nil = nothing configured for this user
	exp time.Time
}

type scopeCache struct {
	mu         sync.Mutex
	configured *bool
	confExp    time.Time
	users      map[int64]scopeEntry
}

// InvalidateScopes drops cached data scopes (membership / binding changes).
func (s *Service) InvalidateScopes() {
	s.scopes.mu.Lock()
	s.scopes.configured = nil
	s.scopes.users = nil
	s.scopes.mu.Unlock()
}

// BuildScopedPrincipal returns a copy of base with role bindings and workspace
// memberships applied. base is never mutated (auth caches it). With an empty
// input the copy is identical to base apart from Scopes staying nil.
func BuildScopedPrincipal(base *Principal, in *ScopeInput) *Principal {
	if base == nil {
		return nil
	}
	cp := *base
	if in == nil || (len(in.Bindings) == 0 && len(in.Members) == 0) {
		return &cp
	}
	// Token principals keep their own permission ceiling: bindings never add
	// permissions beyond what the token already carries.
	tokenCeiling := cp.ViaToken()

	perms := map[string]bool{}
	for _, p := range cp.Permissions {
		perms[p] = true
	}
	envAll := cp.EnvScope == nil && len(cp.Roles) > 0
	envSet := map[string]bool{}
	for _, e := range cp.EnvScope {
		envSet[e] = true
	}
	hasGlobal := len(cp.Roles) > 0

	wsRole := map[int64]string{}
	for _, m := range in.Members {
		wsRole[m.WorkspaceID] = HigherWorkspaceRole(wsRole[m.WorkspaceID], m.Role)
	}
	projectsOf := map[int64][]NamespaceRef{}
	for _, pr := range in.Projects {
		projectsOf[pr.WorkspaceID] = append(projectsOf[pr.WorkspaceID], NamespaceRef{ClusterID: pr.ClusterID, Namespace: pr.Namespace})
	}

	var grants []ScopeGrant
	for _, b := range in.Bindings {
		switch b.Scope {
		case ScopeGlobal:
			hasGlobal, envAll = true, true
			if !tokenCeiling {
				for _, p := range b.Permissions {
					perms[p] = true
				}
			}
		case ScopeEnv:
			hasGlobal = true
			if !tokenCeiling {
				for _, p := range b.Permissions {
					perms[p] = true
				}
			}
			envSet[b.ScopeValue] = true
		case ScopeCluster:
			id, err := strconv.ParseInt(b.ScopeValue, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			grants = append(grants, ScopeGrant{Type: ScopeCluster, ClusterID: id,
				Permissions: FilterScopedPermissions(b.Permissions), Source: "binding:" + b.RoleCode})
		case ScopeWorkspace:
			id, err := strconv.ParseInt(b.ScopeValue, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			if wsRole[id] == "" {
				wsRole[id] = WorkspaceViewer
			}
			grants = append(grants, ScopeGrant{Type: ScopeWorkspace, WorkspaceID: id, Namespaces: projectsOf[id],
				Permissions: FilterScopedPermissions(b.Permissions), Source: "binding:" + b.RoleCode})
		case ScopeProject:
			ref, ok := ParseNamespaceRef(b.ScopeValue)
			if !ok {
				continue
			}
			grants = append(grants, ScopeGrant{Type: ScopeProject, ClusterID: ref.ClusterID, Namespaces: []NamespaceRef{ref},
				Permissions: FilterScopedPermissions(b.Permissions), Source: "binding:" + b.RoleCode})
		}
	}
	for id, role := range wsRole {
		grants = append(grants, ScopeGrant{Type: ScopeWorkspace, WorkspaceID: id, Namespaces: projectsOf[id],
			Permissions: WorkspaceRolePermissions(role), Source: "workspace:" + role})
	}
	sort.SliceStable(grants, func(i, j int) bool {
		if grants[i].Type != grants[j].Type {
			return grants[i].Type < grants[j].Type
		}
		if grants[i].WorkspaceID != grants[j].WorkspaceID {
			return grants[i].WorkspaceID < grants[j].WorkspaceID
		}
		return grants[i].ClusterID < grants[j].ClusterID
	})

	ds := &DataScope{Restricted: !hasGlobal, Workspaces: wsRole}
	if !hasGlobal {
		ds.Grants = grants
		// Restricted: permissions are exactly the union of scoped grants (capped
		// by the token's own permissions for token calls).
		scoped := map[string]bool{}
		for _, g := range grants {
			for _, p := range g.Permissions {
				if !tokenCeiling || Match(cp.Permissions, p) {
					scoped[p] = true
				}
			}
		}
		perms = scoped
		// Environment scope = environments of the visible clusters, so modules
		// that only know env filtering never widen a restricted user's view.
		envAll = false
		envSet = map[string]bool{}
		cp.Scopes = ds
		for _, id := range cp.VisibleClusterIDs() {
			if env, ok := in.ClusterEnvs[id]; ok {
				envSet[env] = true
			}
		}
	}
	cp.Scopes = ds

	cp.Permissions = make([]string, 0, len(perms))
	for p := range perms {
		cp.Permissions = append(cp.Permissions, p)
	}
	sort.Strings(cp.Permissions)
	if envAll {
		cp.EnvScope = nil
	} else {
		cp.EnvScope = make([]string, 0, len(envSet))
		for e := range envSet {
			cp.EnvScope = append(cp.EnvScope, e)
		}
		sort.Strings(cp.EnvScope)
	}
	return &cp
}

// scopesConfigured reports whether any workspace membership or role binding
// exists at all; when none does the middleware is a pure pass-through.
func (s *Service) scopesConfigured(ctx context.Context) (bool, error) {
	now := time.Now()
	s.scopes.mu.Lock()
	if s.scopes.configured != nil && now.Before(s.scopes.confExp) {
		v := *s.scopes.configured
		s.scopes.mu.Unlock()
		return v, nil
	}
	s.scopes.mu.Unlock()
	var v bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_bindings) OR EXISTS(SELECT 1 FROM workspace_members)`).Scan(&v); err != nil {
		return false, err
	}
	s.scopes.mu.Lock()
	s.scopes.configured, s.scopes.confExp = &v, now.Add(scopeCacheTTL)
	s.scopes.mu.Unlock()
	return v, nil
}

// LoadScopeInput loads a user's bindings, memberships and workspace projects.
// It returns nil when the user has neither bindings nor memberships.
func (s *Service) LoadScopeInput(ctx context.Context, userID int64) (*ScopeInput, error) {
	now := time.Now()
	s.scopes.mu.Lock()
	if e, ok := s.scopes.users[userID]; ok && now.Before(e.exp) {
		s.scopes.mu.Unlock()
		return e.in, nil
	}
	s.scopes.mu.Unlock()

	in, err := s.loadScopeInput(ctx, userID)
	if err != nil {
		return nil, err
	}
	s.scopes.mu.Lock()
	if s.scopes.users == nil {
		s.scopes.users = map[int64]scopeEntry{}
	}
	if len(s.scopes.users) > 10000 {
		s.scopes.users = map[int64]scopeEntry{}
	}
	s.scopes.users[userID] = scopeEntry{in: in, exp: now.Add(scopeCacheTTL)}
	s.scopes.mu.Unlock()
	return in, nil
}

func (s *Service) loadScopeInput(ctx context.Context, userID int64) (*ScopeInput, error) {
	groups := []int64{}
	rows, err := s.db.Query(ctx, `SELECT group_id FROM user_group_members WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	in := &ScopeInput{ClusterEnvs: map[int64]string{}}
	rows, err = s.db.Query(ctx, `
		SELECT b.scope, b.scope_value, r.code,
		       COALESCE((SELECT array_agg(permission) FROM role_permissions rp WHERE rp.role_id=r.id), '{}')
		FROM role_bindings b JOIN roles r ON r.id=b.role_id
		WHERE b.user_id=$1 OR b.group_id = ANY($2::bigint[])
		ORDER BY b.id`, userID, groups)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b BindingRow
		if err := rows.Scan(&b.Scope, &b.ScopeValue, &b.RoleCode, &b.Permissions); err != nil {
			rows.Close()
			return nil, err
		}
		in.Bindings = append(in.Bindings, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(ctx, `SELECT workspace_id, role FROM workspace_members WHERE user_id=$1 OR group_id = ANY($2::bigint[])`, userID, groups)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m MemberRow
		if err := rows.Scan(&m.WorkspaceID, &m.Role); err != nil {
			rows.Close()
			return nil, err
		}
		in.Members = append(in.Members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(in.Bindings) == 0 && len(in.Members) == 0 {
		return nil, nil
	}

	wsIDs := []int64{}
	seen := map[int64]bool{}
	for _, m := range in.Members {
		if !seen[m.WorkspaceID] {
			seen[m.WorkspaceID] = true
			wsIDs = append(wsIDs, m.WorkspaceID)
		}
	}
	clusterIDs := []int64{}
	for _, b := range in.Bindings {
		switch b.Scope {
		case ScopeWorkspace:
			if id, err := strconv.ParseInt(b.ScopeValue, 10, 64); err == nil && !seen[id] {
				seen[id] = true
				wsIDs = append(wsIDs, id)
			}
		case ScopeCluster:
			if id, err := strconv.ParseInt(b.ScopeValue, 10, 64); err == nil {
				clusterIDs = append(clusterIDs, id)
			}
		case ScopeProject:
			if ref, ok := ParseNamespaceRef(b.ScopeValue); ok {
				clusterIDs = append(clusterIDs, ref.ClusterID)
			}
		}
	}
	if len(wsIDs) > 0 {
		rows, err = s.db.Query(ctx, `SELECT workspace_id, cluster_id, namespace FROM workspace_projects WHERE workspace_id = ANY($1::bigint[])`, wsIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p ProjectRow
			if err := rows.Scan(&p.WorkspaceID, &p.ClusterID, &p.Namespace); err != nil {
				rows.Close()
				return nil, err
			}
			in.Projects = append(in.Projects, p)
			clusterIDs = append(clusterIDs, p.ClusterID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(clusterIDs) > 0 {
		// cluster_clusters belongs to the cluster module; tolerate its absence
		// (restricted users then simply get no environment and fail closed).
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT to_regclass('cluster_clusters') IS NOT NULL`).Scan(&exists); err == nil && exists {
			rows, err = s.db.Query(ctx, `SELECT id, env FROM cluster_clusters WHERE id = ANY($1::bigint[])`, clusterIDs)
			if err == nil {
				for rows.Next() {
					var id int64
					var env string
					if rows.Scan(&id, &env) == nil {
						in.ClusterEnvs[id] = env
					}
				}
				rows.Close()
			}
		}
	}
	return in, nil
}

// ScopedPrincipal resolves base's data scope (cached) and returns the merged copy.
func (s *Service) ScopedPrincipal(ctx context.Context, base *Principal) (*Principal, error) {
	if base == nil {
		return nil, nil
	}
	ok, err := s.scopesConfigured(ctx)
	if err != nil || !ok {
		return base, err
	}
	in, err := s.LoadScopeInput(ctx, base.UserID)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return base, nil
	}
	return BuildScopedPrincipal(base, in), nil
}

// ScopeMiddleware applies data scopes to the authenticated principal. Mount it
// right after the auth middleware. With no workspaces or scoped bindings
// configured it is a pass-through (one cached EXISTS query per 15s).
func (s *Service) ScopeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		base := PrincipalFrom(c)
		if base == nil {
			c.Next()
			return
		}
		p, err := s.ScopedPrincipal(c.Request.Context(), base)
		if err != nil {
			slog.Default().Warn("rbac: resolve data scope failed", "user_id", base.UserID, "err", err)
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		if p != base {
			SetPrincipal(c, p)
		}
		c.Next()
	}
}
