package observability

// 告警认领 / 时间线 / 转故障（前端 public/xs-live-ops.js「告警事件」页）：
//   POST /observability/alerts/:id/claim     alert:handle（silence=true 时另需 alert:silence）
//   GET  /observability/alerts/:id/timeline  alert:read
//   POST /observability/alerts/:id/incident  alert:handle + oncall:manage
// :id 可以是告警数字 id 或 fingerprint（同一 fingerprint 存在于多个数据源时需 datasource_id/datasource 消歧）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ------------------------------------------------------------------ pure logic

// ClaimDecision is the outcome of checking whether user may claim an alert.
type ClaimDecision int

const (
	ClaimAllowed      ClaimDecision = iota // open：可认领
	ClaimAlreadyMine                       // 已由本人认领：幂等返回
	ClaimTakenByOther                      // 已被他人认领：409
	ClaimClosed                            // 已关闭：409
)

// DecideClaim decides a claim attempt from the alert's handling state.
func DecideClaim(handleStatus, claimedBy, user string) ClaimDecision {
	switch {
	case handleStatus == "closed":
		return ClaimClosed
	case handleStatus == "acked" && claimedBy != "" && claimedBy == user:
		return ClaimAlreadyMine
	case handleStatus == "acked" && claimedBy != "":
		return ClaimTakenByOther
	default: // open，或 acked 但认领人为空（异常数据）视为可认领
		return ClaimAllowed
	}
}

// claimError maps a non-allowed decision to the Chinese HTTP error.
func claimError(d ClaimDecision, claimedBy string) error {
	switch d {
	case ClaimTakenByOther:
		return httpx.Conflict("告警已被 " + claimedBy + " 认领，如需接手请先与其确认")
	case ClaimClosed:
		return httpx.Conflict("告警已关闭，无法认领")
	}
	return nil
}

// ParseClaimDuration validates the silence duration of a claim (default 2h, 5m–72h).
func ParseClaimDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "2h"
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 5*time.Minute || d > 72*time.Hour {
		return 0, httpx.Validation("静默时长无效，应在 5m 到 72h 之间（如 1h、2h、24h）")
	}
	return d, nil
}

// IncidentSeverityFor derives a default incident severity from an alert severity.
func IncidentSeverityFor(alertSeverity string) string {
	s := strings.ToLower(strings.TrimSpace(alertSeverity))
	switch s {
	case "critical", "p0", "p1", "fatal", "emergency", "disaster":
		return "P1"
	case "info", "p4", "none":
		return "P3"
	}
	return "P2"
}

// TimelineEvent is one entry of an alert timeline.
type TimelineEvent struct {
	At      time.Time `json:"created_at"`
	Kind    string    `json:"kind"` // triggered|notified|notify_failed|notify_suppressed|claimed|silenced|resolved|closed|incident|incident_resolved
	Actor   string    `json:"actor"`
	Message string    `json:"message"`
	Ref     string    `json:"ref,omitempty"` // 关联对象：notify record id / silence id / incident id
}

// timelineOrder breaks ties when two events share a timestamp, in causal order.
var timelineOrder = map[string]int{
	"triggered": 0, "notified": 1, "notify_failed": 1, "notify_suppressed": 1,
	"claimed": 2, "silenced": 3, "incident": 4, "resolved": 5, "incident_resolved": 6, "closed": 7,
}

// SortTimeline orders events by time ascending; ties follow the causal kind order,
// then the original order (stable). Zero-time events are dropped.
func SortTimeline(in []TimelineEvent) []TimelineEvent {
	out := make([]TimelineEvent, 0, len(in))
	for _, e := range in {
		if !e.At.IsZero() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		oi, ok := timelineOrder[out[i].Kind]
		if !ok {
			oi = 99
		}
		oj, ok := timelineOrder[out[j].Kind]
		if !ok {
			oj = 99
		}
		return oi < oj
	})
	return out
}

// claimState is the stored claim information of an alert (0009 columns).
type claimState struct {
	Comment      string
	SilenceID    string
	SilenceUntil *time.Time
}

