package automation

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

//go:embed migrations/*.sql
var migFS embed.FS

type Module struct{ d *app.Deps }

func New() *Module                  { return &Module{} }
func (m *Module) Name() string      { return "automation" }
func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }
func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{
		rbac.P("automation", "automation", "read", "查看编排、版本和执行记录"),
		rbac.P("automation", "automation", "manage", "管理编排定义和版本"),
		rbac.P("automation", "automation", "run", "触发、重试和取消编排执行"),
	}
}
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/automation")
	g.GET("/workflows", rbac.RequirePerm("automation:read"), m.list)
	g.POST("/workflows", rbac.RequirePerm("automation:manage"), m.create)
	g.GET("/workflows/:id", rbac.RequirePerm("automation:read"), m.get)
	g.POST("/workflows/:id/versions", rbac.RequirePerm("automation:manage"), m.version)
	g.POST("/workflows/:id/trigger", rbac.RequirePerm("automation:run"), m.trigger)
	g.GET("/runs", rbac.RequirePerm("automation:read"), m.runs)
	g.POST("/runs/:id/retry", rbac.RequirePerm("automation:run"), m.retry)
	g.POST("/runs/:id/cancel", rbac.RequirePerm("automation:run"), m.cancel)
}

type workflowInput struct {
	Name        string          `json:"name" binding:"required,max=120"`
	Description string          `json:"description"`
	Trigger     string          `json:"trigger"`
	Steps       json.RawMessage `json:"steps" binding:"required"`
}

func (m *Module) create(c *gin.Context) {
	var in workflowInput
	if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" {
		httpx.Fail(c, httpx.Validation("name and steps are required"))
		return
	}
	var id int64
	err := m.d.DB.QueryRow(c, `INSERT INTO automation_workflows(name,description,trigger,created_by) VALUES($1,$2,$3,$4) RETURNING id`, in.Name, in.Description, in.Trigger, rbac.PrincipalFrom(c).UserID).Scan(&id)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if _, err = m.d.DB.Exec(c, `INSERT INTO automation_workflow_versions(workflow_id,version,steps,created_by) VALUES($1,1,$2,$3)`, id, in.Steps, rbac.PrincipalFrom(c).UserID); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.Created(c, gin.H{"id": id, "version": 1})
}
func (m *Module) list(c *gin.Context) {
	limit, offset := page(c)
	rows, err := m.d.DB.Query(c, `SELECT id,name,description,trigger,status,current_version,created_at,updated_at FROM automation_workflows ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var id int64
		var n, d, t, s string
		var v int
		var a, u time.Time
		if err := rows.Scan(&id, &n, &d, &t, &s, &v, &a, &u); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		items = append(items, gin.H{"id": id, "name": n, "description": d, "trigger": t, "status": s, "current_version": v, "created_at": a, "updated_at": u})
	}
	httpx.OK(c, gin.H{"items": items, "page_size": limit, "offset": offset})
}
func (m *Module) get(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var n, d, t, s string
	var v int
	var created, updated time.Time
	err := m.d.DB.QueryRow(c, `SELECT name,description,trigger,status,current_version,created_at,updated_at FROM automation_workflows WHERE id=$1`, id).Scan(&n, &d, &t, &s, &v, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("workflow not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"id": id, "name": n, "description": d, "trigger": t, "status": s, "current_version": v, "created_at": created, "updated_at": updated})
}
func (m *Module) version(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Steps json.RawMessage `json:"steps" binding:"required"`
	}
	if c.ShouldBindJSON(&in) != nil {
		httpx.Fail(c, httpx.Validation("steps are required"))
		return
	}
	var v int
	err := m.d.DB.QueryRow(c, `SELECT COALESCE(MAX(version),0)+1 FROM automation_workflow_versions WHERE workflow_id=$1`, id).Scan(&v)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	_, err = m.d.DB.Exec(c, `INSERT INTO automation_workflow_versions(workflow_id,version,steps,created_by) VALUES($1,$2,$3,$4)`, id, v, in.Steps, rbac.PrincipalFrom(c).UserID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	_, _ = m.d.DB.Exec(c, `UPDATE automation_workflows SET current_version=$2,updated_at=now() WHERE id=$1`, id, v)
	httpx.Created(c, gin.H{"workflow_id": id, "version": v})
}
func (m *Module) trigger(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var v int
	err := m.d.DB.QueryRow(c, `SELECT current_version FROM automation_workflows WHERE id=$1 AND status='active'`, id).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("active workflow not found"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	var run int64
	err = m.d.DB.QueryRow(c, `INSERT INTO automation_runs(workflow_id,version,status,triggered_by) VALUES($1,$2,'queued',$3) RETURNING id`, id, v, rbac.PrincipalFrom(c).UserID).Scan(&run)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.Created(c, gin.H{"id": run, "status": "queued", "mode": "controlled"})
}
func (m *Module) runs(c *gin.Context) {
	limit, offset := page(c)
	rows, err := m.d.DB.Query(c, `SELECT id,workflow_id,version,status,triggered_by,started_at,finished_at,error FROM automation_runs ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var id, w, u int64
		var v int
		var s string
		var st, ft *time.Time
		var e *string
		if rows.Scan(&id, &w, &v, &s, &u, &st, &ft, &e) != nil {
			httpx.Fail(c, httpx.Internal(errors.New("scan automation run")))
			return
		}
		items = append(items, gin.H{"id": id, "workflow_id": w, "version": v, "status": s, "triggered_by": u, "started_at": st, "finished_at": ft, "error": e})
	}
	httpx.OK(c, gin.H{"items": items, "page_size": limit, "offset": offset})
}
func (m *Module) retry(c *gin.Context)  { m.transition(c, "queued", true) }
func (m *Module) cancel(c *gin.Context) { m.transition(c, "cancelled", false) }
func (m *Module) transition(c *gin.Context, status string, retry bool) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	q := `UPDATE automation_runs SET status=$2,finished_at=CASE WHEN $2='cancelled' THEN now() ELSE NULL END,error=NULL WHERE id=$1 AND status IN ('queued','failed')`
	if retry {
		q = `UPDATE automation_runs SET status='queued',finished_at=NULL,error=NULL,attempt=attempt+1 WHERE id=$1 AND status='failed'`
	}
	var tag pgconn.CommandTag
	var err error
	if retry { tag, err = m.d.DB.Exec(c, q, id) } else { tag, err = m.d.DB.Exec(c, q, id, status) }
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.Conflict("run cannot be transitioned"))
		return
	}
	httpx.OK(c, gin.H{"id": id, "status": status})
}
func idParam(c *gin.Context) (int64, bool) {
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id < 1 {
		httpx.Fail(c, httpx.Validation("invalid id"))
		return 0, false
	}
	return id, true
}
func page(c *gin.Context) (int, int) {
	n, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if n < 1 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	o, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if o < 0 {
		o = 0
	}
	return n, o
}
