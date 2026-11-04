// Package devcenter 是研发中心后端：需求、迭代、缺陷、测试用例。
// 每类对象有固定的状态集合（见 Statuses），写操作记录最后修改人；扩展字段放 data（JSON 对象）。
package devcenter

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
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

// Kinds 是支持的对象类型。
var Kinds = []string{"requirements", "iterations", "bugs", "tests"}

// Statuses 是各类型允许的状态，第一个为默认状态。
var Statuses = map[string][]string{
	"requirements": {"todo", "ready", "doing", "testing", "accepting", "done", "rejected"},
	"iterations":   {"todo", "doing", "done"},
	"bugs":         {"todo", "doing", "fixed", "verified", "closed", "reopened"},
	"tests":        {"todo", "passed", "failed", "blocked"},
}

var priorities = map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}

func validStatus(k, s string) bool {
	for _, x := range Statuses[k] {
		if x == s {
			return true
		}
	}
	return false
}

func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
	m.d = d
	g := r.Group("/devcenter")
	rd, mg := rbac.RequirePerm("devcenter:read"), rbac.RequirePerm("devcenter:manage")
	g.GET("/summary", rd, m.summary)
	g.GET("/meta", rd, func(c *gin.Context) {
		httpx.OK(c, gin.H{"statuses": Statuses, "priorities": []string{"P0", "P1", "P2", "P3"}})
	})
	for _, k := range Kinds {
		g.GET("/"+k, rd, m.list(k))
		g.POST("/"+k, mg, m.create(k))
		g.GET("/"+k+"/:id", rd, m.get(k))
		g.PUT("/"+k+"/:id", mg, m.put(k))
		g.PATCH("/"+k+"/:id", mg, m.patch(k))
		g.DELETE("/"+k+"/:id", mg, m.del(k))
	}
}

func table(k string) string { return "devcenter_" + k }

const cols = "id,title,description,status,priority,iteration_id,assignee,data,created_by,updated_by,created_at,updated_at"

// Item 是四类对象共用的结构。
type Item struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Priority    string          `json:"priority"`
	IterationID *int64          `json:"iteration_id"`
	Assignee    string          `json:"assignee"`
	Data        json.RawMessage `json:"data"`
	CreatedBy   int64           `json:"created_by"`
	UpdatedBy   string          `json:"updated_by"`
	CreatedAt   any             `json:"created_at"`
	UpdatedAt   any             `json:"updated_at"`
}

func scan(row pgx.Row) (*Item, error) {
	var it Item
	var data []byte
	err := row.Scan(&it.ID, &it.Title, &it.Description, &it.Status, &it.Priority, &it.IterationID, &it.Assignee, &data, &it.CreatedBy, &it.UpdatedBy, &it.CreatedAt, &it.UpdatedAt)
	if len(data) == 0 {
		data = []byte("{}")
	}
	it.Data = data
	return &it, err
}

func fail(c *gin.Context, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("item not found"))
		return
	}
	if strings.Contains(err.Error(), "SQLSTATE 23503") {
		httpx.Fail(c, httpx.Validation("iteration does not exist"))
		return
	}
	httpx.Fail(c, httpx.Internal(err))
}

func who(c *gin.Context) (int64, string) {
	if p := rbac.PrincipalFrom(c); p != nil {
		return p.UserID, p.Username
	}
	return 0, ""
}

func (m *Module) list(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, me := who(c)
		assignee := c.Query("assignee")
		if assignee == "me" {
			assignee = me
		}
		var iter any
		if v := c.Query("iteration_id"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				httpx.Fail(c, httpx.Validation("invalid iteration_id"))
				return
			}
			iter = n
		}
		where := ` WHERE ($1='' OR status=ANY(string_to_array($1,','))) AND ($2='' OR assignee=$2) AND ($3::bigint IS NULL OR iteration_id=$3)
			AND ($4='' OR title ILIKE '%'||$4||'%' OR description ILIKE '%'||$4||'%')`
		args := []any{c.Query("status"), assignee, iter, strings.TrimSpace(c.Query("q"))}
		var total int
		if err := m.d.DB.QueryRow(c, "SELECT count(*) FROM "+table(k)+where, args...).Scan(&total); err != nil {
			fail(c, err)
			return
		}
		rows, err := m.d.DB.Query(c, "SELECT "+cols+" FROM "+table(k)+where+" ORDER BY updated_at DESC, id DESC LIMIT $5 OFFSET $6", append(args, lim(c), off(c))...)
		if err != nil {
			fail(c, err)
			return
		}
		defer rows.Close()
		out := []*Item{}
		for rows.Next() {
			it, err := scan(rows)
			if err != nil {
				fail(c, err)
				return
			}
			out = append(out, it)
		}
		httpx.OK(c, gin.H{"items": out, "total": total, "page_size": lim(c), "offset": off(c)})
	}
}

type input struct {
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	Status      *string         `json:"status"`
	Priority    *string         `json:"priority"`
	IterationID *int64          `json:"iteration_id"`
	Assignee    *string         `json:"assignee"`
	Data        json.RawMessage `json:"data"`
}