// alertTimelineEvents derives events from the alert row itself.
func alertTimelineEvents(a *Alert, cs claimState) []TimelineEvent {
	var out []TimelineEvent
	trig := a.CreatedAt
	if a.StartsAt != nil {
		trig = *a.StartsAt
	}
	msg := "告警触发：" + firstNonEmpty(a.Alertname, a.Fingerprint)
	if a.Severity != "" {
		msg += "（级别 " + a.Severity + "）"
	}
	if s := firstNonEmpty(a.Annotations["summary"], a.Annotations["message"]); s != "" {
		msg += "，" + s
	}
	if a.FireCount > 1 {
		msg += "；累计触发 " + strconv.Itoa(a.FireCount) + " 次，此为最近一次"
	}
	out = append(out, TimelineEvent{At: trig, Kind: "triggered", Actor: "Alertmanager", Message: msg})
	if a.AckedBy != "" && a.AckedAt != nil {
		m := "认领告警"
		if cs.Comment != "" {
			m += "：" + cs.Comment
		}
		out = append(out, TimelineEvent{At: *a.AckedAt, Kind: "claimed", Actor: a.AckedBy, Message: m})
		if cs.SilenceID != "" {
			m := "认领时在 Alertmanager 创建静默 " + cs.SilenceID
			if cs.SilenceUntil != nil {
				m += "，至 " + cs.SilenceUntil.Format("2006-01-02 15:04:05 MST")
			}
			// 静默紧随认领（同一时刻，排序时排在认领之后）
			out = append(out, TimelineEvent{At: *a.AckedAt, Kind: "silenced", Actor: a.AckedBy, Message: m, Ref: cs.SilenceID})
		}
	}
	if a.Status == "resolved" {
		at := a.LastReceived
		if a.EndsAt != nil {
			at = *a.EndsAt
		}
		out = append(out, TimelineEvent{At: at, Kind: "resolved", Actor: "Alertmanager", Message: "告警已恢复"})
	}
	if a.ClosedAt != nil {
		m := "关闭告警"
		if a.CloseComment != "" {
			m += "：" + a.CloseComment
		}
		out = append(out, TimelineEvent{At: *a.ClosedAt, Kind: "closed", Actor: a.ClosedBy, Message: m})
	}
	return out
}

// notifyTimelineEvent converts one notify record into a timeline entry.
func notifyTimelineEvent(id int64, channel, eventStatus, status, reason, by string, at time.Time) TimelineEvent {
	what := "触发通知"
	if eventStatus == "resolved" {
		what = "恢复通知"
	}
	e := TimelineEvent{At: at, Actor: firstNonEmpty(by, "通知中心"), Ref: strconv.FormatInt(id, 10)}
	switch status {
	case "success":
		e.Kind, e.Message = "notified", what+"已发送至「"+channel+"」"
	case "suppressed":
		e.Kind, e.Message = "notify_suppressed", what+"未发送至「"+channel+"」（已抑制）"
	default:
		e.Kind, e.Message = "notify_failed", what+"发送至「"+channel+"」失败"
	}
	if reason != "" {
		e.Message += "：" + reason
	}
	return e
}

// alertNotifyKeyPrefix is the notify event key prefix of an alert (see alertEvent).
func alertNotifyKeyPrefix(a *Alert) string {
	fp := a.Fingerprint
	if fp == "" {
		fp = strconv.FormatInt(a.ID, 10)
	}
	return "alert:" + strconv.FormatInt(a.DatasourceID, 10) + ":" + fp + ":"
}

// ------------------------------------------------------------------ loading

