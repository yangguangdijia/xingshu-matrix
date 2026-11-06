package devcenter

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

/* ------------------------------------------------------------------ 变更记录 */

// Activity 是一条变更记录：创建、状态流转、指派、迭代调整。
type Activity struct {
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

func iterStr(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

// diffActivities 比较新旧对象，返回需要记录的变更；old 为 nil 表示创建。
func diffActivities(old, cur *Item) []Activity {
	if cur == nil {
		return nil
	}
	if old == nil {
		out := []Activity{{Action: "create", To: cur.Status}}
		if cur.Assignee != "" {
			out = append(out, Activity{Action: "assign", To: cur.Assignee})
		}
		return out
	}
	var out []Activity
	if old.Status != cur.Status {
		out = append(out, Activity{Action: "status", From: old.Status, To: cur.Status})
	}
	if old.Assignee != cur.Assignee {
		out = append(out, Activity{Action: "assign", From: old.Assignee, To: cur.Assignee})
	}
	if iterStr(old.IterationID) != iterStr(cur.IterationID) {
		out = append(out, Activity{Action: "iteration", From: iterStr(old.IterationID), To: iterStr(cur.IterationID)})
	}
	return out
}

func writeActivities(ctx context.Context, tx pgx.Tx, k string, id int64, actor string, acts []Activity) error {
	for _, a := range acts {
		if _, err := tx.Exec(ctx, "INSERT INTO devcenter_activities(kind,item_id,actor,action,from_value,to_value) VALUES($1,$2,$3,$4,$5,$6)", k, id, actor, a.Action, a.From, a.To); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) itemExists(ctx context.Context, k string, id int64) error {
	var x int64
	return m.d.DB.QueryRow(ctx, "SELECT id FROM "+table(k)+" WHERE id=$1", id).Scan(&x)
}

func (m *Module) listActivities(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		rows, err := m.d.DB.Query(c, "SELECT id,actor,action,from_value,to_value,created_at FROM devcenter_activities WHERE kind=$1 AND item_id=$2 ORDER BY id DESC LIMIT 200", k, id)
		if err != nil {
			fail(c, err)
			return
		}
		defer rows.Close()
		out := []Activity{}
		for rows.Next() {
			var a Activity
			if err := rows.Scan(&a.ID, &a.Actor, &a.Action, &a.From, &a.To, &a.CreatedAt); err != nil {
				fail(c, err)
				return
			}
			out = append(out, a)
		}
		httpx.OK(c, gin.H{"items": out})
	}
}

/* ------------------------------------------------------------------ 评论 */

// Comment 是对象下的一条评论。
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	Mine      bool      `json:"mine"`
}

func checkCommentBody(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", httpx.Validation("评论内容不能为空")
	}
	if len([]rune(s)) > 4000 {
		return "", httpx.Validation("评论不能超过 4000 个字")
	}
	return s, nil
}

func (m *Module) listComments(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		_, me := who(c)
		rows, err := m.d.DB.Query(c, "SELECT id,body,author,created_at FROM devcenter_comments WHERE kind=$1 AND item_id=$2 ORDER BY id LIMIT 500", k, id)
		if err != nil {
			fail(c, err)
			return
		}
		defer rows.Close()
		out := []Comment{}
		for rows.Next() {
			var cm Comment
			if err := rows.Scan(&cm.ID, &cm.Body, &cm.Author, &cm.CreatedAt); err != nil {
				fail(c, err)
				return
			}
			cm.Mine = cm.Author == me
			out = append(out, cm)
		}
		httpx.OK(c, gin.H{"items": out})
	}
}

func (m *Module) addComment(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		if c.ShouldBindJSON(&in) != nil {
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		body, err := checkCommentBody(in.Body)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if err := m.itemExists(c, k, id); err != nil {
			fail(c, err)
			return
		}
		uid, me := who(c)
		cm := Comment{Body: body, Author: me, Mine: true}
		if err := m.d.DB.QueryRow(c, "INSERT INTO devcenter_comments(kind,item_id,body,author_id,author) VALUES($1,$2,$3,$4,$5) RETURNING id,created_at", k, id, body, uid, me).Scan(&cm.ID, &cm.CreatedAt); err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_comment", strconv.FormatInt(cm.ID, 10))
		audit.SetDetail(c, gin.H{"kind": k, "item_id": id})
		httpx.Created(c, cm)
	}
}

// canDeleteComment：作者本人或拥有全部权限（*）的管理员可删除评论。
func canDeleteComment(p *rbac.Principal, author string) bool {
	if p == nil {
		return false
	}
	return p.Username == author || slices.Contains(p.Permissions, "*")
}

func (m *Module) delComment(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var author string
	if err := m.d.DB.QueryRow(c, "SELECT author FROM devcenter_comments WHERE id=$1", id).Scan(&author); err != nil {
		fail(c, err)
		return
	}
	if !canDeleteComment(rbac.PrincipalFrom(c), author) {
		httpx.Fail(c, httpx.Forbidden("只能删除自己发表的评论"))
		return
	}
	if _, err := m.d.DB.Exec(c, "DELETE FROM devcenter_comments WHERE id=$1", id); err != nil {
		fail(c, err)
		return
	}
	audit.SetResource(c, "devcenter_comment", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"id": id})
}

