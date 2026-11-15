package workspace

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Permission codes (registered in rbac.CorePermissions).
const (
	PermRead   = "workspace:read"
	PermManage = "workspace:manage"
)

var _ ScopeCache = (*rbac.Service)(nil)

// Deps are the dependencies of RegisterRoutes.
type Deps struct {
	Store Store
	// Scopes is the rbac service: its cache is invalidated after membership /
	// project / binding changes, and it resolves data scopes for these routes
	// when the global scope middleware is not mounted. Optional.
	Scopes ScopeCache
}

// RegisterRoutes mounts the workspace API on an authenticated group:
//
//	GET    /workspaces                       workspace:read (filtered by membership for scope-only users)
//	POST   /workspaces                       workspace:manage
//	GET    /workspaces/:id                   workspace:read + workspace visible
//	PUT    /workspaces/:id                   workspace:manage
//	DELETE /workspaces/:id                   workspace:manage, body {confirm_name}
//	GET    /workspaces/:id/members           workspace:read + workspace visible
//	POST   /workspaces/:id/members           workspace:manage
//	PUT    /workspaces/:id/members/:mid      workspace:manage
//	DELETE /workspaces/:id/members/:mid      workspace:manage
//	GET    /workspaces/:id/projects          workspace:read + workspace visible
//	POST   /workspaces/:id/projects          workspace:manage
//	DELETE /workspaces/:id/projects/:pid     workspace:manage
//	GET    /role-bindings                    workspace:read
//	POST   /role-bindings                    workspace:manage (+ user:manage for global/env)
//	DELETE /role-bindings/:id                workspace:manage (+ user:manage for global/env)
func RegisterRoutes(r gin.IRoutes, d Deps) {
	svc := &Service{Store: d.Store, Scopes: d.Scopes}
	scope := scopeMiddleware(d.Scopes)
	read := rbac.RequirePerm(PermRead)
	manage := rbac.RequirePerm(PermManage)
	visible := rbac.RequireWorkspaceAccess("id", false)

	r.GET("/workspaces", scope, read, func(c *gin.Context) {
		pg := httpx.ParsePage(c)
		items, total, err := svc.ListWorkspaces(c.Request.Context(), rbac.PrincipalFrom(c), c.Query("keyword"), pg.Limit(), pg.Offset())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, pg))
	})

	r.POST("/workspaces", scope, manage, func(c *gin.Context) {
		audit.SetResource(c, "workspace", "")
		audit.SetAction(c, "create")
		var in CreateInput
		if !httpx.Bind(c, &in) {
			return
		}
		w, err := svc.CreateWorkspace(c.Request.Context(), rbac.PrincipalFrom(c), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "workspace", strconv.FormatInt(w.ID, 10))
		audit.SetDetail(c, gin.H{"name": w.Name, "manager_id": w.ManagerID,
			"cpu_limit_milli": w.CPULimitMilli, "memory_limit_bytes": w.MemoryLimitBytes})
		httpx.Created(c, w)
	})

	r.GET("/workspaces/:id", scope, read, visible, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		w, err := svc.GetWorkspace(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, w)
	})

	r.PUT("/workspaces/:id", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace", strconv.FormatInt(id, 10))
		audit.SetAction(c, "update")
		var in UpdateInput
		if !httpx.Bind(c, &in) {
			return
		}
		w, err := svc.UpdateWorkspace(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"name": w.Name, "changed": changedFields(in)})
		httpx.OK(c, w)
	})

	r.DELETE("/workspaces/:id", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace", strconv.FormatInt(id, 10))
		audit.SetAction(c, "delete")
		var in struct {
			ConfirmName string `json:"confirm_name"`
		}
		if c.Request.ContentLength != 0 && !httpx.Bind(c, &in) {
			return
		}
		w, err := svc.DeleteWorkspace(c.Request.Context(), id, in.ConfirmName)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"name": w.Name, "member_count": w.MemberCount, "project_count": w.ProjectCount})
		httpx.OK(c, gin.H{"id": id, "deleted": true})
	})

	// ---------------------------------------------------------- members

	r.GET("/workspaces/:id/members", scope, read, visible, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		items, err := svc.ListMembers(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"items": items, "total": len(items)})
	})

	r.POST("/workspaces/:id/members", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace_member", strconv.FormatInt(id, 10))
		audit.SetAction(c, "create")
		var in MemberInput
		if !httpx.Bind(c, &in) {
			return
		}
		m, err := svc.AddMember(c.Request.Context(), rbac.PrincipalFrom(c), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, memberDetail(m))
		httpx.Created(c, m)
	})

	r.PUT("/workspaces/:id/members/:mid", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		mid, ok := parseID(c, "mid")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace_member", strconv.FormatInt(id, 10))
		audit.SetAction(c, "update")
		var in struct {
			Role string `json:"role"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		m, err := svc.UpdateMember(c.Request.Context(), id, mid, in.Role)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, memberDetail(m))
		httpx.OK(c, m)
	})

	r.DELETE("/workspaces/:id/members/:mid", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		mid, ok := parseID(c, "mid")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace_member", strconv.FormatInt(id, 10))
		audit.SetAction(c, "delete")
		m, err := svc.DeleteMember(c.Request.Context(), id, mid)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, memberDetail(m))
		httpx.OK(c, gin.H{"id": mid, "deleted": true})
	})

	// ---------------------------------------------------------- projects

	r.GET("/workspaces/:id/projects", scope, read, visible, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		items, err := svc.ListProjects(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"items": items, "total": len(items)})
	})

	r.POST("/workspaces/:id/projects", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace_project", strconv.FormatInt(id, 10))
		audit.SetAction(c, "create")
		var in ProjectInput
		if !httpx.Bind(c, &in) {
			return
		}
		p, err := svc.AddProject(c.Request.Context(), rbac.PrincipalFrom(c), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"project_id": p.ID, "cluster_id": p.ClusterID, "namespace": p.Namespace})
		httpx.Created(c, p)
	})

	r.DELETE("/workspaces/:id/projects/:pid", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		pid, ok := parseID(c, "pid")
		if !ok {
			return
		}
		audit.SetResource(c, "workspace_project", strconv.FormatInt(id, 10))
		audit.SetAction(c, "delete")
		p, err := svc.DeleteProject(c.Request.Context(), id, pid)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"project_id": p.ID, "cluster_id": p.ClusterID, "namespace": p.Namespace})
		httpx.OK(c, gin.H{"id": pid, "deleted": true})
	})

	// ---------------------------------------------------------- role bindings

	r.GET("/role-bindings", scope, read, func(c *gin.Context) {
		f := BindingFilter{Scope: c.Query("scope"), ScopeValue: strings.TrimSpace(c.Query("scope_value"))}
		var ok bool
		if f.RoleID, ok = queryID(c, "role_id"); !ok {
			return
		}
		if f.UserID, ok = queryID(c, "user_id"); !ok {
			return
		}
		if f.GroupID, ok = queryID(c, "group_id"); !ok {
			return
		}
		pg := httpx.ParsePage(c)
		items, total, err := svc.ListBindings(c.Request.Context(), rbac.PrincipalFrom(c), f, pg.Limit(), pg.Offset())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, pg))
	})

	r.POST("/role-bindings", scope, manage, func(c *gin.Context) {
		audit.SetResource(c, "role_binding", "")
		audit.SetAction(c, "create")
		var in BindingInput
		if !httpx.Bind(c, &in) {
			return
		}
		b, err := svc.CreateBinding(c.Request.Context(), rbac.PrincipalFrom(c), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "role_binding", strconv.FormatInt(b.ID, 10))
		audit.SetDetail(c, bindingDetail(b))
		httpx.Created(c, b)
	})

	r.DELETE("/role-bindings/:id", scope, manage, func(c *gin.Context) {
		id, ok := parseID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "role_binding", strconv.FormatInt(id, 10))
		audit.SetAction(c, "delete")
		b, err := svc.DeleteBinding(c.Request.Context(), rbac.PrincipalFrom(c), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, bindingDetail(b))
		httpx.OK(c, gin.H{"id": id, "deleted": true})
	})
}

// scopeMiddleware resolves the caller's data scope for these routes when the
// global rbac scope middleware has not already done so, so workspace members
// without a global role reach the read endpoints. With no ScopeCache it is a
// pass-through.
func scopeMiddleware(sc ScopeCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		base := rbac.PrincipalFrom(c)
		if sc == nil || base == nil || base.Scopes != nil {
			c.Next()
			return
		}
		p, err := sc.ScopedPrincipal(c.Request.Context(), base)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		if p != nil && p != base {
			rbac.SetPrincipal(c, p)
		}
		c.Next()
	}
}

func parseID(c *gin.Context, name string) (int64, bool) {
	id, ok := parsePositiveID(c.Param(name))
	if !ok {
		msg := "编号无效"
		switch name {
		case "id":
			msg = "企业空间编号无效"
			if strings.Contains(c.FullPath(), "role-bindings") {
				msg = "角色绑定编号无效"
			}
		case "mid":
			msg = "成员编号无效"
		case "pid":
			msg = "项目编号无效"
		}
		httpx.Fail(c, httpx.BadRequest(msg))
		return 0, false
	}
	return id, true
}

func queryID(c *gin.Context, name string) (int64, bool) {
	v := strings.TrimSpace(c.Query(name))
	if v == "" {
		return 0, true
	}
	id, ok := parsePositiveID(v)
	if !ok {
		httpx.Fail(c, httpx.Validation(name+" 无效"))
		return 0, false
	}
	return id, true
}

func changedFields(in UpdateInput) []string {
	out := []string{}
	if in.Name != nil {
		out = append(out, "name")
	}
	if in.Description != nil {
		out = append(out, "description")
	}
	if in.ManagerID != nil {
		out = append(out, "manager_id")
	}
	if in.CPULimitMilli != nil {
		out = append(out, "cpu_limit_milli")
	}
	if in.MemoryLimitBytes != nil {
		out = append(out, "memory_limit_bytes")
	}
	return out
}

func memberDetail(m *Member) gin.H {
	return gin.H{"member_id": m.ID, "kind": m.Kind, "user_id": m.UserID, "group_id": m.GroupID, "name": m.Name, "role": m.Role}
}

func bindingDetail(b *RoleBinding) gin.H {
	return gin.H{"role_id": b.RoleID, "role_code": b.RoleCode, "kind": b.Kind, "user_id": b.UserID, "group_id": b.GroupID,
		"subject": b.SubjectName, "scope": b.Scope, "scope_value": b.ScopeValue}
}
