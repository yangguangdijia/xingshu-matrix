package observability

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const recCols = `id,channel_id,channel_name,channel_type,subscription_id,subscription_name,event_kind,event_status,event_key,severity,env,cluster,
	title,body,event_json,status,reason,duration_ms,resend_of,triggered_by,created_at`

func scanRecord(row pgx.Row) (*notifyRecord, error) {
	var r notifyRecord
	var ev []byte
	err := row.Scan(&r.ID, &r.ChannelID, &r.ChannelName, &r.ChannelType, &r.SubscriptionID, &r.SubscriptionName, &r.EventKind, &r.EventStatus,
		&r.EventKey, &r.Severity, &r.Env, &r.Cluster, &r.Title, &r.Body, &ev, &r.Status, &r.Reason, &r.DurationMS, &r.ResendOf, &r.TriggeredBy, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("发送记录不存在")
	}
	if err != nil {
		return nil, err
	}
	if len(ev) > 2 {
		r.EventJSON = ev
	}
	return &r, nil
}

// envScope returns the caller's environment restriction, nil = unrestricted.
func envScope(p *rbac.Principal) []string {
	if p == nil || p.EnvScope == nil {
		return nil
	}
	for _, e := range p.EnvScope {
		if e == "*" {
			return nil
		}
	}
	return append([]string{}, p.EnvScope...)
}

// recordFilter builds the WHERE clause for the record list (pure, unit-tested).
func recordFilter(q map[string]string, scope []string) (string, []any, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	for _, k := range []string{"channel_id", "subscription_id", "resend_of"} {
		if v := q[k]; v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return "", nil, httpx.Validation(k + " 必须是正整数")
			}
			add(k+"=?", n)
		}
	}
	if v := q["event_kind"]; v != "" {
		if !validKind(v) {
			return "", nil, httpx.Validation("事件类型不正确")
		}
		add("event_kind=?", v)
	}
	if v := q["status"]; v != "" {
		if v != "success" && v != "failed" && v != "suppressed" {
			return "", nil, httpx.Validation("状态必须是 success|failed|suppressed")
		}
		add("status=?", v)
	}
	for _, k := range []string{"env", "cluster", "severity", "channel_type", "event_status"} {
		if v := q[k]; v != "" {
			add(k+"=?", v)
		}
	}
	if v := strings.TrimSpace(q["q"]); v != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v) + "%"
		add("(title ILIKE ? OR channel_name ILIKE ? OR event_key ILIKE ? OR reason ILIKE ?)", like)
	}
	for _, k := range []string{"since", "until"} {
		if v := q[k]; v != "" {
			t, err := parseQueryTime(v)
			if err != nil {
				return "", nil, httpx.Validation(k + " 时间格式应为 RFC3339 或 Unix 秒")
			}
			op := ">="
			if k == "until" {
				op = "<"
			}
			add("created_at "+op+" ?", t)
		}
	}
	if scope != nil {
		add("(env = '' OR env = ANY(?))", scope)
	}
	if len(where) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(where, " AND "), args, nil
}

func parseQueryTime(v string) (time.Time, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.Parse(time.RFC3339, v)
}

