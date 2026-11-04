package observability

// 值班与故障：分层轮值排班、代班、当前值班计算、故障声明 / 认领 / 解决 / 时间线、SLO 与错误预算。
// 路由挂在 /oncall 下（与前端「值班故障」菜单一致），权限 oncall:read / oncall:manage。

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	PermOncallRead   = "oncall:read"
	PermOncallManage = "oncall:manage"
)

func (m *Module) registerOncall(r *gin.RouterGroup) {
	g := r.Group("/oncall")
	rd, mg := rbac.RequirePerm(PermOncallRead), rbac.RequirePerm(PermOncallManage)
	g.GET("/overview", rd, m.ocOverview)
	g.GET("/current", rd, m.ocCurrent)
	g.GET("/schedules", rd, m.ocListSchedules)
	g.POST("/schedules", mg, m.ocSaveSchedule)
	g.GET("/schedules/:id", rd, m.ocGetSchedule)
	g.PUT("/schedules/:id", mg, m.ocSaveSchedule)
	g.DELETE("/schedules/:id", mg, m.ocDeleteSchedule)
	g.PUT("/schedules/:id/rotations/:layer", mg, m.ocPutRotation)
	g.DELETE("/schedules/:id/rotations/:layer", mg, m.ocDeleteRotation)
	g.POST("/schedules/:id/overrides", mg, m.ocCreateOverride)
	g.DELETE("/overrides/:id", mg, m.ocDeleteOverride)

	g.GET("/incidents", rd, m.ocListIncidents)
	g.POST("/incidents", mg, m.ocCreateIncident)
	g.GET("/incidents/:id", rd, m.ocGetIncident)
	g.POST("/incidents/:id/ack", mg, m.ocIncidentAction("ack"))
	g.POST("/incidents/:id/resolve", mg, m.ocIncidentAction("resolve"))
	g.POST("/incidents/:id/notes", mg, m.ocIncidentAction("note"))

	g.GET("/slos", rd, m.ocListSLOs)
	g.POST("/slos", mg, m.ocSaveSLO)
	g.GET("/slos/:id", rd, m.ocGetSLO)
	g.PUT("/slos/:id", mg, m.ocSaveSLO)
	g.DELETE("/slos/:id", mg, m.ocDeleteSLO)
	g.GET("/slos/:id/budget", rd, m.ocBudget)
	g.GET("/slos/:id/entries", rd, m.ocListEntries)
	g.POST("/slos/:id/entries", mg, m.ocCreateEntry)
	g.DELETE("/slos/:id/entries/:entry", mg, m.ocDeleteEntry)
}

func actor(c *gin.Context) string {
	if p := rbac.PrincipalFrom(c); p != nil {
		return p.Username
	}
	return ""
}

func canEnv(c *gin.Context, env string) bool {
	p := rbac.PrincipalFrom(c)
	return env == "" || p == nil || p.CanEnv(env)
}

func pgFail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Fail(c, httpx.NotFound("not found"))
	case strings.Contains(err.Error(), "SQLSTATE 23505"):
		httpx.Fail(c, httpx.Conflict("name already exists"))
	case strings.Contains(err.Error(), "SQLSTATE 23"):
		httpx.Fail(c, httpx.Validation("invalid value: "+err.Error()))
	default:
		httpx.Fail(c, httpx.Internal(err))
	}
}

func collect(rows pgx.Rows, err error) ([]map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

func one(ctx context.Context, m *Module, sql string, args ...any) (map[string]any, error) {
	rows, err := m.d.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToMap)
}

// ------------------------------------------------------------------ 排班

type scheduleIn struct {
	Name        string `json:"name" binding:"required,max=64"`
	Description string `json:"description"`
	Env         string `json:"env"`
	Timezone    string `json:"timezone"`
	Enabled     *bool  `json:"enabled"`
}

