package observability

// 告警 → 故障：
//   - 自动：新触发的告警命中启用的升级规则时，声明故障（标题取告警摘要），按规则关联的排班自动指派一线值班，
//     时间线记录来源告警；同一告警在其关联故障解决前不会重复声明。
//   - 手动：POST /oncall/incidents/from-alert/:alert_id。
//   - 告警恢复时在关联且未解决的故障时间线追加「告警已恢复」。

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// EscalationRule 是告警升级规则。
type EscalationRule struct {
	Severities  []string
	Env         string
	MatchLabels map[string]string
}

// Matches 判断告警是否命中规则：级别在列表内（空列表表示任意）、环境相同（空表示任意）、所有匹配标签相等。
func (r EscalationRule) Matches(a Alert) bool {
	if len(r.Severities) > 0 {
		ok := false
		for _, s := range r.Severities {
			ok = ok || strings.EqualFold(s, a.Severity)
		}
		if !ok {
			return false
		}
	}
	if r.Env != "" && r.Env != a.Env {
		return false
	}
	for k, v := range r.MatchLabels {
		if a.Labels[k] != v {
			return false
		}
	}
	return true
}

func (m *Module) registerEscalation(g *gin.RouterGroup) {
	rd, mg := rbac.RequirePerm(PermOncallRead), rbac.RequirePerm(PermOncallManage)
	g.GET("/escalation-rules", rd, m.ocListRules)
	g.POST("/escalation-rules", mg, m.ocSaveRule)
	g.PUT("/escalation-rules/:id", mg, m.ocSaveRule)
	g.DELETE("/escalation-rules/:id", mg, m.ocDeleteRule)
	g.POST("/incidents/from-alert/:alert_id", mg, m.ocFromAlert)
	g.GET("/incidents/:id/alerts", rd, m.ocIncidentAlerts)
}

type ruleIn struct {
	Name             string            `json:"name" binding:"required,max=100"`
	Enabled          *bool             `json:"enabled"`
	Severities       []string          `json:"severities"`
	Env              string            `json:"env"`
	MatchLabels      map[string]string `json:"match_labels"`
	ScheduleID       *int64            `json:"schedule_id"`
	IncidentSeverity string            `json:"incident_severity"`
}

func (m *Module) ocListRules(c *gin.Context) {
	items, err := collect(m.d.DB.Query(c, `SELECT * FROM oncall_escalation_rules ORDER BY id`))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}

func (m *Module) ocSaveRule(c *gin.Context) {
	var in ruleIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.IncidentSeverity == "" {
		in.IncidentSeverity = "P2"
	}
	if in.MatchLabels == nil {
		in.MatchLabels = map[string]string{}
	}
	if len(in.MatchLabels) > 10 {
		httpx.Fail(c, httpx.Validation("at most 10 match labels"))
		return
	}
	if in.Severities == nil {
		in.Severities = []string{}
	}
	lb, _ := json.Marshal(in.MatchLabels)
	en := in.Enabled == nil || *in.Enabled
	var sid any
	if in.ScheduleID != nil && *in.ScheduleID > 0 {
		sid = *in.ScheduleID
	}
	var (
		r   map[string]any
		err error
	)
	if c.Param("id") == "" {
		r, err = one(c, m, `INSERT INTO oncall_escalation_rules (name,enabled,severities,env,match_labels,schedule_id,incident_severity,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *`, strings.TrimSpace(in.Name), en, in.Severities, in.Env, lb, sid, in.IncidentSeverity, actor(c))
	} else {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		r, err = one(c, m, `UPDATE oncall_escalation_rules SET name=$2,enabled=$3,severities=$4,env=$5,match_labels=$6,schedule_id=$7,incident_severity=$8,updated_at=now()
			WHERE id=$1 RETURNING *`, id, strings.TrimSpace(in.Name), en, in.Severities, in.Env, lb, sid, in.IncidentSeverity)
	}
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_escalation_rule", strconv.FormatInt(r["id"].(int64), 10))
	httpx.OK(c, r)
}