// loadAlertByKey loads :id as numeric id or fingerprint, authorizing against cluster_id.
func (m *Module) loadAlertByKey(c *gin.Context, dsHint int64) (*Alert, bool) {
	key := strings.TrimSpace(c.Param("id"))
	if key == "" {
		httpx.Fail(c, httpx.BadRequest("缺少告警 id"))
		return nil, false
	}
	if dsHint == 0 {
		dsHint, _ = strconv.ParseInt(firstNonEmpty(c.Query("datasource_id"), c.Query("datasource")), 10, 64)
	}
	p := rbac.PrincipalFrom(c)
	clusterID := c.Query("cluster_id")
	// 纯数字且短于 fingerprint 长度（16 位十六进制）时按 id 解析
	if id, err := strconv.ParseInt(key, 10, 64); err == nil && id > 0 && len(key) < 16 {
		a, err := scanAlert(m.d.DB.QueryRow(c, `SELECT `+alertCols+` FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.id=$1`, id))
		if err == nil {
			err = m.authorizeAlert(c, p, a, clusterID)
		}
		if err != nil {
			httpx.Fail(c, err)
			return nil, false
		}
		return a, true
	}
	q := `SELECT ` + alertCols + ` FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.fingerprint=$1`
	args := []any{key}
	if dsHint > 0 {
		q += ` AND a.datasource_id=$2`
		args = append(args, dsHint)
	}
	rows, err := m.d.DB.Query(c, q+` ORDER BY a.id LIMIT 20`, args...)
	if err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	var cands []*Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			rows.Close()
			httpx.Fail(c, err)
			return nil, false
		}
		cands = append(cands, a)
	}
	rows.Close()
	var hits []*Alert
	for _, a := range cands {
		if m.authorizeAlert(c, p, a, clusterID) == nil {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 0:
		httpx.Fail(c, httpx.NotFound("告警不存在或不属于当前集群"))
		return nil, false
	case 1:
		return hits[0], true
	}
	httpx.Fail(c, httpx.Conflict("该 fingerprint 存在于多个 Alertmanager 数据源，请使用告警 id 或指定 datasource_id"))
	return nil, false
}

func (m *Module) loadClaimState(ctx context.Context, id int64) (claimState, error) {
	var cs claimState
	err := m.d.DB.QueryRow(ctx, `SELECT claim_comment,claim_silence_id,claim_silence_until FROM obs_alerts WHERE id=$1`, id).
		Scan(&cs.Comment, &cs.SilenceID, &cs.SilenceUntil)
	return cs, err
}

// alertmanagerForAlert builds the Alertmanager client of the datasource that sent the alert.
// Missing / unusable datasource → 412 so the UI can offer "只认领".
func (m *Module) alertmanagerForAlert(c *gin.Context, a *Alert) (*client, error) {
	p := rbac.PrincipalFrom(c)
	ds, err := m.loadDS(c, nil, a.DatasourceID, "alertmanager")
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusBadRequest) {
			return nil, httpx.PreconditionFailed("该告警所在环境没有可用的 Alertmanager 数据源，无法创建静默")
		}
		return nil, err
	}
	if strings.TrimSpace(ds.URL) == "" {
		return nil, httpx.PreconditionFailed("Alertmanager 数据源未配置地址，无法创建静默")
	}
	scope, err := m.resolveSourceScope(c, p, ds, c.Query("cluster_id"))
	if err != nil {
		return nil, err
	}
	selected, _, err := m.selectSource(c, p, ds.Type, strconv.FormatInt(scope.Cluster.ID, 10))
	if err != nil {
		return nil, err
	}
	if selected.ID != ds.ID {
		return nil, httpx.PreconditionFailed("当前集群优先使用其他 Alertmanager 数据源，无法在告警来源上创建静默")
	}
	cl := newClient(ds)
	cl.scope = scope
	return cl, nil
}

// ------------------------------------------------------------------ handlers

type claimIn struct {
	Comment      string `json:"comment"`
	Duration     string `json:"duration"`
	DatasourceID int64  `json:"datasource_id"`
	Silence      *bool  `json:"silence"` // 省略 = true
}