func (m *Module) ocListSchedules(c *gin.Context) {
	items, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_schedules ORDER BY id`))
	if err != nil {
		pgFail(c, err)
		return
	}
	out := []map[string]any{}
	for _, s := range items {
		if canEnv(c, s["env"].(string)) {
			out = append(out, s)
		}
	}
	httpx.OK(c, gin.H{"items": out, "total": len(out)})
}

func (m *Module) ocSaveSchedule(c *gin.Context) {
	var in scheduleIn
	if !httpx.Bind(c, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Timezone == "" {
		in.Timezone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		httpx.Fail(c, httpx.Validation("unknown timezone"))
		return
	}
	if !canEnv(c, in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	en := in.Enabled == nil || *in.Enabled
	var (
		s   map[string]any
		err error
	)
	if c.Param("id") == "" {
		s, err = one(c, m, `INSERT INTO oncall_schedules (name,description,env,timezone,enabled,created_by) VALUES ($1,$2,$3,$4,$5,$6) RETURNING *`,
			in.Name, in.Description, in.Env, in.Timezone, en, actor(c))
	} else {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		s, err = one(c, m, `UPDATE oncall_schedules SET name=$2,description=$3,env=$4,timezone=$5,enabled=$6,updated_at=now() WHERE id=$1 RETURNING *`,
			id, in.Name, in.Description, in.Env, in.Timezone, en)
	}
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_schedule", strconv.FormatInt(s["id"].(int64), 10))
	if c.Param("id") == "" {
		httpx.Created(c, s)
	} else {
		httpx.OK(c, s)
	}
}

func (m *Module) ocGetSchedule(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	s, err := one(c, m, `SELECT * FROM oncall_schedules WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if !canEnv(c, s["env"].(string)) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	rots, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_rotations WHERE schedule_id=$1 ORDER BY layer`, id))
	if err != nil {
		pgFail(c, err)
		return
	}
	ovs, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_overrides WHERE schedule_id=$1 AND ends_at > now() - interval '7 days' ORDER BY starts_at`, id))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"schedule": s, "rotations": rots, "overrides": ovs})
}

func (m *Module) ocDeleteSchedule(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_schedules WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	audit.SetResource(c, "oncall_schedule", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"id": id})
}

type rotationIn struct {
	Name          string    `json:"name"`
	StartAt       time.Time `json:"start_at" binding:"required"`
	LengthSeconds int       `json:"length_seconds" binding:"required"`
	Members       []string  `json:"members" binding:"required,min=1,max=50"`
}

func (m *Module) ocPutRotation(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	layer, err := strconv.Atoi(c.Param("layer"))
	if err != nil || layer < 1 || layer > 9 {
		httpx.Fail(c, httpx.Validation("layer must be 1-9"))
		return
	}
	var in rotationIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.LengthSeconds < 3600 {
		httpx.Fail(c, httpx.Validation("rotation length must be at least 1 hour"))
		return
	}
	members := []string{}
	for _, u := range in.Members {
		if u = strings.TrimSpace(u); u != "" {
			members = append(members, u)
		}
	}
	if len(members) == 0 {
		httpx.Fail(c, httpx.Validation("members required"))
		return
	}
	r, err := one(c, m, `INSERT INTO oncall_rotations (schedule_id,layer,name,start_at,length_seconds,members) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (schedule_id,layer) DO UPDATE SET name=EXCLUDED.name,start_at=EXCLUDED.start_at,length_seconds=EXCLUDED.length_seconds,members=EXCLUDED.members,updated_at=now()
		RETURNING *`, id, layer, in.Name, in.StartAt, in.LengthSeconds, members)
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_schedule", strconv.FormatInt(id, 10))
	httpx.OK(c, r)
}