/* ------------------------------------------------------------------ 代码仓库 / 流水线关联 */

// Link 把需求或缺陷关联到 CI/CD 应用（代码仓库）或流水线。ref_name 为关联时的名称快照。
type Link struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	ItemID    int64     `json:"item_id"`
	RefType   string    `json:"ref_type"`
	RefID     int64     `json:"ref_id"`
	RefName   string    `json:"ref_name"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// 反查列表时附带对象标题与状态
	ItemTitle  string `json:"item_title,omitempty"`
	ItemStatus string `json:"item_status,omitempty"`
}

type linkInput struct {
	RefType string `json:"ref_type"`
	RefID   int64  `json:"ref_id"`
	RefName string `json:"ref_name"`
}

func (in *linkInput) check() error {
	if in.RefType != "app" && in.RefType != "pipeline" {
		return httpx.Validation("关联类型只能是代码仓库（应用）或流水线")
	}
	if in.RefID <= 0 {
		return httpx.Validation("请选择要关联的代码仓库或流水线")
	}
	in.RefName = strings.TrimSpace(in.RefName)
	if in.RefName == "" {
		return httpx.Validation("缺少关联名称")
	}
	if len([]rune(in.RefName)) > 200 {
		return httpx.Validation("关联名称过长")
	}
	return nil
}

const linkCols = "l.id,l.kind,l.item_id,l.ref_type,l.ref_id,l.ref_name,l.created_by,l.created_at"

func scanLink(row pgx.Row, extra bool) (Link, error) {
	var l Link
	dst := []any{&l.ID, &l.Kind, &l.ItemID, &l.RefType, &l.RefID, &l.RefName, &l.CreatedBy, &l.CreatedAt}
	if extra {
		dst = append(dst, &l.ItemTitle, &l.ItemStatus)
	}
	return l, row.Scan(dst...)
}

func (m *Module) listItemLinks(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		rows, err := m.d.DB.Query(c, "SELECT "+linkCols+" FROM devcenter_links l WHERE l.kind=$1 AND l.item_id=$2 ORDER BY l.id", k, id)
		if err != nil {
			fail(c, err)
			return
		}
		defer rows.Close()
		out := []Link{}
		for rows.Next() {
			l, err := scanLink(rows, false)
			if err != nil {
				fail(c, err)
				return
			}
			out = append(out, l)
		}
		httpx.OK(c, gin.H{"items": out})
	}
}

func (m *Module) addLink(k string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in linkInput
		if c.ShouldBindJSON(&in) != nil {
			httpx.Fail(c, httpx.Validation("请求内容格式错误"))
			return
		}
		if err := in.check(); err != nil {
			httpx.Fail(c, err)
			return
		}
		if err := m.itemExists(c, k, id); err != nil {
			fail(c, err)
			return
		}
		_, me := who(c)
		l, err := scanLink(m.d.DB.QueryRow(c, "INSERT INTO devcenter_links AS l(kind,item_id,ref_type,ref_id,ref_name,created_by) VALUES($1,$2,$3,$4,$5,$6) RETURNING "+linkCols, k, id, in.RefType, in.RefID, in.RefName, me), false)
		if err != nil {
			fail(c, err)
			return
		}
		audit.SetResource(c, "devcenter_link", strconv.FormatInt(l.ID, 10))
		audit.SetDetail(c, gin.H{"kind": k, "item_id": id, "ref_type": in.RefType, "ref_id": in.RefID})
		httpx.Created(c, l)
	}
}

// listLinks 反查：某个代码仓库 / 流水线关联了哪些需求与缺陷；不带参数时返回全部关联（最多 1000 条）。
func (m *Module) listLinks(c *gin.Context) {
	rt := c.Query("ref_type")
	if rt != "" && rt != "app" && rt != "pipeline" {
		httpx.Fail(c, httpx.Validation("ref_type 只能是 app 或 pipeline"))
		return
	}
	ref, ok := optID(c, "ref_id")
	if !ok {
		return
	}
	q := `SELECT ` + linkCols + `, COALESCE(r.title,b.title,''), COALESCE(r.status,b.status,'') FROM devcenter_links l
		LEFT JOIN devcenter_requirements r ON l.kind='requirements' AND r.id=l.item_id
		LEFT JOIN devcenter_bugs b ON l.kind='bugs' AND b.id=l.item_id
		WHERE ($1='' OR l.ref_type=$1) AND ($2::bigint IS NULL OR l.ref_id=$2) ORDER BY l.id DESC LIMIT 1000`
	rows, err := m.d.DB.Query(c, q, rt, ref)
	if err != nil {
		fail(c, err)
		return
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		l, err := scanLink(rows, true)
		if err != nil {
			fail(c, err)
			return
		}
		out = append(out, l)
	}
	httpx.OK(c, gin.H{"items": out})
}

func (m *Module) delLink(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, "DELETE FROM devcenter_links WHERE id=$1", id)
	if err != nil {
		fail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("关联不存在或已解除"))
		return
	}
	audit.SetResource(c, "devcenter_link", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"id": id})
}