func (m *Module) listRecords(c *gin.Context) {
	q := map[string]string{}
	for _, k := range []string{"channel_id", "subscription_id", "resend_of", "event_kind", "status", "env", "cluster", "severity", "channel_type", "event_status", "q", "since", "until"} {
		q[k] = c.Query(k)
	}
	where, args, err := recordFilter(q, envScope(rbac.PrincipalFrom(c)))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	pg := httpx.ParsePage(c)
	var total int64
	if err := m.d.DB.QueryRow(c, `SELECT count(*) FROM obs_notify_records`+where, args...).Scan(&total); err != nil {
		httpx.Fail(c, err)
		return
	}
	n := len(args)
	rows, err := m.d.DB.Query(c, `SELECT `+recCols+` FROM obs_notify_records`+where+` ORDER BY id DESC LIMIT $`+strconv.Itoa(n+1)+` OFFSET $`+strconv.Itoa(n+2),
		append(args, pg.Limit(), pg.Offset())...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	items := []notifyRecord{}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		r.Body, r.EventJSON = "", nil // list stays light; GET /records/:id has the content
		items = append(items, *r)
	}
	var stats struct {
		Success    int64 `json:"success"`
		Failed     int64 `json:"failed"`
		Suppressed int64 `json:"suppressed"`
	}
	_ = m.d.DB.QueryRow(c, `SELECT count(*) FILTER (WHERE status='success'), count(*) FILTER (WHERE status='failed'), count(*) FILTER (WHERE status='suppressed')
		FROM obs_notify_records WHERE created_at > now() - interval '24 hours'`).Scan(&stats.Success, &stats.Failed, &stats.Suppressed)
	res := httpx.NewPageResult(items, total, pg)
	httpx.OK(c, gin.H{"items": res.Items, "total": res.Total, "page": res.Page, "page_size": res.PageSize, "last_24h": stats})
}

func (m *Module) loadRecord(c *gin.Context) (*notifyRecord, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, false
	}
	r, err := scanRecord(m.d.DB.QueryRow(c, `SELECT `+recCols+` FROM obs_notify_records WHERE id=$1`, id))
	if err == nil && r.Env != "" && !canEnv(c, r.Env) {
		err = httpx.Forbidden("无权查看该环境的记录")
	}
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return r, true
}

func (m *Module) getRecord(c *gin.Context) {
	if r, ok := m.loadRecord(c); ok {
		httpx.OK(c, r)
	}
}

// resendRecord re-sends the stored title/body of a record through the
// channel's current configuration and records a new attempt.
func (m *Module) resendRecord(c *gin.Context) {
	r, ok := m.loadRecord(c)
	if !ok {
		return
	}
	if r.Status == "success" && c.Query("force") != "true" {
		httpx.Fail(c, httpx.Conflict("该记录已发送成功，如需再次发送请确认"))
		return
	}
	ch, err := m.channelByID(c, r.ChannelID)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("渠道已删除，无法重发"))
		return
	}
	if !ch.Enabled {
		httpx.Fail(c, httpx.Conflict("渠道已停用，请先启用后再重发"))
		return
	}
	title, body := r.Title, r.Body
	var ev *NotifyEvent
	if len(r.EventJSON) > 0 {
		var e NotifyEvent
		if json.Unmarshal(r.EventJSON, &e) == nil && e.Kind != "" {
			ev = &e
		}
	}
	if r.Status == "suppressed" || body == "" {
		// suppressed records were never rendered
		if ev == nil {
			httpx.Fail(c, httpx.Conflict("记录缺少事件内容，无法重发"))
			return
		}
		if title, body, err = m.renderFor(c, ch, ev); err != nil {
			httpx.Fail(c, httpx.Validation(err.Error()))
			return
		}
	}
	start := time.Now()
	err = m.sender(c).send(c, ch, Message{Title: title, Body: body, Event: ev})
	orig := r.ID
	nr := &notifyRecord{ChannelID: ch.ID, ChannelName: ch.Name, ChannelType: ch.Type, SubscriptionID: r.SubscriptionID, SubscriptionName: r.SubscriptionName,
		EventKind: r.EventKind, EventStatus: r.EventStatus, EventKey: r.EventKey, Severity: r.Severity, Env: r.Env, Cluster: r.Cluster,
		Title: title, Body: body, EventJSON: r.EventJSON, DurationMS: int(time.Since(start).Milliseconds()), ResendOf: &orig,
		TriggeredBy: rbac.PrincipalFrom(c).Username}
	nr.setResult(err)
	m.saveRecord(c, nr)
	audit.SetResource(c, "obs_notify_record", strconv.FormatInt(orig, 10))
	res := gin.H{"ok": err == nil, "record_id": nr.ID, "duration_ms": nr.DurationMS}
	if err != nil {
		res["error"] = err.Error()
	}
	httpx.OK(c, res)
}