func (m *Module) ocDeleteRotation(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_rotations WHERE schedule_id=$1 AND layer=$2`, id, c.Param("layer"))
	if err != nil {
		pgFail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	httpx.OK(c, gin.H{"schedule_id": id, "layer": c.Param("layer")})
}

type overrideIn struct {
	Layer    int       `json:"layer" binding:"required,min=1,max=9"`
	Username string    `json:"username" binding:"required,max=64"`
	StartsAt time.Time `json:"starts_at" binding:"required"`
	EndsAt   time.Time `json:"ends_at" binding:"required"`
	Reason   string    `json:"reason"`
}

func (m *Module) ocCreateOverride(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in overrideIn
	if !httpx.Bind(c, &in) {
		return
	}
	if !in.EndsAt.After(in.StartsAt) {
		httpx.Fail(c, httpx.Validation("ends_at must be after starts_at"))
		return
	}
	o, err := one(c, m, `INSERT INTO oncall_overrides (schedule_id,layer,username,starts_at,ends_at,reason,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`,
		id, in.Layer, strings.TrimSpace(in.Username), in.StartsAt, in.EndsAt, in.Reason, actor(c))
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_schedule", strconv.FormatInt(id, 10))
	httpx.Created(c, o)
}

func (m *Module) ocDeleteOverride(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_overrides WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	httpx.OK(c, gin.H{"id": id})
}

// ------------------------------------------------------------------ 当前值班

// Rotation 是计算当前值班所需的轮值层。
type Rotation struct {
	Layer   int
	Start   time.Time
	Length  time.Duration
	Members []string
}

// Override 是代班区间 [Start, End)。
type Override struct {
	ID       int64
	Layer    int
	Username string
	Start    time.Time
	End      time.Time
}

// Shift 是某一层在某时刻的值班结果。
type Shift struct {
	Username     string
	Start, End   time.Time
	NextUsername string
	NextStart    time.Time
	Override     bool
}

// CurrentShift 计算 now 时刻某轮值层的值班人：成员按顺序每 Length 轮换一次；
// 有生效代班时，最晚开始（同开始取最大 id）的代班覆盖轮值。返回 false 表示该层尚未开始。
func CurrentShift(r Rotation, ovs []Override, now time.Time) (Shift, bool) {
	if len(r.Members) == 0 || r.Length <= 0 || now.Before(r.Start) {
		return Shift{}, false
	}
	k := int64(now.Sub(r.Start) / r.Length)
	idx := int(k % int64(len(r.Members)))
	s := Shift{Username: r.Members[idx], Start: r.Start.Add(time.Duration(k) * r.Length)}
	s.End = s.Start.Add(r.Length)
	s.NextUsername, s.NextStart = r.Members[(idx+1)%len(r.Members)], s.End
	var best *Override
	for i := range ovs {
		o := &ovs[i]
		if o.Layer != r.Layer || now.Before(o.Start) || !now.Before(o.End) {
			continue
		}
		if best == nil || o.Start.After(best.Start) || (o.Start.Equal(best.Start) && o.ID > best.ID) {
			best = o
		}
	}
	if best != nil {
		s.Username, s.Start, s.End, s.Override = best.Username, best.Start, best.End, true
	}
	return s, true
}

func (m *Module) currentOncall(ctx context.Context, p *rbac.Principal, scheduleID int64) ([]gin.H, error) {
	now := time.Now()
	scheds, err := collect(m.d.DB.Query(ctx, `SELECT id,name,env FROM oncall_schedules WHERE enabled AND ($1=0 OR id=$1) ORDER BY id`, scheduleID))
	if err != nil {
		return nil, err
	}
	out := []gin.H{}
	for _, s := range scheds {
		env := s["env"].(string)
		if p != nil && env != "" && !p.CanEnv(env) {
			continue
		}
		sid := s["id"].(int64)
		rows, err := m.d.DB.Query(ctx, `SELECT layer,start_at,length_seconds,members FROM oncall_rotations WHERE schedule_id=$1 ORDER BY layer`, sid)
		if err != nil {
			return nil, err
		}
		var rots []Rotation
		for rows.Next() {
			var r Rotation
			var secs int
			if err := rows.Scan(&r.Layer, &r.Start, &secs, &r.Members); err != nil {
				rows.Close()
				return nil, err
			}
			r.Length = time.Duration(secs) * time.Second
			rots = append(rots, r)
		}
		rows.Close()
		orows, err := m.d.DB.Query(ctx, `SELECT id,layer,username,starts_at,ends_at FROM oncall_overrides WHERE schedule_id=$1 AND starts_at<=now() AND ends_at>now()`, sid)
		if err != nil {
			return nil, err
		}
		var ovs []Override
		for orows.Next() {
			var o Override
			if err := orows.Scan(&o.ID, &o.Layer, &o.Username, &o.Start, &o.End); err != nil {
				orows.Close()
				return nil, err
			}
			ovs = append(ovs, o)
		}
		orows.Close()
		for _, r := range rots {
			sh, ok := CurrentShift(r, ovs, now)
			if !ok {
				continue
			}
			out = append(out, gin.H{"schedule_id": sid, "schedule": s["name"], "env": env, "layer": r.Layer, "username": sh.Username,
				"starts_at": sh.Start, "ends_at": sh.End, "override": sh.Override, "next_username": sh.NextUsername, "next_starts_at": sh.NextStart})
		}
	}
	return out, nil
}

func (m *Module) ocCurrent(c *gin.Context) {
	sid, _ := strconv.ParseInt(c.Query("schedule_id"), 10, 64)
	cur, err := m.currentOncall(c, rbac.PrincipalFrom(c), sid)
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": cur})
}

func (m *Module) ocOverview(c *gin.Context) {
	cur, err := m.currentOncall(c, rbac.PrincipalFrom(c), 0)
	if err != nil {
		pgFail(c, err)
		return
	}
	counts := gin.H{"open": 0, "acknowledged": 0, "resolved": 0}
	rows, err := m.d.DB.Query(c, `SELECT status, env, count(*) FROM oncall_incidents GROUP BY status, env`)
	if err != nil {
		pgFail(c, err)
		return
	}
	for rows.Next() {
		var st, env string
		var n int
		if err := rows.Scan(&st, &env, &n); err != nil {
			rows.Close()
			pgFail(c, err)
			return
		}
		if !canEnv(c, env) {
			continue
		}
		if st == "declared" {
			st = "open"
		}
		counts[st] = counts[st].(int) + n
	}
	rows.Close()
	recent, err := collect(m.d.DB.Query(c, `SELECT id,title,severity,status,service,env,declared_at FROM oncall_incidents ORDER BY declared_at DESC LIMIT 20`))
	if err != nil {
		pgFail(c, err)
		return
	}
	rec := []map[string]any{}
	for _, x := range recent {
		if canEnv(c, x["env"].(string)) && len(rec) < 5 {
			rec = append(rec, x)
		}
	}
	slos, err := collect(m.d.DB.Query(c, `SELECT id,name,env FROM oncall_slos ORDER BY id`))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"current": cur, "incidents": counts, "recent_incidents": rec, "slos": slos})
}

// ------------------------------------------------------------------ 故障

type incidentIn struct {
	Title       string `json:"title" binding:"required,max=200"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Service     string `json:"service"`
	Env         string `json:"env"`
	ScheduleID  *int64 `json:"schedule_id"`
	AssignedTo  string `json:"assigned_to"`
}

func (m *Module) ocListIncidents(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	where := `WHERE ($1='' OR status=$1) AND ($2='' OR severity=$2) AND ($3='' OR title ILIKE '%'||$3||'%' OR service ILIKE '%'||$3||'%')`
	args := []any{c.Query("status"), c.Query("severity"), strings.TrimSpace(c.Query("q"))}
	items, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_incidents `+where+` ORDER BY declared_at DESC`, args...))
	if err != nil {
		pgFail(c, err)
		return
	}
	vis := []map[string]any{}
	for _, x := range items {
		if canEnv(c, x["env"].(string)) {
			vis = append(vis, x)
		}
	}
	total := len(vis)
	from := (page - 1) * limit
	if from > total {
		from = total
	}
	to := from + limit
	if to > total {
		to = total
	}
	httpx.OK(c, gin.H{"items": vis[from:to], "total": total, "page": page, "page_size": limit})
}

func (m *Module) ocCreateIncident(c *gin.Context) {
	var in incidentIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Severity == "" {
		in.Severity = "P3"
	}
	if !canEnv(c, in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	assignee, auto := strings.TrimSpace(in.AssignedTo), false
	var sid any
	if in.ScheduleID != nil && *in.ScheduleID > 0 {
		sid = *in.ScheduleID
		if assignee == "" {
			cur, err := m.currentOncall(c, nil, *in.ScheduleID)
			if err != nil {
				pgFail(c, err)
				return
			}
			best := 99
			for _, w := range cur {
				if l := w["layer"].(int); l < best {
					best, assignee, auto = l, w["username"].(string), true
				}
			}
		}
	}
	who := actor(c)
	tx, err := m.d.DB.Begin(c)
	if err != nil {
		pgFail(c, err)
		return
	}
	defer tx.Rollback(c) //nolint:errcheck
	rows, err := tx.Query(c, `INSERT INTO oncall_incidents (title,description,severity,service,env,schedule_id,assigned_to,declared_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *`,
		strings.TrimSpace(in.Title), in.Description, in.Severity, in.Service, in.Env, sid, assignee, who)
	if err != nil {
		pgFail(c, err)
		return
	}
	x, err := pgx.CollectExactlyOneRow(rows, pgx.RowToMap)
	if err != nil {
		pgFail(c, err)
		return
	}
	id := x["id"].(int64)
	msg := in.Severity + " 故障已声明"
	if _, err = tx.Exec(c, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,'declared',$2,$3)`, id, who, msg); err == nil && assignee != "" {
		how := "手动指派给 " + assignee
		if auto {
			how = "按排班自动指派给一线值班 " + assignee
		}
		_, err = tx.Exec(c, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,'assigned',$2,$3)`, id, who, how)
	}
	if err != nil {
		pgFail(c, err)
		return
	}
	if err := tx.Commit(c); err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_incident", strconv.FormatInt(id, 10))
	httpx.Created(c, x)
}

func (m *Module) ocGetIncident(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	x, err := one(c, m, `SELECT * FROM oncall_incidents WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if !canEnv(c, x["env"].(string)) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	tl, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_incident_events WHERE incident_id=$1 ORDER BY id`, id))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"incident": x, "timeline": tl})
}

func (m *Module) ocIncidentAction(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in struct {
			Comment string `json:"comment"`
			Note    string `json:"note"`
			Message string `json:"message"`
		}
		_ = c.ShouldBindJSON(&in)
		x, err := one(c, m, `SELECT status, env FROM oncall_incidents WHERE id=$1`, id)
		if err != nil {
			pgFail(c, err)
			return
		}
		if !canEnv(c, x["env"].(string)) {
			httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
			return
		}
		st, who := x["status"].(string), actor(c)
		var upd, evKind, msg string
		switch kind {
		case "ack":
			if st != "declared" {
				httpx.Fail(c, httpx.Conflict("only a declared incident can be acknowledged"))
				return
			}
			upd = `UPDATE oncall_incidents SET status='acknowledged',acked_by=$2,acked_at=now(),assigned_to=CASE WHEN assigned_to='' THEN $2 ELSE assigned_to END,updated_at=now() WHERE id=$1 AND status='declared'`
			evKind, msg = "acknowledged", firstNonEmpty(in.Comment, "已认领，开始处理")
		case "resolve":
			if st == "resolved" {
				httpx.Fail(c, httpx.Conflict("incident already resolved"))
				return
			}
			upd = `UPDATE oncall_incidents SET status='resolved',resolved_by=$2,resolved_at=now(),resolve_note=$3,updated_at=now() WHERE id=$1 AND status<>'resolved'`
			evKind, msg = "resolved", firstNonEmpty(in.Note, "已解决")
		default:
			msg = strings.TrimSpace(firstNonEmpty(in.Message, in.Comment))
			if msg == "" {
				httpx.Fail(c, httpx.Validation("message required"))
				return
			}
			evKind = "note"
		}
		tx, err := m.d.DB.Begin(c)
		if err != nil {
			pgFail(c, err)
			return
		}
		defer tx.Rollback(c) //nolint:errcheck
		if upd != "" {
			args := []any{id, who}
			if kind == "resolve" {
				args = append(args, in.Note)
			}
			tag, err := tx.Exec(c, upd, args...)
			if err != nil {
				pgFail(c, err)
				return
			}
			if tag.RowsAffected() == 0 {
				httpx.Fail(c, httpx.Conflict("incident status changed, refresh and retry"))
				return
			}
		}
		if _, err := tx.Exec(c, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,$2,$3,$4)`, id, evKind, who, msg); err != nil {
			pgFail(c, err)
			return
		}
		if err := tx.Commit(c); err != nil {
			pgFail(c, err)
			return
		}
		audit.SetResource(c, "oncall_incident", strconv.FormatInt(id, 10))
		httpx.OK(c, gin.H{"id": id, "event": evKind})
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ------------------------------------------------------------------ SLO

type sloIn struct {
	Name         string  `json:"name" binding:"required,max=100"`
	Service      string  `json:"service"`
	Env          string  `json:"env"`
	Description  string  `json:"description"`
	Target       float64 `json:"target" binding:"required"`
	WindowDays   int     `json:"window_days"`
	Source       string  `json:"source"`
	SLIType      string  `json:"sli_type"`
	DatasourceID *int64  `json:"datasource_id"`
	GoodQuery    string  `json:"good_query"`
	TotalQuery   string  `json:"total_query"`
	Query        string  `json:"query"`
}

func (in *sloIn) check() error {
	if !(in.Target > 0 && in.Target <= 100) {
		return httpx.Validation("target must be in (0,100]")
	}
	if in.WindowDays == 0 {
		in.WindowDays = 30
	}
	if in.WindowDays < 1 || in.WindowDays > 365 {
		return httpx.Validation("window_days must be 1-365")
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.SLIType == "" {
		in.SLIType = "ratio"
	}
	if in.Source != "manual" && in.Source != "prometheus" {
		return httpx.Validation("source must be manual or prometheus")
	}
	if in.SLIType != "ratio" && in.SLIType != "query" {
		return httpx.Validation("sli_type must be ratio or query")
	}
	if in.Source == "prometheus" {
		if in.DatasourceID == nil || *in.DatasourceID <= 0 {
			return httpx.Validation("prometheus source requires datasource_id")
		}
		if in.SLIType == "ratio" && (strings.TrimSpace(in.GoodQuery) == "" || strings.TrimSpace(in.TotalQuery) == "") {
			return httpx.Validation("ratio SLI requires good_query and total_query")
		}
		if in.SLIType == "query" && strings.TrimSpace(in.Query) == "" {
			return httpx.Validation("query SLI requires query")
		}
	}
	return nil
}

func (m *Module) ocListSLOs(c *gin.Context) {
	items, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_slos ORDER BY id`))
	if err != nil {
		pgFail(c, err)
		return
	}
	out := []map[string]any{}
	for _, s := range items {
		if canEnv(c, s["env"].(string)) {
			out = append(out, normSLO(s))
		}
	}
	httpx.OK(c, gin.H{"items": out, "total": len(out)})
}

// normSLO 把 NUMERIC 目标值转成数字，便于前端直接显示。
func normSLO(s map[string]any) map[string]any {
	if v, ok := s["target"]; ok {
		if b, err := json.Marshal(v); err == nil {
			var f float64
			if json.Unmarshal(b, &f) == nil {
				s["target"] = f
			} else if str := strings.Trim(string(b), `"`); str != "" {
				if f, err := strconv.ParseFloat(str, 64); err == nil {
					s["target"] = f
				}
			}
		}
	}
	return s
}

