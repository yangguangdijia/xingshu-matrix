package devcenter

import (
	"embed"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migFS embed.FS

type Module struct{ d *app.Deps }

func New() *Module                  { return &Module{} }
func (m *Module) Name() string      { return "devcenter" }
func (m *Module) Migrations() fs.FS { sub, _ := fs.Sub(migFS, "migrations"); return sub }
func (m *Module) Permissions() []rbac.Permission {
	return []rbac.Permission{rbac.P("devcenter", "devcenter", "read", "查看研发中心数据"), rbac.P("devcenter", "devcenter", "manage", "管理需求、缺陷、迭代和测试")}
}
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/devcenter")
	for _, k := range []string{"requirements", "iterations", "bugs", "tests"} {
		g.GET("/"+k, rbac.RequirePerm("devcenter:read"), m.list(k))
		g.POST("/"+k, rbac.RequirePerm("devcenter:manage"), m.create(k))
		g.PATCH("/"+k+"/:id", rbac.RequirePerm("devcenter:manage"), m.update(k))
	}
}

type input struct {
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Priority    string          `json:"priority"`
	IterationID *int64          `json:"iteration_id"`
	Data        json.RawMessage `json:"data"`
}

func (m *Module) table(k string) string {
	switch k {
	case "requirements":
		return "devcenter_requirements"
	case "iterations":
		return "devcenter_iterations"
	case "bugs":
		return "devcenter_bugs"
	default:
		return "devcenter_tests"
	}
}
func (m *Module) list(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		t := m.table(k)
		rows, e := m.d.DB.Query(c, "SELECT id,title,description,status,priority,iteration_id,created_by,created_at,updated_at FROM "+t+" ORDER BY id DESC LIMIT $1 OFFSET $2", lim(c), off(c))
		if e != nil {
			httpx.Fail(c, httpx.Internal(e))
			return
		}
		defer rows.Close()
		out := []gin.H{}
		for rows.Next() {
			var id, by int64
			var title, desc, status, pri string
			var it *int64
			var at, ut interface{}
			if e = rows.Scan(&id, &title, &desc, &status, &pri, &it, &by, &at, &ut); e != nil {
				httpx.Fail(c, httpx.Internal(e))
				return
			}
			out = append(out, gin.H{"id": id, "title": title, "description": desc, "status": status, "priority": pri, "iteration_id": it, "created_by": by, "created_at": at, "updated_at": ut})
		}
		httpx.OK(c, gin.H{"items": out, "page_size": lim(c), "offset": off(c)})
	}
}
func (m *Module) create(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in input
		if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Title) == "" {
			httpx.Fail(c, httpx.Validation("title is required"))
			return
		}
		q := "INSERT INTO " + m.table(k) + "(title,description,status,priority,iteration_id,created_by) VALUES($1,$2,COALESCE(NULLIF($3,''),'todo'),COALESCE(NULLIF($4,''),'P2'),$5,$6) RETURNING id"
		var id int64
		e := m.d.DB.QueryRow(c, q, in.Title, in.Description, in.Status, in.Priority, in.IterationID, rbac.PrincipalFrom(c).UserID).Scan(&id)
		if e != nil {
			httpx.Fail(c, httpx.Internal(e))
			return
		}
		httpx.Created(c, gin.H{"id": id, "type": k})
	}
}
func (m *Module) update(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, e := strconv.ParseInt(c.Param("id"), 10, 64)
		if e != nil || id < 1 {
			httpx.Fail(c, httpx.Validation("invalid id"))
			return
		}
		var in struct {
			Status      string `json:"status"`
			Priority    string `json:"priority"`
			Description string `json:"description"`
		}
		if c.ShouldBindJSON(&in) != nil {
			httpx.Fail(c, httpx.Validation("invalid body"))
			return
		}
		tag, e := m.d.DB.Exec(c, "UPDATE "+m.table(k)+" SET status=COALESCE(NULLIF($2,''),status),priority=COALESCE(NULLIF($3,''),priority),description=COALESCE(NULLIF($4,''),description),updated_at=now() WHERE id=$1", id, in.Status, in.Priority, in.Description)
		if e != nil {
			httpx.Fail(c, httpx.Internal(e))
			return
		}
		if tag.RowsAffected() == 0 {
			httpx.Fail(c, httpx.NotFound("item not found"))
			return
		}
		httpx.OK(c, gin.H{"id": id, "status": in.Status})
	}
}
func lim(c *gin.Context) int {
	n, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if n < 1 || n > 200 {
		n = 50
	}
	return n
}
func off(c *gin.Context) int {
	n, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if n < 0 {
		return 0
	}
	return n
}

var _ = errors.Is
var _ = pgx.ErrNoRows
