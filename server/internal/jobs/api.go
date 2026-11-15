package jobs

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Permission codes. Like platform backups there is no dedicated platform:*
// permission in the core catalog; the job queue follows system settings.
const (
	PermRead   = "setting:read"
	PermManage = "setting:manage"
)

// AdminStore is what the HTTP endpoints need (PgStore in production).
type AdminStore interface {
	List(ctx context.Context, f ListFilter, limit, offset int) ([]Job, int64, error)
	Get(ctx context.Context, id int64) (*Job, error)
	Retry(ctx context.Context, id int64) (*Job, error)
	Delete(ctx context.Context, id int64) (*Job, error)
}

// APIDeps are the dependencies of the HTTP endpoints.
type APIDeps struct {
	Store AdminStore
	// Worker is optional; a retry wakes it so the job runs without poll delay.
	Worker *Worker
}

// RegisterRoutes mounts on an authenticated group (writes are recorded by the
// audit middleware with resource platform_job):
//
//	GET    /jobs             list (?status=a,b&type=&page=&page_size=)
//	GET    /jobs/:id         detail
//	POST   /jobs/:id/retry   requeue a failed / dead job
//	DELETE /jobs/:id         delete a dead / succeeded job
func RegisterRoutes(r gin.IRoutes, d APIDeps) {
	r.GET("/jobs", rbac.RequirePerm(PermRead), func(c *gin.Context) {
		f, err := parseListFilter(c.Query("status"), c.Query("type"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		p := httpx.ParsePage(c)
		items, total, err := d.Store.List(c.Request.Context(), f, p.Limit(), p.Offset())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		for i := range items {
			items[i].Payload = RedactPayload(items[i].Payload)
		}
		httpx.OK(c, httpx.NewPageResult(items, total, p))
	})

	r.GET("/jobs/:id", rbac.RequirePerm(PermRead), func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		j, err := d.Store.Get(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, mapErr(err))
			return
		}
		j.Payload = RedactPayload(j.Payload)
		httpx.OK(c, j)
	})

	r.POST("/jobs/:id/retry", rbac.RequirePerm(PermManage), func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		audit.SetResource(c, "platform_job", strconv.FormatInt(id, 10))
		audit.SetAction(c, "retry")
		j, err := d.Store.Retry(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, mapErr(err))
			return
		}
		audit.SetDetail(c, gin.H{"type": j.Type, "max_attempts": j.MaxAttempts})
		if d.Worker != nil {
			d.Worker.Notify()
		}
		j.Payload = RedactPayload(j.Payload)
		httpx.OK(c, j)
	})

	r.DELETE("/jobs/:id", rbac.RequirePerm(PermManage), func(c *gin.Context) {
		id, ok := parseID(c)
		if !ok {
			return
		}
		audit.SetResource(c, "platform_job", strconv.FormatInt(id, 10))
		audit.SetAction(c, "delete")
		j, err := d.Store.Delete(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, mapErr(err))
			return
		}
		audit.SetDetail(c, gin.H{"type": j.Type, "status": j.Status, "attempts": j.Attempts})
		httpx.OK(c, gin.H{"id": j.ID, "deleted": true})
	})
}

func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, httpx.BadRequest("任务编号无效"))
		return 0, false
	}
	return id, true
}

// parseListFilter validates ?status= (comma separated) and ?type=.
func parseListFilter(status, jobType string) (ListFilter, error) {
	var f ListFilter
	seen := map[string]bool{}
	for _, s := range strings.Split(status, ",") {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !ValidStatus(s) {
			return f, httpx.Validation("任务状态无效：" + s).WithDetails(gin.H{"allowed": AllStatuses})
		}
		seen[s] = true
		f.Statuses = append(f.Statuses, s)
	}
	f.Type = strings.TrimSpace(jobType)
	if len(f.Type) > maxTypeLen {
		return f, httpx.Validation("任务类型过长")
	}
	return f, nil
}

// mapErr translates store errors to API errors.
func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return httpx.NotFound("任务不存在")
	case errors.Is(err, ErrDuplicate):
		return httpx.Conflict("已有相同去重键的未完成任务，无法重试")
	case errors.Is(err, ErrInvalidState):
		return httpx.Conflict("当前状态不允许该操作：仅失败或死信任务可重试，仅死信或已完成任务可删除")
	}
	return err
}
