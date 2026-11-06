// Package devcenter 是研发中心后端：需求 / 任务、迭代、缺陷、测试用例，以及评论、变更记录、
// 代码仓库与流水线关联、统计报表和需求 AI 开发（生成实施方案与任务拆解）。
// 每类对象有固定的状态集合（见 Statuses），写操作记录最后修改人和变更记录；扩展字段放 data（JSON 对象）。
package devcenter

import (
	"context"
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

// DoneStatuses 是各类型视为“已结束”的状态（工作台、统计共用）。
var DoneStatuses = map[string][]string{"requirements": {"done", "rejected"}, "iterations": {"done"}, "bugs": {"verified", "closed"}, "tests": {"passed"}}

// ItemTypes 是需求表里的条目类型：story 需求、task 任务（存 data.type，缺省为 story）。
var ItemTypes = map[string]bool{"story": true, "task": true}

var priorities = map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}

func validKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

func validStatus(k, s string) bool {
	for _, x := range Statuses[k] {
		if x == s {
			return true
		}
	}
	return false
}

func isDone(k, s string) bool {
	for _, x := range DoneStatuses[k] {
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
		httpx.OK(c, gin.H{"statuses": Statuses, "done_statuses": DoneStatuses, "priorities": []string{"P0", "P1", "P2", "P3"}, "item_types": []string{"story", "task"}})
	})
	for _, k := range Kinds {
		g.GET("/"+k, rd, m.list(k))
		g.POST("/"+k, mg, m.create(k))
		g.GET("/"+k+"/:id", rd, m.get(k))
		g.PUT("/"+k+"/:id", mg, m.put(k))
		g.PATCH("/"+k+"/:id", mg, m.patch(k))
		g.DELETE("/"+k+"/:id", mg, m.del(k))
		g.GET("/"+k+"/:id/comments", rd, m.listComments(k))
		g.POST("/"+k+"/:id/comments", mg, m.addComment(k))
		g.GET("/"+k+"/:id/activities", rd, m.listActivities(k))
		if k == "requirements" || k == "bugs" {
			g.GET("/"+k+"/:id/links", rd, m.listItemLinks(k))
			g.POST("/"+k+"/:id/links", mg, m.addLink(k))
		}
	}
	g.DELETE("/comments/:id", mg, m.delComment)
	g.GET("/links", rd, m.listLinks)
	g.DELETE("/links/:id", mg, m.delLink)
	g.GET("/stats", rd, m.stats)
	g.GET("/iterations/:id/burndown", rd, m.burndown)
	g.GET("/ai/status", rd, m.aiStatus)
	g.GET("/ai-tasks", rd, m.listAITasks)
	g.GET("/ai-tasks/:id", rd, m.getAITask)
	g.POST("/requirements/:id/ai-tasks", mg, rbac.RequirePerm("ai:chat"), m.createAITask)
	g.POST("/ai-tasks/:id/apply", mg, m.applyAITask)
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
	var he *httpx.Error
	if errors.As(err, &he) {
		httpx.Fail(c, he)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Fail(c, httpx.NotFound("记录不存在或已被删除"))
		return
	}
	if strings.Contains(err.Error(), "SQLSTATE 23503") {
		httpx.Fail(c, httpx.Validation("所选迭代不存在"))
		return
	}
	if strings.Contains(err.Error(), "SQLSTATE 23505") {
		httpx.Fail(c, httpx.Conflict("已存在相同的关联"))
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

// listQuery 是列表筛选条件；"me" 由调用方解析为当前用户。
type listQuery struct {
	Status, Assignee, Q, Priority, Type, Involved string
	Iteration, CreatedBy, Parent                  *int64
}

// buildWhere 生成列表 WHERE 子句与参数（表别名 t）。只拼接固定片段，所有取值走参数。
func buildWhere(k string, q listQuery) (string, []any) {
	conds, args := []string{}, []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if q.Status != "" {
		conds = append(conds, "t.status=ANY(string_to_array("+arg(q.Status)+",','))")
	}
	if q.Assignee != "" {
		conds = append(conds, "t.assignee="+arg(q.Assignee))
	}
	if q.Iteration != nil {
		conds = append(conds, "t.iteration_id="+arg(*q.Iteration))
	}
	if q.Q != "" {
		p := arg(q.Q)
		conds = append(conds, "(t.title ILIKE '%'||"+p+"||'%' OR t.description ILIKE '%'||"+p+"||'%' OR t.id::text="+p+")")
	}
	if q.Priority != "" {
		conds = append(conds, "t.priority=ANY(string_to_array("+arg(q.Priority)+",','))")
	}
	if q.CreatedBy != nil {
		conds = append(conds, "t.created_by="+arg(*q.CreatedBy))
	}
	if q.Type != "" && k == "requirements" {
		conds = append(conds, "COALESCE(NULLIF(t.data->>'type',''),'story')="+arg(q.Type))
	}
	if q.Parent != nil && k == "requirements" {
		conds = append(conds, "t.data->>'parent_id'="+arg(strconv.FormatInt(*q.Parent, 10)))
	}
	if q.Involved != "" {
		u, kk := arg(q.Involved), arg(k)
		conds = append(conds, "(t.assignee="+u+" OR t.updated_by="+u+
			" OR EXISTS(SELECT 1 FROM devcenter_activities a WHERE a.kind="+kk+" AND a.item_id=t.id AND a.actor="+u+")"+
			" OR EXISTS(SELECT 1 FROM devcenter_comments cm WHERE cm.kind="+kk+" AND cm.item_id=t.id AND cm.author="+u+"))")
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func optID(c *gin.Context, key string) (*int64, bool) {
	v := c.Query(key)
	if v == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		httpx.Fail(c, httpx.Validation(key+" 参数无效"))
		return nil, false
	}
	return &n, true
}

func (m *Module) list(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, me := who(c)
		q := listQuery{Status: c.Query("status"), Assignee: c.Query("assignee"), Q: strings.TrimSpace(c.Query("q")), Priority: c.Query("priority"), Type: c.Query("type"), Involved: c.Query("involved")}
		if q.Assignee == "me" {
			q.Assignee = me
		}
		if q.Involved == "me" {
			q.Involved = me
		}
		if q.Type != "" && !ItemTypes[q.Type] {
			httpx.Fail(c, httpx.Validation("type 只能是 story 或 task"))
			return
		}
		var ok bool
		if q.Iteration, ok = optID(c, "iteration_id"); !ok {
			return
		}
		if q.Parent, ok = optID(c, "parent_id"); !ok {
			return
		}
		if cb := c.Query("created_by"); cb == "me" {
			q.CreatedBy = &uid
		} else if q.CreatedBy, ok = optID(c, "created_by"); !ok {
			return
		}
		where, args := buildWhere(k, q)
		var total int
		if err := m.d.DB.QueryRow(c, "SELECT count(*) FROM "+table(k)+" t"+where, args...).Scan(&total); err != nil {
			fail(c, err)
			return
		}
		n := len(args)
		rows, err := m.d.DB.Query(c, "SELECT "+prefixed("t", cols)+" FROM "+table(k)+" t"+where+" ORDER BY t.updated_at DESC, t.id DESC LIMIT $"+strconv.Itoa(n+1)+" OFFSET $"+strconv.Itoa(n+2), append(args, lim(c), off(c))...)
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

func prefixed(alias, list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ",")
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

// check 校验并补默认值；full=true 时为创建 / 全量更新。
func (in *input) check(k string, full bool) error {
	if full {
		if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
			return httpx.Validation("请填写标题")
		}
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			return httpx.Validation("标题不能为空")
		}
		if len([]rune(t)) > 200 {
			return httpx.Validation("标题不能超过 200 个字")
		}
		in.Title = &t
	}
	if in.Description != nil && len(*in.Description) > 20000 {
		return httpx.Validation("描述过长（最多 20000 字节）")
	}
	if in.Status != nil && *in.Status != "" && !validStatus(k, *in.Status) {
		return httpx.Validation("状态取值无效：" + *in.Status)
	}
	if in.Priority != nil && *in.Priority != "" && !priorities[*in.Priority] {
		return httpx.Validation("优先级只能是 P0-P3")
	}
	if in.Assignee != nil {
		a := strings.TrimSpace(*in.Assignee)
		if len([]rune(a)) > 64 {
			return httpx.Validation("处理人用户名过长")
		}
		in.Assignee = &a
	}
	if len(in.Data) > 0 && string(in.Data) != "null" {
		var obj map[string]any
		if json.Unmarshal(in.Data, &obj) != nil {
			return httpx.Validation("扩展字段必须是 JSON 对象")
		}
		if len(in.Data) > 16384 {
			return httpx.Validation("扩展字段过大（最多 16 KB）")
		}
		if t, has := obj["type"]; has && k == "requirements" {
			s, _ := t.(string)
			if s != "" && !ItemTypes[s] {
				return httpx.Validation("条目类型只能是需求或任务")
			}
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
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		if err := in.check(k, true); err != nil {
			httpx.Fail(c, err)
			return
		}
		uid, uname := who(c)
		it, err := m.insertItem(c, k, in, uid, uname)
		if err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(it.ID, 10))
		httpx.Created(c, it)
	}
}

// insertItem 在事务内创建对象并写入创建记录。
func (m *Module) insertItem(ctx context.Context, k string, in input, uid int64, uname string) (*Item, error) {
	data := in.Data
	if len(data) == 0 || string(data) == "null" {
		data = json.RawMessage("{}")
	}
	var iter any
	if in.IterationID != nil && k != "iterations" {
		iter = *in.IterationID
	}
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	it, err := scan(tx.QueryRow(ctx, "INSERT INTO "+table(k)+"(title,description,status,priority,iteration_id,assignee,data,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING "+cols,
		*in.Title, str(in.Description, ""), str(in.Status, Statuses[k][0]), str(in.Priority, "P2"), iter, str(in.Assignee, ""), []byte(data), uid, uname))
	if err != nil {
		return nil, err
	}
	if err := writeActivities(ctx, tx, k, it.ID, uname, diffActivities(nil, it)); err != nil {
		return nil, err
	}
	return it, tx.Commit(ctx)
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

// put 全量更新（标题必填）；patch 只改给出的字段，常用于看板拖动改状态、指派。
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
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		var in input
		if json.Unmarshal(body, &in) != nil {
			httpx.Fail(c, httpx.Validation("请求字段类型错误"))
			return
		}
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
			add("assignee", *in.Assignee)
		}
		if len(in.Data) > 0 && string(in.Data) != "null" {
			add("data", []byte(in.Data))
		}
		if len(sets) == 0 {
			httpx.Fail(c, httpx.Validation("没有需要更新的字段"))
			return
		}
		add("updated_by", uname)
		tx, err := m.d.DB.Begin(c)
		if err != nil {
			fail(c, err)
			return
		}
		defer tx.Rollback(c) //nolint:errcheck
		old, err := scan(tx.QueryRow(c, "SELECT "+cols+" FROM "+table(k)+" WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			fail(c, err)
			return
		}
		it, err := scan(tx.QueryRow(c, "UPDATE "+table(k)+" SET "+strings.Join(sets, ",")+",updated_at=now() WHERE id=$1 RETURNING "+cols, args...))
		if err != nil {
			fail(c, err)
			return
		}
		acts := diffActivities(old, it)
		if err := writeActivities(c, tx, k, id, uname, acts); err != nil {
			fail(c, err)
			return
		}
		if err := tx.Commit(c); err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(id, 10))
		if len(acts) > 0 {
			audit.SetDetail(c, acts)
		}
		httpx.OK(c, it)
	}
}