// normalize 校验并补默认值；full=true 时为创建 / 全量更新。
func (in *input) check(k string, full bool) error {
	if full {
		if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
			return httpx.Validation("title is required")
		}
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if len([]rune(t)) > 200 {
			return httpx.Validation("title too long (max 200)")
		}
		in.Title = &t
	}
	if in.Status != nil && *in.Status != "" && !validStatus(k, *in.Status) {
		return httpx.Validation("invalid status for " + k + ": " + *in.Status)
	}
	if in.Priority != nil && *in.Priority != "" && !priorities[*in.Priority] {
		return httpx.Validation("priority must be P0-P3")
	}
	if len(in.Data) > 0 && string(in.Data) != "null" {
		var obj map[string]any
		if json.Unmarshal(in.Data, &obj) != nil {
			return httpx.Validation("data must be a JSON object")
		}
		if len(in.Data) > 16384 {
			return httpx.Validation("data too large")
		}
	}
	if in.IterationID != nil && *in.IterationID <= 0 {
		in.IterationID = nil
	}
	return nil
}

func str(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

func (m *Module) create(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in input
		if c.ShouldBindJSON(&in) != nil {
			httpx.Fail(c, httpx.Validation("invalid body"))
			return
		}
		if err := in.check(k, true); err != nil {
			httpx.Fail(c, err)
			return
		}
		uid, uname := who(c)
		data := in.Data
		if len(data) == 0 || string(data) == "null" {
			data = json.RawMessage("{}")
		}
		var iter any
		if in.IterationID != nil && k != "iterations" {
			iter = *in.IterationID
		}
		it, err := scan(m.d.DB.QueryRow(c, "INSERT INTO "+table(k)+"(title,description,status,priority,iteration_id,assignee,data,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING "+cols,
			*in.Title, str(in.Description, ""), str(in.Status, Statuses[k][0]), str(in.Priority, "P2"), iter, strings.TrimSpace(str(in.Assignee, "")), []byte(data), uid, uname))
		if err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(it.ID, 10))
		httpx.Created(c, it)
	}
}

func (m *Module) get(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		it, err := scan(m.d.DB.QueryRow(c, "SELECT "+cols+" FROM "+table(k)+" WHERE id=$1", id))
		if err != nil {
			fail(c, err)
			return
		}
		httpx.OK(c, it)
	}
}

// put 全量更新（标题必填）；patch 只改给出的字段，常用于看板拖动改状态。
func (m *Module) put(k string) gin.HandlerFunc   { return m.update(k, true) }
func (m *Module) patch(k string) gin.HandlerFunc { return m.update(k, false) }

func (m *Module) update(k string, full bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		raw := map[string]json.RawMessage{}
		body, _ := c.GetRawData()
		if json.Unmarshal(body, &raw) != nil {
			httpx.Fail(c, httpx.Validation("invalid body"))
			return
		}
		var in input
		_ = json.Unmarshal(body, &in)
		if err := in.check(k, full); err != nil {
			httpx.Fail(c, err)
			return
		}
		_, uname := who(c)
		sets, args := []string{}, []any{id}
		add := func(col string, v any) { args = append(args, v); sets = append(sets, col+"=$"+strconv.Itoa(len(args))) }
		if in.Title != nil {
			add("title", *in.Title)
		}
		if in.Description != nil {
			add("description", *in.Description)
		}
		if in.Status != nil && *in.Status != "" {
			add("status", *in.Status)
		}
		if in.Priority != nil && *in.Priority != "" {
			add("priority", *in.Priority)
		}
		if _, has := raw["iteration_id"]; has && k != "iterations" {
			if in.IterationID == nil {
				add("iteration_id", nil)
			} else {
				add("iteration_id", *in.IterationID)
			}
		}
		if in.Assignee != nil {
			add("assignee", strings.TrimSpace(*in.Assignee))
		}
		if len(in.Data) > 0 && string(in.Data) != "null" {
			add("data", []byte(in.Data))
		}
		if len(sets) == 0 {
			httpx.Fail(c, httpx.Validation("nothing to update"))
			return
		}
		add("updated_by", uname)
		it, err := scan(m.d.DB.QueryRow(c, "UPDATE "+table(k)+" SET "+strings.Join(sets, ",")+",updated_at=now() WHERE id=$1 RETURNING "+cols, args...))
		if err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(id, 10))
		httpx.OK(c, it)
	}
}

func (m *Module) del(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		tag, err := m.d.DB.Exec(c, "DELETE FROM "+table(k)+" WHERE id=$1", id)
		if err != nil {
			fail(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			httpx.Fail(c, httpx.NotFound("item not found"))
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(id, 10))
		httpx.OK(c, gin.H{"id": id})
	}
}

// summary 返回各类型按状态的计数，以及指派给当前用户的未完成数量，供工作台使用。
func (m *Module) summary(c *gin.Context) {
	_, me := who(c)
	out := gin.H{}
	done := map[string][]string{"requirements": {"done", "rejected"}, "iterations": {"done"}, "bugs": {"verified", "closed"}, "tests": {"passed"}}
	for _, k := range Kinds {
		rows, err := m.d.DB.Query(c, "SELECT status, count(*), count(*) FILTER (WHERE assignee=$1) FROM "+table(k)+" GROUP BY status", me)
		if err != nil {
			fail(c, err)
			return
		}
		by, total, mine := map[string]int{}, 0, 0
		for rows.Next() {
			var s string
			var n, my int
			if err := rows.Scan(&s, &n, &my); err != nil {
				rows.Close()
				fail(c, err)
				return
			}
			by[s] = n
			total += n
			finished := false
			for _, d := range done[k] {
				finished = finished || d == s
			}
			if !finished {
				mine += my
			}
		}
		rows.Close()
		out[k] = gin.H{"by_status": by, "total": total, "mine_open": mine}
	}
	httpx.OK(c, out)
}

func lim(c *gin.Context) int {
	n, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if n < 1 || n > 500 {
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
