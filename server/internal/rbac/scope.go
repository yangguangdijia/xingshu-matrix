package rbac

import (
	"sort"
	"strconv"
	"strings"
)

// Data scopes (v1.29): KubeSphere-style 企业空间 → 项目 → 成员 on top of the
// existing permission + environment model.
//
// Compatibility contract:
//   - A principal whose Scopes is nil, or whose Scopes.Restricted is false, sees
//     exactly what it saw before data scopes existed (global permissions plus
//     the old environment scope). Every user holding at least one global role
//     binding (user_roles, or a role_bindings row with scope global/env) is
//     unrestricted.
//   - Only users whose access comes solely from cluster / workspace / project
//     bindings or workspace membership are restricted. Their permissions are
//     limited to the scope-aware resources below, and data lists are filtered.

// Scope types of a role binding.
const (
	ScopeGlobal    = "global"
	ScopeEnv       = "env"
	ScopeCluster   = "cluster"
	ScopeWorkspace = "workspace"
	ScopeProject   = "project"
)

// Workspace member roles, highest first.
const (
	WorkspaceOwner  = "owner"
	WorkspaceAdmin  = "admin"
	WorkspaceMember = "member"
	WorkspaceViewer = "viewer"
)

// ValidScopeType reports whether s is a known binding scope.
func ValidScopeType(s string) bool {
	switch s {
	case ScopeGlobal, ScopeEnv, ScopeCluster, ScopeWorkspace, ScopeProject:
		return true
	}
	return false
}

// ValidWorkspaceRole reports whether s is a known workspace member role.
func ValidWorkspaceRole(s string) bool { return workspaceRoleRank(s) > 0 }

func workspaceRoleRank(s string) int {
	switch s {
	case WorkspaceOwner:
		return 4
	case WorkspaceAdmin:
		return 3
	case WorkspaceMember:
		return 2
	case WorkspaceViewer:
		return 1
	}
	return 0
}

// HigherWorkspaceRole returns the stronger of two workspace roles.
func HigherWorkspaceRole(a, b string) string {
	if workspaceRoleRank(b) > workspaceRoleRank(a) {
		return b
	}
	return a
}

// ScopedPermissions are the only permissions a restricted (scope-only) user can
// hold: resources whose lists and writes are filtered by cluster / namespace.
// Anything else (hosts, bastion, settings, node operations…) stays global-only
// so a scoped binding can never widen access to unfiltered modules.
var ScopedPermissions = []string{
	"cluster:read",
	"namespace:read", "namespace:write",
	"workload:read", "workload:write",
	"network:write",
	"storage:read", "storage:write",
	"config:read", "config:write",
	"cicd:read",
	"workspace:read",
}

var workspaceRolePerms = map[string][]string{
	WorkspaceViewer: {"cluster:read", "namespace:read", "workload:read", "storage:read", "config:read", "cicd:read", "workspace:read"},
	WorkspaceMember: {"cluster:read", "namespace:read", "workload:read", "storage:read", "config:read", "cicd:read", "workspace:read",
		"workload:write", "config:write", "network:write", "storage:write"},
	WorkspaceAdmin: {"cluster:read", "namespace:read", "workload:read", "storage:read", "config:read", "cicd:read", "workspace:read",
		"workload:write", "config:write", "network:write", "storage:write", "namespace:write"},
}

// WorkspaceRolePermissions returns the permissions a workspace role implies
// inside the workspace's projects.
func WorkspaceRolePermissions(role string) []string {
	if role == WorkspaceOwner {
		role = WorkspaceAdmin
	}
	return append([]string(nil), workspaceRolePerms[role]...)
}

// FilterScopedPermissions keeps the subset of ScopedPermissions matched by the
// granted codes (wildcards such as "*" or "*:read" are expanded).
func FilterScopedPermissions(granted []string) []string {
	out := []string{}
	for _, p := range ScopedPermissions {
		if Match(granted, p) {
			out = append(out, p)
		}
	}
	return out
}

// NamespaceRef identifies one project (namespace of a cluster).
type NamespaceRef struct {
	ClusterID int64  `json:"cluster_id"`
	Namespace string `json:"namespace"`
}

// String renders the project scope value "<cluster>/<namespace>".
func (n NamespaceRef) String() string { return strconv.FormatInt(n.ClusterID, 10) + "/" + n.Namespace }

// ParseNamespaceRef parses a project scope value "<cluster>/<namespace>".
func ParseNamespaceRef(s string) (NamespaceRef, bool) {
	c, ns, ok := strings.Cut(s, "/")
	if !ok {
		return NamespaceRef{}, false
	}
	id, err := strconv.ParseInt(c, 10, 64)
	if err != nil || id <= 0 || !ValidNamespace(ns) {
		return NamespaceRef{}, false
	}
	return NamespaceRef{ClusterID: id, Namespace: ns}, true
}