func (m *Module) ocDeleteRule(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM oncall_escalation_rules WHERE id=$1`, id)
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

func alertTitle(a Alert) string {
	t := a.Annotations["summary"]
	if t == "" {
		t = a.Alertname
	}
	if t == "" {
		t = "告警 " + a.Fingerprint
	}
	if len([]rune(t)) > 180 {
		t = string([]rune(t)[:180])
	}
	return "[告警] " + t
}

// declareFromAlert 为告警声明故障；告警已关联未解决故障时返回该故障 id 与 false。
func (m *Module) declareFromAlert(ctx context.Context, a Alert, scheduleID int64, severity, by string, ruleID any) (int64, bool, error) {
	var existing int64
	err := m.d.DB.QueryRow(ctx, `SELECT l.incident_id FROM oncall_alert_links l JOIN oncall_incidents i ON i.id=l.incident_id
		WHERE l.alert_id=$1 AND i.status<>'resolved' ORDER BY l.incident_id DESC LIMIT 1`, a.ID).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	assignee := ""
	if scheduleID > 0 {
		if cur, err := m.currentOncall(ctx, nil, scheduleID); err == nil {
			best := 99
			for _, w := range cur {
				l, _ := w["layer"].(int)
				u, _ := w["username"].(string)
				if u != "" && l < best {
					best, assignee = l, u
				}
			}
		}
	}
	var sid any
	if scheduleID > 0 {
		sid = scheduleID
	}
	desc := a.Annotations["description"]
	if desc == "" {
		desc = a.Annotations["summary"]
	}
	tx, err := m.d.DB.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO oncall_incidents (title,description,severity,service,env,schedule_id,assigned_to,declared_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, alertTitle(a), desc, severity,
		firstNonEmpty(a.Labels["service"], a.Labels["app"], a.Labels["deployment"], a.Labels["namespace"]), a.Env, sid, assignee, by).Scan(&id); err != nil {
		return 0, false, err
	}
	src := "由告警 #" + strconv.FormatInt(a.ID, 10) + "（" + firstNonEmpty(a.Alertname, a.Fingerprint) + "，级别 " + firstNonEmpty(a.Severity, "—") + "）"
	if ruleID != nil {
		src += "按升级规则自动声明"
	} else {
		src += "手动转为故障"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,'declared',$2,$3)`, id, by, src); err != nil {
		return 0, false, err
	}
	if assignee != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message) VALUES ($1,'assigned',$2,$3)`, id, by, "按排班自动指派给一线值班 "+assignee); err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oncall_alert_links (alert_id,incident_id,rule_id) VALUES ($1,$2,$3)`, a.ID, id, ruleID); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}

// escalate 由告警通知流程调用：对新触发的告警匹配升级规则。
func (m *Module) escalate(ctx context.Context, alerts []Alert) {
	rows, err := m.d.DB.Query(ctx, `SELECT id, severities, env, match_labels, COALESCE(schedule_id,0), incident_severity FROM oncall_escalation_rules WHERE enabled ORDER BY id`)
	if err != nil {
		return
	}
	type rr struct {
		id   int64
		rule EscalationRule
		sid  int64
		sev  string
	}
	var rules []rr
	for rows.Next() {
		var x rr
		var lb []byte
		if rows.Scan(&x.id, &x.rule.Severities, &x.rule.Env, &lb, &x.sid, &x.sev) == nil {
			_ = json.Unmarshal(lb, &x.rule.MatchLabels)
			rules = append(rules, x)
		}
	}
	rows.Close()
	for _, a := range alerts {
		if a.Status != "firing" {
			continue
		}
		for _, r := range rules {
			if !r.rule.Matches(a) {
				continue
			}
			if _, _, err := m.declareFromAlert(ctx, a, r.sid, r.sev, "告警升级", r.id); err != nil && m.d.Logger != nil {
				m.d.Logger.Warn("alert escalation failed", "alert", a.ID, "rule", r.id, "err", err)
			}
			break // 一条告警只按第一条命中的规则声明
		}
	}
}

// noteResolved 在告警恢复时给关联的未解决故障追加时间线。
func (m *Module) noteResolved(ctx context.Context, alertID int64, name string) {
	_, _ = m.d.DB.Exec(ctx, `INSERT INTO oncall_incident_events (incident_id,kind,actor,message)
		SELECT l.incident_id,'note','告警升级',$2 FROM oncall_alert_links l JOIN oncall_incidents i ON i.id=l.incident_id
		WHERE l.alert_id=$1 AND i.status<>'resolved'`, alertID, "关联告警 "+name+" 已恢复，请确认后解决故障")
}

func (m *Module) ocFromAlert(c *gin.Context) {
	aid, ok := httpx.ParamID(c, "alert_id")
	if !ok {
		return
	}
	var in struct {
		Severity   string `json:"severity"`
		ScheduleID int64  `json:"schedule_id"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Severity == "" {
		in.Severity = "P2"
	}
	if in.Severity != "P1" && in.Severity != "P2" && in.Severity != "P3" && in.Severity != "P4" {
		httpx.Fail(c, httpx.Validation("severity must be P1-P4"))
		return
	}
	a, err := scanAlert(m.d.DB.QueryRow(c, `SELECT `+alertCols+` FROM obs_alerts a JOIN obs_datasources d ON d.id=a.datasource_id WHERE a.id=$1`, aid))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !canEnv(c, a.Env) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope"))
		return
	}
	id, created, err := m.declareFromAlert(c, *a, in.ScheduleID, in.Severity, actor(c), nil)
	if err != nil {
		pgFail(c, err)
		return
	}
	audit.SetResource(c, "oncall_incident", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"incident_id": id, "created": created})
}

func (m *Module) ocIncidentAlerts(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	items, err := collect(m.d.DB.Query(c, `SELECT a.id, a.alertname, a.severity, a.status, a.fingerprint, l.created_at AS linked_at
		FROM oncall_alert_links l JOIN obs_alerts a ON a.id=l.alert_id WHERE l.incident_id=$1 ORDER BY l.created_at`, id))
	if err != nil {
		pgFail(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}