func (m *Module) claimAlert(c *gin.Context) {
	var in claimIn
	if !bindOptionalJSON(c, &in) {
		return
	}
	if len([]rune(in.Comment)) > 1000 {
		httpx.Fail(c, httpx.Validation("认领备注不能超过 1000 字"))
		return
	}
	silence := in.Silence == nil || *in.Silence
	dur, err := ParseClaimDuration(in.Duration)
	if silence && err != nil {
		httpx.Fail(c, err)
		return
	}
	a, ok := m.loadAlertByKey(c, in.DatasourceID)
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	user := p.Username
	idStr := strconv.FormatInt(a.ID, 10)
	audit.SetResource(c, "obs_alert", idStr)
	audit.SetAction(c, "claim")

	decision := DecideClaim(a.HandleStatus, a.AckedBy, user)
	if decision == ClaimAlreadyMine {
		cs, _ := m.loadClaimState(c, a.ID)
		httpx.OK(c, gin.H{"id": a.ID, "handle_status": "acked", "claimed_by": a.AckedBy, "claimed_at": a.AckedAt,
			"acked_by": a.AckedBy, "acked_at": a.AckedAt, "silenced": cs.SilenceID != "", "silence_id": cs.SilenceID,
			"silence_until": cs.SilenceUntil, "already_claimed": true})
		return
	}
	if err := claimError(decision, a.AckedBy); err != nil {
		httpx.Fail(c, err)
		return
	}

	var silenceID string
	var until *time.Time
	var cl *client
	if silence {
		if !p.Can(PermSilence) {
			httpx.Fail(c, httpx.Forbidden("没有创建静默的权限（alert:silence），可选择只认领"))
			return
		}
		cl, err = m.alertmanagerForAlert(c, a)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if m.silencePolicy(c).NeedsApproval(cl.scope.Cluster.Env) {
			httpx.Fail(c, httpx.PreconditionFailed("该环境创建静默需要审批，认领时无法直接静默；可只认领后在静默页提交审批"))
			return
		}
		matchers := DefaultSilenceMatchers(a.Labels)
		if len(matchers) == 0 {
			httpx.Fail(c, httpx.PreconditionFailed("告警缺少可用于静默的标签，无法创建静默"))
			return
		}
		comment := "认领告警 #" + idStr
		if in.Comment != "" {
			comment += "：" + in.Comment
		}
		sin := &SilenceInput{Datasource: a.DatasourceID, Matchers: matchers, StartsAt: "now", Duration: dur.String(), Comment: comment}
		silenceID, err = cl.CreateSilence(c, sin, user)
		if err != nil {
			httpx.Fail(c, err) // 上游失败 502，告警不会被认领
			return
		}
		t := time.Now().Add(dur)
		until = &t
	}

	var claimedAt time.Time
	err = m.d.DB.QueryRow(c, `UPDATE obs_alerts SET handle_status='acked',acked_by=$2,acked_at=now(),
		claim_comment=$3,claim_silence_id=$4,claim_silence_until=$5
		WHERE id=$1 AND (handle_status='open' OR (handle_status='acked' AND acked_by='')) RETURNING acked_at`,
		a.ID, user, in.Comment, silenceID, until).Scan(&claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 并发：他人抢先认领/关闭。撤销刚建的静默，返回最新占用者。
		if silenceID != "" && cl != nil {
			_ = cl.DeleteSilence(context.WithoutCancel(c), silenceID)
		}
		var hs, by string
		_ = m.d.DB.QueryRow(c, `SELECT handle_status,acked_by FROM obs_alerts WHERE id=$1`, a.ID).Scan(&hs, &by)
		if e := claimError(DecideClaim(hs, by, user), by); e != nil && by != user {
			httpx.Fail(c, e)
		} else {
			httpx.Fail(c, httpx.Conflict("告警状态已变化，请刷新后重试"))
		}
		return
	}
	if err != nil {
		if silenceID != "" && cl != nil {
			_ = cl.DeleteSilence(context.WithoutCancel(c), silenceID)
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetDetail(c, gin.H{"alert_id": a.ID, "alertname": a.Alertname, "comment": in.Comment, "silence": silence,
		"silence_id": silenceID, "duration": durOrEmpty(silence, dur)})
	httpx.OK(c, gin.H{"id": a.ID, "handle_status": "acked", "claimed_by": user, "claimed_at": claimedAt,
		"acked_by": user, "acked_at": claimedAt, "silenced": silenceID != "", "silence_id": silenceID, "silence_until": until})
}

// bindOptionalJSON binds a JSON body that may be absent.
func bindOptionalJSON(c *gin.Context, v any) bool {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return true
	}
	if err := c.ShouldBindJSON(v); err != nil && !errors.Is(err, io.EOF) {
		httpx.Fail(c, httpx.BadRequest("请求体格式错误"))
		return false
	}
	return true
}

func durOrEmpty(on bool, d time.Duration) string {
	if !on {
		return ""
	}
	return d.String()
}

func (m *Module) alertTimeline(c *gin.Context) {
	a, ok := m.loadAlertByKey(c, 0)
	if !ok {
		return
	}
	cs, err := m.loadClaimState(c, a.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	events := alertTimelineEvents(a, cs)

	// 通知：单条与聚合发送的 event_key 都包含 alert:<ds>:<fp>:<status>
	rows, err := m.d.DB.Query(c, `SELECT id,channel_name,event_status,status,reason,triggered_by,created_at FROM obs_notify_records
		WHERE event_kind='alert' AND strpos(event_key,$1)>0 AND created_at >= $2 ORDER BY created_at LIMIT 200`,
		alertNotifyKeyPrefix(a), a.CreatedAt.Add(-time.Minute))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	for rows.Next() {
		var id int64
		var ch, evs, st, reason, by string
		var at time.Time
		if err := rows.Scan(&id, &ch, &evs, &st, &reason, &by, &at); err != nil {
			rows.Close()
			httpx.Fail(c, err)
			return
		}
		events = append(events, notifyTimelineEvent(id, ch, evs, st, reason, by, at))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}

	// 转故障
	rows, err = m.d.DB.Query(c, `SELECT i.id,i.title,i.status,i.severity,i.declared_by,l.created_at,l.rule_id IS NOT NULL,
		i.resolved_by,i.resolved_at FROM oncall_alert_links l JOIN oncall_incidents i ON i.id=l.incident_id
		WHERE l.alert_id=$1 ORDER BY l.created_at`, a.ID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	for rows.Next() {
		var id int64
		var title, st, sev, by, rby string
		var at time.Time
		var auto bool
		var rat *time.Time
		if err := rows.Scan(&id, &title, &st, &sev, &by, &at, &auto, &rby, &rat); err != nil {
			rows.Close()
			httpx.Fail(c, err)
			return
		}
		how := "手动转为故障"
		if auto {
			how = "按升级规则自动转为故障"
		}
		ref := strconv.FormatInt(id, 10)
		events = append(events, TimelineEvent{At: at, Kind: "incident", Actor: by, Ref: ref,
			Message: how + " INC-" + ref + "（" + sev + "）：" + title})
		if st == "resolved" && rat != nil {
			events = append(events, TimelineEvent{At: *rat, Kind: "incident_resolved", Actor: rby, Ref: ref,
				Message: "关联故障 INC-" + ref + " 已解决"})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"alert_id": a.ID, "items": SortTimeline(events)})
}

func (m *Module) alertToIncident(c *gin.Context) {
	var in struct {
		Severity     string `json:"severity"`
		ScheduleID   int64  `json:"schedule_id"`
		DatasourceID int64  `json:"datasource_id"`
	}
	if !bindOptionalJSON(c, &in) {
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.Can(PermOncallManage) {
		httpx.Fail(c, httpx.Forbidden("没有声明故障的权限（oncall:manage）"))
		return
	}
	a, ok := m.loadAlertByKey(c, in.DatasourceID)
	if !ok {
		return
	}
	if in.Severity == "" {
		in.Severity = IncidentSeverityFor(a.Severity)
	}
	switch in.Severity {
	case "P1", "P2", "P3", "P4":
	default:
		httpx.Fail(c, httpx.Validation("故障级别应为 P1-P4"))
		return
	}
	if in.ScheduleID < 0 {
		httpx.Fail(c, httpx.Validation("排班 id 无效"))
		return
	}
	id, created, err := m.declareFromAlert(c, *a, in.ScheduleID, in.Severity, actor(c), nil)
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "obs_alert", strconv.FormatInt(a.ID, 10))
	audit.SetAction(c, "to_incident")
	audit.SetDetail(c, gin.H{"alert_id": a.ID, "incident_id": id, "created": created, "severity": in.Severity})
	httpx.OK(c, gin.H{"alert_id": a.ID, "incident_id": id, "created": created})
}