// ValidNamespace reports whether s is a DNS-1123 label (Kubernetes namespace).
func ValidNamespace(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// ScopeGrant is a set of permissions that applies inside one data scope.
type ScopeGrant struct {
	Type        string         `json:"type"` // cluster | workspace | project
	ClusterID   int64          `json:"cluster_id,omitempty"`
	WorkspaceID int64          `json:"workspace_id,omitempty"`
	Namespaces  []NamespaceRef `json:"namespaces,omitempty"`
	Permissions []string       `json:"permissions"`
	Source      string         `json:"source"`
}

func (g *ScopeGrant) coversCluster(id int64) bool { return g.Type == ScopeCluster && g.ClusterID == id }

func (g *ScopeGrant) coversNamespace(clusterID int64, ns string) bool {
	if g.coversCluster(clusterID) {
		return true
	}
	for _, n := range g.Namespaces {
		if n.ClusterID == clusterID && n.Namespace == ns {
			return true
		}
	}
	return false
}

// DataScope is the resolved data range of a principal.
type DataScope struct {
	// Restricted is false for users with any global binding (legacy behaviour).
	Restricted bool         `json:"restricted"`
	Grants     []ScopeGrant `json:"grants,omitempty"`
	// Workspaces maps workspace id → strongest member role (direct or via group).
	Workspaces map[int64]string `json:"workspaces,omitempty"`
}

// Restricted reports whether the principal's data is limited by scopes.
func (p *Principal) Restricted() bool { return p != nil && p.Scopes != nil && p.Scopes.Restricted }

// CanCluster reports whether the principal may see cluster id at all (whole
// cluster, or at least one project in it). Environment scope is checked
// separately with CanEnv.
func (p *Principal) CanCluster(id int64) bool {
	if p == nil {
		return false
	}
	if !p.Restricted() {
		return true
	}
	for i := range p.Scopes.Grants {
		g := &p.Scopes.Grants[i]
		if g.coversCluster(id) {
			return true
		}
		for _, n := range g.Namespaces {
			if n.ClusterID == id {
				return true
			}
		}
	}
	return false
}

// HasClusterWide reports whether the principal can see every namespace of cluster id.
func (p *Principal) HasClusterWide(id int64) bool {
	if p == nil {
		return false
	}
	if !p.Restricted() {
		return true
	}
	for i := range p.Scopes.Grants {
		if p.Scopes.Grants[i].coversCluster(id) {
			return true
		}
	}
	return false
}

// CanNamespace reports whether the principal may see namespace ns of cluster clusterID.
func (p *Principal) CanNamespace(clusterID int64, ns string) bool {
	if p == nil {
		return false
	}
	if !p.Restricted() {
		return true
	}
	for i := range p.Scopes.Grants {
		if p.Scopes.Grants[i].coversNamespace(clusterID, ns) {
			return true
		}
	}
	return false
}

// CanIn reports whether the principal holds perm inside namespace ns of cluster
// clusterID. Unrestricted principals fall back to the global check.
func (p *Principal) CanIn(perm string, clusterID int64, ns string) bool {
	if p == nil {
		return false
	}
	if !p.Restricted() {
		return p.Can(perm)
	}
	for i := range p.Scopes.Grants {
		g := &p.Scopes.Grants[i]
		if g.coversNamespace(clusterID, ns) && Match(g.Permissions, perm) {
			return true
		}
	}
	return false
}

// WorkspaceRole returns the principal's member role in workspace id ("" = none).
func (p *Principal) WorkspaceRole(id int64) string {
	if p == nil || p.Scopes == nil {
		return ""
	}
	return p.Scopes.Workspaces[id]
}

// CanWorkspace reports whether the principal may see workspace id: platform
// workspace managers and unrestricted readers see all, everybody else only the
// workspaces they belong to.
func (p *Principal) CanWorkspace(id int64) bool {
	if p == nil {
		return false
	}
	if p.Can("workspace:manage") || (!p.Restricted() && p.Can("workspace:read")) {
		return true
	}
	return p.WorkspaceRole(id) != ""
}

// CanManageWorkspace reports whether the principal may change members and
// projects of workspace id (platform manager, or workspace owner/admin).
func (p *Principal) CanManageWorkspace(id int64) bool {
	if p == nil {
		return false
	}
	if p.Can("workspace:manage") {
		return true
	}
	r := p.WorkspaceRole(id)
	return r == WorkspaceOwner || r == WorkspaceAdmin
}

// AllowedNamespaces returns (true, nil) when every namespace of the cluster is
// visible, otherwise the sorted list of visible namespaces.
func (p *Principal) AllowedNamespaces(clusterID int64) (all bool, namespaces []string) {
	if p.HasClusterWide(clusterID) {
		return true, nil
	}
	if p == nil {
		return false, []string{}
	}
	set := map[string]bool{}
	for i := range p.Scopes.Grants {
		for _, n := range p.Scopes.Grants[i].Namespaces {
			if n.ClusterID == clusterID {
				set[n.Namespace] = true
			}
		}
	}
	namespaces = make([]string, 0, len(set))
	for ns := range set {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return false, namespaces
}

// VisibleScope returns the clusters visible whole and the individual projects
// visible to a restricted principal (both empty for an unrestricted one).
func (p *Principal) VisibleScope() (clusters []int64, projects []NamespaceRef) {
	if !p.Restricted() {
		return nil, nil
	}
	seenC := map[int64]bool{}
	seenN := map[NamespaceRef]bool{}
	for i := range p.Scopes.Grants {
		g := &p.Scopes.Grants[i]
		if g.Type == ScopeCluster && !seenC[g.ClusterID] {
			seenC[g.ClusterID] = true
			clusters = append(clusters, g.ClusterID)
		}
		for _, n := range g.Namespaces {
			if !seenN[n] {
				seenN[n] = true
				projects = append(projects, n)
			}
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i] < clusters[j] })
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].ClusterID != projects[j].ClusterID {
			return projects[i].ClusterID < projects[j].ClusterID
		}
		return projects[i].Namespace < projects[j].Namespace
	})
	return clusters, projects
}

// VisibleClusterIDs returns every cluster a restricted principal can see.
func (p *Principal) VisibleClusterIDs() []int64 {
	clusters, projects := p.VisibleScope()
	set := map[int64]bool{}
	for _, c := range clusters {
		set[c] = true
	}
	for _, n := range projects {
		set[n.ClusterID] = true
	}
	out := make([]int64, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
