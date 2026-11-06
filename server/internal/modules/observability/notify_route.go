package observability

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// QuietHours suppresses notifications during a daily window (e.g. 22:00-08:00).
type QuietHours struct {
	Enabled       bool   `json:"enabled"`
	Start         string `json:"start"`          // HH:MM
	End           string `json:"end"`            // HH:MM; End <= Start spans midnight
	Weekdays      []int  `json:"weekdays"`       // 0=Sunday … 6; empty = every day; an overnight window belongs to the day it starts
	Timezone      string `json:"timezone"`       // default Asia/Shanghai
	AllowCritical bool   `json:"allow_critical"` // critical events still go out
}

func parseHM(s string) (int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func (q *QuietHours) validate() error {
	if !q.Enabled {
		return nil
	}
	s, ok1 := parseHM(q.Start)
	e, ok2 := parseHM(q.End)
	if !ok1 || !ok2 {
		return httpx.Validation("静默时段格式应为 HH:MM")
	}
	if s == e {
		return httpx.Validation("静默时段开始与结束不能相同")
	}
	if q.Timezone == "" {
		q.Timezone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil {
		return httpx.Validation("时区不正确：" + q.Timezone)
	}
	for _, d := range q.Weekdays {
		if d < 0 || d > 6 {
			return httpx.Validation("星期取值 0~6（0 为周日）")
		}
	}
	return nil
}

// active reports whether now falls in the quiet window.
func (q QuietHours) active(now time.Time) bool {
	if !q.Enabled {
		return false
	}
	s, ok1 := parseHM(q.Start)
	e, ok2 := parseHM(q.End)
	if !ok1 || !ok2 || s == e {
		return false
	}
	loc := time.Local
	if q.Timezone != "" {
		if l, err := time.LoadLocation(q.Timezone); err == nil {
			loc = l
		}
	}
	t := now.In(loc)
	cur := t.Hour()*60 + t.Minute()
	day := int(t.Weekday())
	in := false
	if s < e {
		in = cur >= s && cur < e
	} else { // overnight
		if cur >= s {
			in = true
		} else if cur < e {
			in, day = true, (day+6)%7 // window started yesterday
		}
	}
	if !in {
		return false
	}
	if len(q.Weekdays) == 0 {
		return true
	}
	for _, d := range q.Weekdays {
		if d == day {
			return true
		}
	}
	return false
}

// Subscription routes matching events to channels.
type Subscription struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Enabled      bool              `json:"enabled"`
	EventKinds   []string          `json:"event_kinds"`
	Statuses     []string          `json:"statuses"`   // empty = all statuses of the kinds
	Severities   []string          `json:"severities"` // critical|warning|info; empty = all
	Envs         []string          `json:"envs"`
	Clusters     []string          `json:"clusters"`
	MatchLabels  map[string]string `json:"match_labels"` // value "~re" = regexp, "!v" = not equal, else exact
	ChannelIDs   []int64           `json:"channel_ids"`
	Quiet        QuietHours        `json:"quiet"`
	DedupMinutes int               `json:"dedup_minutes"`
	Aggregate    bool              `json:"aggregate"`
	CreatedBy    string            `json:"created_by"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`

	labelRes map[string]*regexp.Regexp
}

func inList(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

func (s *Subscription) compile() error {
	s.labelRes = map[string]*regexp.Regexp{}
	for k, v := range s.MatchLabels {
		if strings.HasPrefix(v, "~") {
			re, err := regexp.Compile("^(?:" + v[1:] + ")$")
			if err != nil {
				return httpx.Validation("标签 " + k + " 的正则不正确：" + err.Error())
			}
			s.labelRes[k] = re
		}
	}
	return nil
}

// eventLabel looks up a label, falling back to well-known event fields so
// rules can match on them uniformly (cluster, namespace, env, ...).
func eventLabel(ev *NotifyEvent, k string) (string, bool) {
	if v, ok := ev.Labels[k]; ok {
		return v, true
	}
	switch k {
	case "cluster":
		return ev.Cluster, ev.Cluster != ""
	case "namespace":
		return ev.Namespace, ev.Namespace != ""
	case "env":
		return ev.Env, ev.Env != ""
	case "alertname":
		return ev.AlertName, ev.AlertName != ""
	case "severity":
		return ev.Severity, ev.Severity != ""
	}
	return "", false
}

// Match reports whether the subscription routes ev.
func (s *Subscription) Match(ev *NotifyEvent) bool {
	if !s.Enabled || len(s.ChannelIDs) == 0 {
		return false
	}
	if len(s.EventKinds) == 0 || !inList(s.EventKinds, ev.Kind) {
		return false
	}
	if !inList(s.Statuses, ev.Status) {
		return false
	}
	if len(s.Severities) > 0 && !inList(s.Severities, ev.Severity) {
		return false
	}
	if len(s.Envs) > 0 && !inList(s.Envs, ev.Env) {
		return false
	}
	if len(s.Clusters) > 0 && !inList(s.Clusters, ev.Cluster) {
		return false
	}
	if s.labelRes == nil && len(s.MatchLabels) > 0 {
		if s.compile() != nil {
			return false
		}
	}
	for k, want := range s.MatchLabels {
		got, ok := eventLabel(ev, k)
		switch {
		case strings.HasPrefix(want, "~"):
			if re := s.labelRes[k]; re == nil || !re.MatchString(got) {
				return false
			}
		case strings.HasPrefix(want, "!"):
			if ok && got == want[1:] {
				return false
			}
		default:
			if !ok || got != want {
				return false
			}
		}
	}
	return true
}

type subscriptionInput struct {
	Name         string            `json:"name" binding:"required,max=64"`
	Description  string            `json:"description" binding:"max=256"`
	Enabled      *bool             `json:"enabled"`
	EventKinds   []string          `json:"event_kinds"`
	Statuses     []string          `json:"statuses"`
	Severities   []string          `json:"severities"`
	Envs         []string          `json:"envs"`
	Clusters     []string          `json:"clusters"`
	MatchLabels  map[string]string `json:"match_labels"`
	ChannelIDs   []int64           `json:"channel_ids"`
	Quiet        QuietHours        `json:"quiet"`
	DedupMinutes int               `json:"dedup_minutes"`
	Aggregate    bool              `json:"aggregate"`
}

var labelKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.\-/]*$`)

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (in *subscriptionInput) toSub() (*Subscription, error) {
	s := &Subscription{Name: strings.TrimSpace(in.Name), Description: in.Description, Enabled: in.Enabled == nil || *in.Enabled,
		EventKinds: cleanList(in.EventKinds), Statuses: cleanList(in.Statuses), Severities: cleanList(in.Severities),
		Envs: cleanList(in.Envs), Clusters: cleanList(in.Clusters), MatchLabels: map[string]string{},
		Quiet: in.Quiet, DedupMinutes: in.DedupMinutes, Aggregate: in.Aggregate}
	if s.Name == "" {
		return nil, httpx.Validation("请填写订阅名称")
	}
	if len(s.EventKinds) == 0 {
		return nil, httpx.Validation("至少选择一种事件类型")
	}
	allowed := map[string]bool{}
	for _, k := range s.EventKinds {
		if !validKind(k) || k == "test" {
			return nil, httpx.Validation("事件类型必须是 alert|incident|pipeline|approval")
		}
		for _, st := range eventStatuses[k] {
			allowed[st] = true
		}
	}
	for _, st := range s.Statuses {
		if !allowed[st] {
			return nil, httpx.Validation("状态 " + st + " 不属于所选事件类型")
		}
	}
	for _, v := range s.Severities {
		if v != "critical" && v != "warning" && v != "info" {
			return nil, httpx.Validation("级别必须是 critical|warning|info")
		}
	}
	if len(in.MatchLabels) > 32 {
		return nil, httpx.Validation("标签匹配最多 32 项")
	}
	for k, v := range in.MatchLabels {
		k = strings.TrimSpace(k)
		if !labelKeyRe.MatchString(k) {
			return nil, httpx.Validation("标签名不合法：" + k)
		}
		s.MatchLabels[k] = v
	}
	if err := s.compile(); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, id := range in.ChannelIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			s.ChannelIDs = append(s.ChannelIDs, id)
		}
	}
	if len(s.ChannelIDs) == 0 {
		return nil, httpx.Validation("至少选择一个通知渠道")
	}
	if s.DedupMinutes < 0 || s.DedupMinutes > 1440 {
		return nil, httpx.Validation("限频窗口取值 0~1440 分钟")
	}
	if err := s.Quiet.validate(); err != nil {
		return nil, err
	}
	if s.Quiet.Weekdays == nil {
		s.Quiet.Weekdays = []int{}
	}
	return s, nil
}

const subCols = `id,name,description,enabled,event_kinds,statuses,severities,envs,clusters,match_labels,channel_ids,quiet,dedup_minutes,aggregate,created_by,created_at,updated_at`

func scanSub(row pgx.Row) (*Subscription, error) {
	var s Subscription
	var labels, quiet []byte
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Enabled, &s.EventKinds, &s.Statuses, &s.Severities, &s.Envs, &s.Clusters,
		&labels, &s.ChannelIDs, &quiet, &s.DedupMinutes, &s.Aggregate, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("订阅不存在")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(labels, &s.MatchLabels)
	_ = json.Unmarshal(quiet, &s.Quiet)
	if s.MatchLabels == nil {
		s.MatchLabels = map[string]string{}
	}
	if s.ChannelIDs == nil {
		s.ChannelIDs = []int64{}
	}
	if s.Quiet.Weekdays == nil {
		s.Quiet.Weekdays = []int{}
	}
	_ = s.compile()
	return &s, nil
}

func (m *Module) loadSubs(ctx context.Context, onlyEnabled bool) ([]*Subscription, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT `+subCols+` FROM obs_notify_subscriptions WHERE ($1=false OR enabled) ORDER BY id`, onlyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		s, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *Module) checkChannelIDs(ctx context.Context, ids []int64) error {
	var n int
	if err := m.d.DB.QueryRow(ctx, `SELECT count(*) FROM obs_channels WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return err
	}
	if n != len(ids) {
		return httpx.Validation("所选通知渠道不存在或已删除")
	}
	return nil
}

func (m *Module) listSubscriptions(c *gin.Context) {
	subs, err := m.loadSubs(c, false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if subs == nil {
		subs = []*Subscription{}
	}
	httpx.OK(c, subs)
}

func (m *Module) getSubscription(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	s, err := scanSub(m.d.DB.QueryRow(c, `SELECT `+subCols+` FROM obs_notify_subscriptions WHERE id=$1`, id))
	respond(c, s, err)
}

func (m *Module) saveSubscription(c *gin.Context) {
	var id int64
	if c.Param("id") != "" {
		var ok bool
		if id, ok = httpx.ParamID(c, "id"); !ok {
			return
		}
	}
	var in subscriptionInput
	if !httpx.Bind(c, &in) {
		return
	}
	s, err := in.toSub()
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.checkChannelIDs(c, s.ChannelIDs); err != nil {
		httpx.Fail(c, err)
		return
	}
	labels, _ := json.Marshal(s.MatchLabels)
	quiet, _ := json.Marshal(s.Quiet)
	args := []any{s.Name, s.Description, s.Enabled, s.EventKinds, s.Statuses, s.Severities, s.Envs, s.Clusters, labels, s.ChannelIDs, quiet, s.DedupMinutes, s.Aggregate}
	if id == 0 {
		err = m.d.DB.QueryRow(c, `INSERT INTO obs_notify_subscriptions (name,description,enabled,event_kinds,statuses,severities,envs,clusters,match_labels,channel_ids,quiet,dedup_minutes,aggregate,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`, append(args, rbac.PrincipalFrom(c).Username)...).Scan(&id)
	} else {
		tag, e := m.d.DB.Exec(c, `UPDATE obs_notify_subscriptions SET name=$1,description=$2,enabled=$3,event_kinds=$4,statuses=$5,severities=$6,envs=$7,clusters=$8,
			match_labels=$9,channel_ids=$10,quiet=$11,dedup_minutes=$12,aggregate=$13,updated_at=now() WHERE id=$14`, append(args, id)...)
		err = e
		if err == nil && tag.RowsAffected() == 0 {
			err = httpx.NotFound("订阅不存在")
		}
	}
	if err != nil {
		httpx.Fail(c, dupErr(err, "订阅名称已存在"))
		return
	}
	audit.SetResource(c, "obs_notify_subscription", strconv.FormatInt(id, 10))
	out, err := scanSub(m.d.DB.QueryRow(c, `SELECT `+subCols+` FROM obs_notify_subscriptions WHERE id=$1`, id))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if c.Request.Method == "POST" {
		httpx.Created(c, out)
		return
	}
	httpx.OK(c, out)
}

func (m *Module) setSubscriptionEnabled(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	tag, err := m.d.DB.Exec(c, `UPDATE obs_notify_subscriptions SET enabled=$2,updated_at=now() WHERE id=$1`, id, *in.Enabled)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.NotFound("订阅不存在")
	}
	audit.SetResource(c, "obs_notify_subscription", strconv.FormatInt(id, 10))
	respond(c, gin.H{"id": id, "enabled": *in.Enabled}, err)
}

func (m *Module) deleteSubscription(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM obs_notify_subscriptions WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.NotFound("订阅不存在")
	}
	audit.SetResource(c, "obs_notify_subscription", strconv.FormatInt(id, 10))
	respond(c, gin.H{"deleted": id}, err)
}

// matchSubscriptions previews which subscriptions and channels an event would
// reach (the rule editor's "试匹配").
func (m *Module) matchSubscriptions(c *gin.Context) {
	var ev NotifyEvent
	if !httpx.Bind(c, &ev) {
		return
	}
	if !validKind(ev.Kind) {
		httpx.Fail(c, httpx.Validation("事件类型不正确"))
		return
	}
	ev.Severity = normSeverity(firstNonEmpty(ev.Severity, ev.SeverityRaw))
	subs, err := m.loadSubs(c, true)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := []gin.H{}
	now := time.Now()
	for _, s := range subs {
		if s.Match(&ev) {
			out = append(out, gin.H{"id": s.ID, "name": s.Name, "channel_ids": s.ChannelIDs,
				"quiet_now": s.Quiet.active(now) && !(s.Quiet.AllowCritical && ev.Severity == "critical")})
		}
	}
	httpx.OK(c, out)
}