func (m *Module) ocGetSLO(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	s, err := one(c, m, `SELECT * FROM oncall_slos WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, normSLO(s))
}

func (m *Module) ocSaveSLO(c *gin.Context) {
	var in sloIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	if !canEnv(c, in.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	var ds any
	if in.Source == "prometheus" {
		if _, err := m.loadDS(c, rbac.PrincipalFrom(c), *in.DatasourceID, "prometheus"); err != nil {
			httpx.Fail(c, err)
			return
		}
		ds = *in.DatasourceID
	}
	var (
		s   map[string]any
		err error
	)
	args := []any{strings.TrimSpace(in.Name), in.Service, in.Env, in.Description, in.Target, in.WindowDays, in.Source, in.SLIType, ds, in.GoodQuery, in.TotalQuery, in.Query}
	if c.Param("id") == "" {
		s, err = one(c, m, `INSERT INTO oncall_slos (name,service,env,description,target,window_days,source,sli_type,datasource_id,good_query,total_query,query,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING *`, append(args, actor(c))...)
	} else {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		s, err = one(c, m, `UPDATE oncall_slos SET name=$2,service=$3,env=$4,description=$5,target=$6,window_days=$7,source=$8,sli_type=$9,datasource_id=$10,
			good_query=$11,total_query=$12,query=$13,updated_at=now() WHERE id=$1 RETURNING *`, append([]any{id}, args...)...)
	}
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_slo", strconv.FormatInt(s["id"].(int64), 10))
	if c.Param("id") == "" {
		httpx.Created(c, normSLO(s))
	} else {
		httpx.OK(c, normSLO(s))
	}
}

func (m *Module) ocDeleteSLO(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_slos WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	audit.SetResource(c, "oncall_slo", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"id": id})
}

func (m *Module) ocListEntries(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	items, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_slo_entries WHERE slo_id=$1 ORDER BY period_start DESC LIMIT 500`, id))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}

