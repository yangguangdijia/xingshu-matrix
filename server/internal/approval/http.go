package approval

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// RegisterRoutes mounts the approval API on an authenticated group.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/approval-types", func(c *gin.Context) { httpx.OK(c, s.Types()) })

	r.GET("/approvals", func(c *gin.Context) {
		pg := httpx.ParsePage(c)
		f := ListFilter{Status: c.Query("status"), Type: c.Query("type"), Env: c.Query("env")}
		items, total, err := s.List(c.Request.Context(), rbac.PrincipalFrom(c), c.DefaultQuery("scope", "mine"), f, pg)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, pg))
	})
	r.GET("/approvals/:id", func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		t, err := s.Get(c.Request.Context(), rbac.PrincipalFrom(c), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, t)
	})
	r.POST("/approvals", func(c *gin.Context) {
		var in CreateInput
		if !httpx.Bind(c, &in) {
			return
		}
		t, err := s.Create(c.Request.Context(), rbac.PrincipalFrom(c), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "approvals", strconv.FormatInt(t.ID, 10))
		audit.SetDetail(c, gin.H{"type": t.Type, "env": t.Env, "title": t.Title})
		httpx.Created(c, t)
	})
	decide := func(fn func(*gin.Context, int64, string) (*Ticket, error)) gin.HandlerFunc {
		return func(c *gin.Context) {
			id, ok := httpx.ParamID(c, "id")
			if !ok {
				return
			}
			var in struct {
				Comment string `json:"comment" binding:"max=2000"`
			}
			if c.Request.ContentLength != 0 && !httpx.Bind(c, &in) {
				return
			}
			t, err := fn(c, id, in.Comment)
			if err != nil {
				httpx.Fail(c, err)
				return
			}
			audit.SetDetail(c, gin.H{"type": t.Type, "env": t.Env, "status": t.Status})
			httpx.OK(c, t)
		}
	}
	r.POST("/approvals/:id/approve", decide(func(c *gin.Context, id int64, comment string) (*Ticket, error) {
		return s.Approve(c.Request.Context(), rbac.PrincipalFrom(c), id, comment)
	}))
	r.POST("/approvals/:id/reject", decide(func(c *gin.Context, id int64, comment string) (*Ticket, error) {
		return s.Reject(c.Request.Context(), rbac.PrincipalFrom(c), id, comment)
	}))
	r.POST("/approvals/:id/cancel", decide(func(c *gin.Context, id int64, _ string) (*Ticket, error) {
		return s.Cancel(c.Request.Context(), rbac.PrincipalFrom(c), id)
	}))
}
