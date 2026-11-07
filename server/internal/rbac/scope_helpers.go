package rbac

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// ---------------------------------------------------------------- SQL filters

// NamespaceSQL returns a WHERE fragment limiting rows whose (clusterCol, nsCol)
// identify a project to the caller's data scope. Placeholders start at $next.
// Unrestricted callers get "TRUE" and no arguments, so the legacy query plan is
// unchanged; a restricted caller with nothing visible gets "FALSE".
//
//	where, args := rbac.NamespaceSQL(p, "t.cluster_id", "t.namespace", len(args)+1)
func NamespaceSQL(p *Principal, clusterCol, nsCol string, next int) (string, []any) {
	if !p.Restricted() {
		return "TRUE", nil
	}
	clusters, projects := p.VisibleScope()
	if len(clusters) == 0 && len(projects) == 0 {
		return "FALSE", nil
	}
	ids := make([]int64, len(projects))
	nss := make([]string, len(projects))
	for i, n := range projects {
		ids[i], nss[i] = n.ClusterID, n.Namespace
	}
	if clusters == nil {
		clusters = []int64{}
	}
	frag := fmt.Sprintf("(%s = ANY($%d::bigint[]) OR (%s, %s) IN (SELECT c, n FROM unnest($%d::bigint[], $%d::text[]) AS s(c, n)))",
		clusterCol, next, clusterCol, nsCol, next+1, next+2)
	return frag, []any{clusters, ids, nss}
}

// ClusterSQL limits rows by cluster id to clusters the caller can see (whole or
// partially). Unrestricted callers get "TRUE".
func ClusterSQL(p *Principal, col string, next int) (string, []any) {
	if !p.Restricted() {
		return "TRUE", nil
	}
	ids := p.VisibleClusterIDs()
	if len(ids) == 0 {
		return "FALSE", nil
	}
	return fmt.Sprintf("%s = ANY($%d::bigint[])", col, next), []any{ids}
}

// WorkspaceSQL limits rows by workspace id. Platform workspace managers and
// unrestricted readers see every workspace; others only their memberships.
func WorkspaceSQL(p *Principal, col string, next int) (string, []any) {
	if p == nil {
		return "FALSE", nil
	}
	if p.Can("workspace:manage") || (!p.Restricted() && p.Can("workspace:read")) {
		return "TRUE", nil
	}
	ids := []int64{}
	if p.Scopes != nil {
		for id := range p.Scopes.Workspaces {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "FALSE", nil
	}
	return fmt.Sprintf("%s = ANY($%d::bigint[])", col, next), []any{ids}
}

// ---------------------------------------------------------------- gin helpers

func paramInt(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || v <= 0 {
		httpx.Fail(c, httpx.BadRequest("invalid "+name))
		return 0, false
	}
	return v, true
}

func principalOrFail(c *gin.Context) *Principal {
	p := PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
	}
	return p
}

// RequireClusterAccess rejects callers whose data scope excludes the cluster in
// path parameter param. It is a no-op for unrestricted callers.
func RequireClusterAccess(param string) gin.HandlerFunc {
	h := func(c *gin.Context) {
		p := principalOrFail(c)
		if p == nil {
			return
		}
		id, ok := paramInt(c, param)
		if !ok {
			return
		}
		if !p.CanCluster(id) {
			httpx.Fail(c, httpx.Forbidden("该集群不在你的数据范围内"))
			return
		}
		c.Next()
	}
	return registerGuard(h, Guard{Kind: "scope", Scope: "cluster"})
}

// RequireNamespaceAccess rejects callers whose data scope excludes the project
// given by the cluster and namespace path parameters.
func RequireNamespaceAccess(clusterParam, nsParam string) gin.HandlerFunc {
	h := func(c *gin.Context) {
		p := principalOrFail(c)
		if p == nil {
			return
		}
		id, ok := paramInt(c, clusterParam)
		if !ok {
			return
		}
		if !p.CanNamespace(id, c.Param(nsParam)) {
			httpx.Fail(c, httpx.Forbidden("该项目不在你的数据范围内"))
			return
		}
		c.Next()
	}
	return registerGuard(h, Guard{Kind: "scope", Scope: "namespace"})
}

// RequireWorkspaceAccess rejects callers that cannot see the workspace in path
// parameter param; with manage=true the caller must be able to manage it.
func RequireWorkspaceAccess(param string, manage bool) gin.HandlerFunc {
	h := func(c *gin.Context) {
		p := principalOrFail(c)
		if p == nil {
			return
		}
		id, ok := paramInt(c, param)
		if !ok {
			return
		}
		if manage && !p.CanManageWorkspace(id) {
			httpx.Fail(c, httpx.Forbidden("需要企业空间管理员权限"))
			return
		}
		if !p.CanWorkspace(id) {
			httpx.Fail(c, httpx.Forbidden("该企业空间不在你的数据范围内"))
			return
		}
		c.Next()
	}
	return registerGuard(h, Guard{Kind: "scope", Scope: workspaceScope(manage)})
}