func (m *Module) ocCreateEntry(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in struct {
		PeriodStart time.Time `json:"period_start" binding:"required"`
		PeriodEnd   time.Time `json:"period_end" binding:"required"`
		Good        int64     `json:"good"`
		Total       int64     `json:"total"`
		Note        string    `json:"note"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Total <= 0 || in.Good < 0 || in.Good > in.Total {
		httpx.Fail(c, httpx.Validation("require 0 <= good <= total and total > 0"))
		return
	}
	if !in.PeriodEnd.After(in.PeriodStart) {
		httpx.Fail(c, httpx.Validation("period_end must be after period_start"))
		return
	}
	s, err := one(c, m, `SELECT source FROM oncall_slos WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	if s["source"] != "manual" {
		httpx.Fail(c, httpx.Conflict("only manual SLOs accept entries"))
		return
	}
	e, err := one(c, m, `INSERT INTO oncall_slo_entries (slo_id,period_start,period_end,good,total,note,created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *`,
		id, in.PeriodStart, in.PeriodEnd, in.Good, in.Total, in.Note, actor(c))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.Created(c, e)
}

func (m *Module) ocDeleteEntry(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_slo_entries WHERE slo_id=$1 AND id=$2`, id, c.Param("entry"))
	if err != nil {
		pgFail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	httpx.OK(c, gin.H{"id": c.Param("entry")})
}

// Budget 是错误预算计算结果。SLI 为 0–1 比率，RemainingPct 为剩余预算百分比（可为负，表示超支）。
type Budget struct {
	SLI          *float64 `json:"sli"`
	Target       float64  `json:"target"`
	RemainingPct *float64 `json:"remaining_pct"`
	BurnRate     *float64 `json:"burn_rate"`
	Healthy      bool     `json:"healthy"`
	HasData      bool     `json:"has_data"`
	Good         float64  `json:"good,omitempty"`
	Total        float64  `json:"total,omitempty"`
}

// ComputeBudget 由 good/total 和目标（百分比）计算错误预算：
// 允许失败率 = 1 - target/100；消耗 = 实际失败率 / 允许失败率；剩余 = (1 - 消耗) × 100%。
// 燃烧率按「已过窗口比例」折算：elapsed 为窗口内已经过的比例（0–1]，燃烧率 = 消耗 / elapsed。
func ComputeBudget(good, total, target, elapsed float64) Budget {
	b := Budget{Target: target, Good: good, Total: total}
	if total <= 0 {
		b.Healthy = true
		return b
	}
	b.HasData = true
	sli := good / total
	return budgetFromSLI(b, sli, elapsed)
}

func budgetFromSLI(b Budget, sli, elapsed float64) Budget {
	b.HasData = true
	sli = math.Max(0, math.Min(1, sli))
	allowed := 1 - b.Target/100
	var consumed float64
	if allowed <= 0 {
		if sli < 1 {
			consumed = math.Inf(1)
		}
	} else {
		consumed = (1 - sli) / allowed
	}
	remaining := (1 - consumed) * 100
	if math.IsInf(remaining, -1) {
		remaining = -100
	}
	if elapsed <= 0 || elapsed > 1 {
		elapsed = 1
	}
	burn := consumed / elapsed
	if math.IsInf(burn, 1) {
		burn = 999
	}
	b.SLI, b.RemainingPct, b.BurnRate = &sli, &remaining, &burn
	b.Healthy = remaining > 0 && burn <= 1
	return b
}

func (m *Module) ocBudget(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	s, err := one(c, m, `SELECT * FROM oncall_slos WHERE id=$1`, id)
	if err != nil {
		pgFail(c, err)
		return
	}
	s = normSLO(s)
	if !canEnv(c, s["env"].(string)) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	target, _ := s["target"].(float64)
	days := int(s["window_days"].(int32))
	to := time.Now()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	resp := gin.H{"slo": s, "source": s["source"], "from": from, "to": to}
	if s["source"] == "manual" {
		rows, err := m.d.DB.Query(c, `SELECT period_start, period_end, good, total FROM oncall_slo_entries WHERE slo_id=$1 AND period_end > $2 ORDER BY period_end`, id, from)
		if err != nil {
			pgFail(c, err)
			return
		}
		var good, total float64
		var first time.Time
		points := []gin.H{}
		for rows.Next() {
			var ps, pe time.Time
			var g, t int64
			if err := rows.Scan(&ps, &pe, &g, &t); err != nil {
				rows.Close()
				pgFail(c, err)
				return
			}
			if first.IsZero() {
				first = ps
			}
			good += float64(g)
			total += float64(t)
			cb := ComputeBudget(good, total, target, 1)
			points = append(points, gin.H{"ts": pe, "remaining_pct": cb.RemainingPct})
		}
		rows.Close()
		elapsed := 1.0
		if !first.IsZero() && first.After(from) {
			elapsed = to.Sub(first).Hours() / to.Sub(from).Hours()
		}
		resp["budget"] = ComputeBudget(good, total, target, elapsed)
		resp["points"] = points
		httpx.OK(c, resp)
		return
	}
	// prometheus：查询语句里的 $window 会替换成窗口长度（例如 30d）
	dsID, _ := s["datasource_id"].(int64)
	ds, err := m.loadDS(c, rbac.PrincipalFrom(c), dsID, "prometheus")
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	cl := newClient(ds)
	win := strconv.Itoa(days) + "d"
	scalar := func(q string) (float64, bool, error) {
		q = strings.ReplaceAll(q, "$window", win)
		env, err := cl.PromInstant(c, q, "")
		if err != nil {
			return 0, false, err
		}
		return firstSample(env.Data)
	}
	b := Budget{Target: target, Healthy: true}
	if s["sli_type"] == "query" {
		v, has, err := scalar(s["query"].(string))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if has {
			b = budgetFromSLI(b, v, 1)
		}
	} else {
		g, hg, err := scalar(s["good_query"].(string))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		t, ht, err := scalar(s["total_query"].(string))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if hg && ht {
			b = ComputeBudget(g, t, target, 1)
		}
	}
	resp["budget"] = b
	resp["points"] = []gin.H{}
	httpx.OK(c, resp)
}

// firstSample 取 Prometheus 即时查询结果（vector 或 scalar）的第一个值。
func firstSample(raw json.RawMessage) (float64, bool, error) {
	var d struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return 0, false, httpx.Validation("unexpected Prometheus response")
	}
	parse := func(v []any) (float64, bool) {
		if len(v) != 2 {
			return 0, false
		}
		s, _ := v[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	switch d.ResultType {
	case "scalar":
		var v []any
		if json.Unmarshal(d.Result, &v) == nil {
			f, ok := parse(v)
			return f, ok, nil
		}
	case "vector":
		var vs []struct {
			Value []any `json:"value"`
		}
		if json.Unmarshal(d.Result, &vs) == nil && len(vs) > 0 {
			f, ok := parse(vs[0].Value)
			return f, ok, nil
		}
	}
	return 0, false, nil
}
