package rbac

import (
	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// CorePermissions are the permissions owned by the core platform.
func CorePermissions() []Permission {
	return []Permission{
		P("core", "user", ActionRead, "查看用户"),
		P("core", "user", ActionManage, "管理用户（创建/修改/禁用/重置密码/分配角色）"),
		P("core", "role", ActionRead, "查看角色与权限"),
		P("core", "role", ActionManage, "管理角色与环境策略"),
		P("core", "setting", ActionRead, "查看系统设置"),
		P("core", "setting", ActionManage, "修改系统设置"),
		P("core", "audit", ActionRead, "查看审计日志"),
		P("core", "approval", ActionRead, "查看全部审批单"),
		P("core", "approval", "create", "发起审批"),
		P("core", "approval", "approve", "审批（通过/驳回）"),
		P("core", "ai", ActionRead, "查看 AI 模型与提示词模板"),
		P("core", "ai", ActionManage, "管理 AI 模型与提示词模板"),
		P("core", "ai", "chat", "使用 AI 助手"),
	}
}

// RegisterRoutes mounts role / permission / env-policy endpoints on an authenticated group.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/permissions", RequirePerm("role:read"), func(c *gin.Context) { httpx.OK(c, s.Catalog.List()) })

	r.GET("/roles", RequirePerm("role:read"), func(c *gin.Context) {
		roles, err := s.ListRoles(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, roles)
	})
	r.GET("/roles/:id", RequirePerm("role:read"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		role, err := s.GetRole(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, role)
	})
	r.POST("/roles", RequirePerm("role:manage"), func(c *gin.Context) {
		var in RoleInput
		if !httpx.Bind(c, &in) {
			return
		}
		role, err := s.CreateRole(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.Created(c, role)
	})
	r.PUT("/roles/:id", RequirePerm("role:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in RoleInput
		if !httpx.Bind(c, &in) {
			return
		}
		role, err := s.UpdateRole(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, role)
	})
	r.DELETE("/roles/:id", RequirePerm("role:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		if err := s.DeleteRole(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id})
	})

	r.GET("/env-policies", RequirePerm("role:read"), func(c *gin.Context) {
		ps, err := s.ListEnvPolicies(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, ps)
	})
	r.PUT("/env-policies/:env", RequirePerm("role:manage"), func(c *gin.Context) {
		var in struct {
			Name               string `json:"name"`
			RequireApproval    bool   `json:"require_approval"`
			ApproverPermission string `json:"approver_permission"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		p, err := s.UpsertEnvPolicy(c.Request.Context(), EnvPolicy{
			Env: c.Param("env"), Name: in.Name, RequireApproval: in.RequireApproval, ApproverPermission: in.ApproverPermission,
		})
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, p)
	})
}