// del 删除对象并在同一事务内清理评论、变更记录和关联。迭代需传 confirm=<迭代名称> 防止误删。
func (m *Module) del(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		tx, err := m.d.DB.Begin(c)
		if err != nil {
			fail(c, err)
			return
		}
		defer tx.Rollback(c) //nolint:errcheck
		var title string
		if err := tx.QueryRow(c, "SELECT title FROM "+table(k)+" WHERE id=$1 FOR UPDATE", id).Scan(&title); err != nil {
			fail(c, err)
			return
		}
		if k == "iterations" && strings.TrimSpace(c.Query("confirm")) != title {
			httpx.Fail(c, httpx.Validation("删除迭代需输入迭代名称确认"))
			return
		}
		if _, err := tx.Exec(c, "DELETE FROM "+table(k)+" WHERE id=$1", id); err != nil {
			fail(c, err)
			return
		}
		for _, q := range []string{"DELETE FROM devcenter_comments WHERE kind=$1 AND item_id=$2", "DELETE FROM devcenter_activities WHERE kind=$1 AND item_id=$2", "DELETE FROM devcenter_links WHERE kind=$1 AND item_id=$2"} {
			if _, err := tx.Exec(c, q, k, id); err != nil {
				fail(c, err)
				return
			}
		}
		if k == "requirements" {
			if _, err := tx.Exec(c, "UPDATE devcenter_requirements SET data=data-'parent_id' WHERE data->>'parent_id'=$1", strconv.FormatInt(id, 10)); err != nil {
				fail(c, err)
				return
			}
		}
		if err := tx.Commit(c); err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_"+k, strconv.FormatInt(id, 10))
		audit.SetDetail(c, gin.H{"title": title})
		httpx.OK(c, gin.H{"id": id})
	}
}

// summary 返回各类型按状态的计数，以及指派给当前用户的未完成数量，供工作台使用。
func (m *Module) summary(c *gin.Context) {
	_, me := who(c)
	out := gin.H{}
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
			if !isDone(k, s) {
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
