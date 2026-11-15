package backup

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup/objstore"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Permission codes. There is no platform:* permission in the core catalog;
// backups follow the system-settings permissions (dumps contain every
// encrypted secret, so download needs manage).
const (
	PermRead   = "setting:read"
	PermManage = "setting:manage"
)

// APIDeps are the dependencies of the HTTP endpoints.
type APIDeps struct {
	Runner *Runner
	// Audit records downloads (GET is not covered by the audit middleware).
	Audit *audit.Recorder
}

// RegisterRoutes mounts on an authenticated group:
//
//	GET  /platform/backups               list (paginated)
//	POST /platform/backups               start a manual backup (202 + row)
//	GET  /platform/backups/:id/download  stream the dump (audited)
//	GET  /platform/backup-policy         effective schedule / retention (read-only, from deployment config)
func RegisterRoutes(r gin.IRoutes, d APIDeps) {
	r.GET("/platform/backup-policy", rbac.RequirePerm(PermRead), func(c *gin.Context) {
		httpx.OK(c, d.Runner.Policy())
	})

	r.GET("/platform/backups", rbac.RequirePerm(PermRead), func(c *gin.Context) {
		p := httpx.ParsePage(c)
		items, total, err := d.Runner.List(c.Request.Context(), p.Limit(), p.Offset())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, p))
	})

	r.POST("/platform/backups", rbac.RequirePerm(PermManage), func(c *gin.Context) {
		var uid int64
		if pr := rbac.PrincipalFrom(c); pr != nil {
			uid = pr.UserID
		}
		audit.SetAction(c, "create")
		id, err := d.Runner.Trigger(c.Request.Context(), KindManual, uid)
		if errors.Is(err, ErrBusy) {
			httpx.Fail(c, httpx.Conflict(err.Error()))
			return
		}
		if err != nil {
			httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, err.Error()))
			return
		}
		audit.SetResource(c, "platform_backup", strconv.FormatInt(id, 10))
		audit.SetDetail(c, gin.H{"kind": KindManual})
		rec, err := d.Runner.Get(c.Request.Context(), id)
		if err != nil {
			rec = &Record{ID: id, Kind: KindManual, Status: StatusRunning}
		}
		c.JSON(http.StatusAccepted, httpx.Envelope{Code: httpx.CodeOK, Message: "备份已开始", Data: rec, RequestID: httpx.RequestIDFrom(c)})
	})

	r.GET("/platform/backups/:id/download", rbac.RequirePerm(PermManage), func(c *gin.Context) {
		start := time.Now()
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(c, httpx.BadRequest("invalid id"))
			return
		}
		audit.SetResource(c, "platform_backup", strconv.FormatInt(id, 10))
		audit.SetAction(c, "download")
		record := func() {
			if d.Audit != nil {
				d.Audit.Record(c.Request.Context(), audit.FromGin(c, start))
			}
		}
		rec, err := d.Runner.Get(c.Request.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Fail(c, httpx.NotFound("备份不存在"))
			record()
			return
		}
		if err != nil {
			httpx.Fail(c, err)
			record()
			return
		}
		if rec.Status != StatusSucceeded || rec.File == "" {
			httpx.Fail(c, httpx.Conflict("该备份未成功完成，无法下载"))
			record()
			return
		}
		rc, size, err := d.Runner.Open(c.Request.Context(), rec.File)
		if errors.Is(err, objstore.ErrNotFound) {
			httpx.Fail(c, httpx.NotFound("备份文件已不存在"))
			record()
			return
		}
		if err != nil {
			httpx.Fail(c, err)
			record()
			return
		}
		defer rc.Close()
		h := c.Writer.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, rec.File))
		h.Set("X-Content-SHA256", rec.SHA256)
		h.Set("Cache-Control", "no-store")
		if size > 0 {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
		c.Status(http.StatusOK)
		n, cerr := io.Copy(c.Writer, rc)
		detail := gin.H{"file": rec.File, "sha256": rec.SHA256, "size": rec.Size, "sent": n}
		if cerr != nil {
			detail["error"] = cerr.Error()
		}
		audit.SetDetail(c, detail)
		record()
	})
}
