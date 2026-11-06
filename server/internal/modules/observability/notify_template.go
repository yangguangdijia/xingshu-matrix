package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func jsonMarshal(v any) (json.RawMessage, error) { return json.Marshal(v) }

// Event kinds a notification can carry.
var eventKinds = []string{"alert", "incident", "pipeline", "approval", "test"}

// eventStatuses lists the statuses each kind emits (used for validation and the UI).
var eventStatuses = map[string][]string{
	"alert":    {"firing", "resolved"},
	"incident": {"declared", "assigned", "acknowledged", "resolved"},
	"pipeline": {"succeeded", "failed", "cancelled"},
	"approval": {"pending"},
	"test":     {"test"},
}

func validKind(k string) bool {
	for _, x := range eventKinds {
		if x == k {
			return true
		}
	}
	return false
}

// NotifyEvent is the normalized payload every source (alerts, incidents,
// pipeline runs, approval tickets) is converted to before routing and
// rendering. Exported field names are the template variables.
type NotifyEvent struct {
	Kind        string            `json:"kind"`
	Status      string            `json:"status"`
	Key         string            `json:"key"` // dedup identity, e.g. alert:<fingerprint>
	ID          int64             `json:"id,omitempty"`
	AlertName   string            `json:"alert_name"` // 告警名 / 故障标题 / 流水线名 / 审批标题
	Severity    string            `json:"severity"`   // normalized: critical | warning | info
	SeverityRaw string            `json:"severity_raw,omitempty"`
	Env         string            `json:"env"`
	Cluster     string            `json:"cluster"`
	Namespace   string            `json:"namespace"`
	Resource    string            `json:"resource"`
	Summary     string            `json:"summary"`
	Description string            `json:"description,omitempty"`
	StartsAtT   time.Time         `json:"starts_at"`
	Link        string            `json:"link"`
	Actor       string            `json:"actor,omitempty"`
	Labels      map[string]string `json:"labels"`

	extra string // appended after rendering (AIOps diagnosis summary)
}

// templateData is what templates see: the event plus readable helpers.
type templateData struct {
	NotifyEvent
	StartsAt     string
	KindText     string
	StatusText   string
	SeverityText string
	EnvText      string
	LabelsText   string
	Count        int
}

var kindText = map[string]string{"alert": "告警", "incident": "故障", "pipeline": "流水线", "approval": "审批", "test": "测试"}
var statusText = map[string]string{
	"firing": "触发", "resolved": "已恢复", "declared": "已声明", "assigned": "已指派", "acknowledged": "已认领",
	"succeeded": "成功", "failed": "失败", "cancelled": "已取消", "pending": "待审批", "test": "测试",
}
var severityText = map[string]string{"critical": "严重", "warning": "警告", "info": "提示"}
var envText = map[string]string{"prod": "生产", "staging": "预发", "test": "测试", "dev": "开发"}

func or(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

// normSeverity maps alert levels and incident priorities onto critical|warning|info.
func normSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "fatal", "emergency", "disaster", "p0", "p1", "p2", "high", "error":
		return "critical"
	case "warning", "warn", "major", "p3", "medium":
		return "warning"
	case "":
		return ""
	default:
		return "info"
	}
}

func (e *NotifyEvent) data() templateData {
	d := templateData{NotifyEvent: *e, KindText: or(kindText, e.Kind), StatusText: or(statusText, e.Status),
		SeverityText: or(severityText, e.Severity), EnvText: or(envText, e.Env), Count: 1}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if !e.StartsAtT.IsZero() {
		d.StartsAt = e.StartsAtT.In(notifyLoc()).Format("2006-01-02 15:04:05")
	}
	keys := make([]string, 0, len(d.Labels))
	for k := range d.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+d.Labels[k])
	}
	d.LabelsText = strings.Join(parts, ", ")
	return d
}

func notifyLoc() *time.Location {
	if l, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return l
	}
	return time.Local
}

var tplFuncs = template.FuncMap{
	"default": func(def string, v any) string {
		s := fmt.Sprint(v)
		if v == nil || strings.TrimSpace(s) == "" {
			return def
		}
		return s
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"trunc": func(n int, s string) string {
		if utf8.RuneCountInString(s) <= n {
			return s
		}
		return string([]rune(s)[:n]) + "…"
	},
	"join": strings.Join,
	"date": func(layout string, t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(notifyLoc()).Format(layout)
	},
	"label": func(k string, m map[string]string) string { return m[k] },
}

const maxTemplateOutput = 32 << 10

type limitedBuf struct{ bytes.Buffer }

func (b *limitedBuf) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxTemplateOutput {
		return 0, errors.New("模板输出超过 32KB")
	}
	return b.Buffer.Write(p)
}

func parseTpl(name, src string) (*template.Template, error) {
	if len(src) > 16<<10 {
		return nil, errors.New("模板过长（最多 16KB）")
	}
	return template.New(name).Funcs(tplFuncs).Option("missingkey=zero").Parse(src)
}

// renderTemplate renders a title/body pair for an event.
func renderTemplate(titleTpl, bodyTpl string, ev *NotifyEvent) (string, string, error) {
	data := ev.data()
	out := [2]string{}
	for i, src := range []string{titleTpl, bodyTpl} {
		t, err := parseTpl([]string{"title", "body"}[i], src)
		if err != nil {
			return "", "", fmt.Errorf("%s模板语法错误：%w", []string{"标题", "正文"}[i], err)
		}
		var b limitedBuf
		if err := t.Execute(&b, data); err != nil {
			return "", "", fmt.Errorf("%s模板渲染失败：%w", []string{"标题", "正文"}[i], err)
		}
		out[i] = b.String()
	}
	title := strings.TrimSpace(strings.ReplaceAll(out[0], "\n", " "))
	body := strings.TrimSpace(collapseBlankLines(out[1]))
	return title, body, nil
}

func collapseBlankLines(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// builtinTemplate is a default template, used when a channel has none selected.
type builtinTemplate struct {
	EventKind string `json:"event_kind"`
	Name      string `json:"name"`
	TitleTpl  string `json:"title_tpl"`
	BodyTpl   string `json:"body_tpl"`
}

var builtinTemplates = map[string]builtinTemplate{
	"alert": {EventKind: "alert", Name: "默认告警模板",
		TitleTpl: `【{{if eq .Status "resolved"}}告警恢复{{else}}告警触发{{end}}】[{{.SeverityText}}] {{.AlertName}}`,
		BodyTpl: `告警：{{.AlertName}}
级别：{{.SeverityText}}
状态：{{.StatusText}}
{{if .Env}}环境：{{.EnvText}}
{{end}}{{if .Cluster}}集群：{{.Cluster}}
{{end}}{{if .Namespace}}命名空间：{{.Namespace}}
{{end}}{{if .Resource}}资源：{{.Resource}}
{{end}}{{if .StartsAt}}开始时间：{{.StartsAt}}
{{end}}{{if .Summary}}摘要：{{.Summary}}
{{end}}{{if .Description}}详情：{{.Description}}
{{end}}{{if .LabelsText}}标签：{{.LabelsText}}
{{end}}{{if .Link}}查看：{{.Link}}{{end}}`},
	"incident": {EventKind: "incident", Name: "默认故障模板",
		TitleTpl: `【故障{{.StatusText}}】[{{.SeverityRaw}}] {{.AlertName}}`,
		BodyTpl: `故障：{{.AlertName}}
优先级：{{.SeverityRaw}}
状态：{{.StatusText}}
{{if .Env}}环境：{{.EnvText}}
{{end}}{{if .Resource}}服务：{{.Resource}}
{{end}}{{if .Actor}}操作人：{{.Actor}}
{{end}}{{if label "assigned_to" .Labels}}处理人：{{label "assigned_to" .Labels}}
{{end}}{{if .Summary}}说明：{{.Summary}}
{{end}}{{if .StartsAt}}声明时间：{{.StartsAt}}
{{end}}{{if .Link}}查看：{{.Link}}{{end}}`},
	"pipeline": {EventKind: "pipeline", Name: "默认流水线模板",
		TitleTpl: `【流水线{{.StatusText}}】{{.AlertName}}`,
		BodyTpl: `流水线：{{.AlertName}}
结果：{{.StatusText}}
{{if .Cluster}}执行集群：{{.Cluster}}
{{end}}{{if label "branch" .Labels}}分支：{{label "branch" .Labels}}
{{end}}{{if label "commit" .Labels}}提交：{{label "commit" .Labels}}
{{end}}{{if .Actor}}触发人：{{.Actor}}
{{end}}{{if .StartsAt}}开始时间：{{.StartsAt}}
{{end}}{{if .Summary}}说明：{{.Summary}}
{{end}}{{if .Link}}查看：{{.Link}}{{end}}`},
	"approval": {EventKind: "approval", Name: "默认审批待办模板",
		TitleTpl: `【待审批】{{.AlertName}}`,
		BodyTpl: `审批单：{{.AlertName}}
{{if .Env}}环境：{{.EnvText}}
{{end}}{{if .Actor}}申请人：{{.Actor}}
{{end}}{{if .Summary}}理由：{{.Summary}}
{{end}}{{if .StartsAt}}提交时间：{{.StartsAt}}
{{end}}{{if .Link}}去审批：{{.Link}}{{end}}`},
	"test": {EventKind: "test", Name: "默认测试模板",
		TitleTpl: `【星枢 Matrix】通知渠道测试`,
		BodyTpl: `这是一条来自星枢 Matrix 的测试消息。
渠道：{{label "channel" .Labels}}（{{label "channel_type" .Labels}}）
{{if .Actor}}操作人：{{.Actor}}
{{end}}时间：{{.StartsAt}}`},
}

// sampleEvent returns realistic data for previews and template validation.
func sampleEvent(kind string) NotifyEvent {
	now := time.Now()
	ev := NotifyEvent{Kind: kind, StartsAtT: now.Add(-5 * time.Minute), Link: "https://matrix.example.com/#/observe/alerts/1"}
	switch kind {
	case "alert":
		ev.Status, ev.AlertName, ev.Severity, ev.SeverityRaw = "firing", "KubePodCrashLooping", "critical", "critical"
		ev.Env, ev.Cluster, ev.Namespace, ev.Resource = "prod", "prod-bj-1", "payment", "pod/payment-api-7d9f8-x2l4k"
		ev.Summary = "Pod payment-api-7d9f8-x2l4k 在 10 分钟内重启 5 次"
		ev.Labels = map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payment", "pod": "payment-api-7d9f8-x2l4k", "severity": "critical"}
		ev.Key = "alert:sample"
	case "incident":
		ev.Status, ev.AlertName, ev.Severity, ev.SeverityRaw = "declared", "支付服务大面积超时", "critical", "P1"
		ev.Env, ev.Resource, ev.Actor, ev.Summary = "prod", "payment", "zhangsan", "支付接口 P99 超过 5 秒"
		ev.Labels = map[string]string{"service": "payment", "assigned_to": "lisi"}
		ev.Link, ev.Key = "https://matrix.example.com/#/oncall/inc/1", "incident:1:declared"
	case "pipeline":
		ev.Status, ev.AlertName, ev.Severity = "failed", "payment-api / build-and-deploy", "warning"
		ev.Cluster, ev.Actor, ev.Summary = "prod-bj-1", "wangwu", "单元测试阶段失败"
		ev.Labels = map[string]string{"app": "payment-api", "pipeline": "build-and-deploy", "branch": "main", "commit": "a1b2c3d"}
		ev.Link, ev.Key = "https://matrix.example.com/#/cicd/pipeline", "pipeline:1"
	case "approval":
		ev.Status, ev.AlertName, ev.Severity = "pending", "生产环境扩容 payment-api 至 6 副本", "info"
		ev.Env, ev.Actor, ev.Summary = "prod", "zhaoliu", "大促流量预估翻倍"
		ev.Labels = map[string]string{"type": "cluster.scale"}
		ev.Link, ev.Key = "https://matrix.example.com/#/approvals", "approval:1"
	default:
		ev.Kind, ev.Status, ev.Severity = "test", "test", "info"
		ev.Actor, ev.StartsAtT = "admin", now
		ev.Labels = map[string]string{"channel": "示例渠道", "channel_type": "feishu"}
		ev.Key = "test"
	}
	return ev
}

// ------------------------------------------------------------- templates CRUD

// NotifyTemplate is a stored message template.
type NotifyTemplate struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	EventKind   string    `json:"event_kind"`
	ChannelType string    `json:"channel_type"`
	TitleTpl    string    `json:"title_tpl"`
	BodyTpl     string    `json:"body_tpl"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type templateInput struct {
	Name        string `json:"name" binding:"required,max=64"`
	Description string `json:"description" binding:"max=256"`
	EventKind   string `json:"event_kind" binding:"required"`
	ChannelType string `json:"channel_type"`
	TitleTpl    string `json:"title_tpl" binding:"required"`
	BodyTpl     string `json:"body_tpl" binding:"required"`
}

var channelTypes = []string{"dingtalk", "feishu", "wecom", "email", "slack", "webhook"}

func validChannelType(t string) bool {
	for _, x := range channelTypes {
		if x == t {
			return true
		}
	}
	return false
}

func (in *templateInput) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return httpx.Validation("请填写模板名称")
	}
	if !validKind(in.EventKind) {
		return httpx.Validation("事件类型必须是 alert|incident|pipeline|approval|test")
	}
	if in.ChannelType != "" && !validChannelType(in.ChannelType) {
		return httpx.Validation("渠道类型不正确")
	}
	ev := sampleEvent(in.EventKind)
	if _, _, err := renderTemplate(in.TitleTpl, in.BodyTpl, &ev); err != nil {
		return httpx.Validation(err.Error())
	}
	return nil
}

const tplCols = `id,name,description,event_kind,channel_type,title_tpl,body_tpl,created_by,created_at,updated_at`

func scanTemplate(row pgx.Row) (*NotifyTemplate, error) {
	var t NotifyTemplate
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.EventKind, &t.ChannelType, &t.TitleTpl, &t.BodyTpl, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NotFound("模板不存在")
	}
	return &t, err
}

func (m *Module) loadTemplate(ctx context.Context, id int64) (*NotifyTemplate, error) {
	return scanTemplate(m.d.DB.QueryRow(ctx, `SELECT `+tplCols+` FROM obs_notify_templates WHERE id=$1`, id))
}

func (m *Module) listTemplates(c *gin.Context) {
	rows, err := m.d.DB.Query(c, `SELECT `+tplCols+` FROM obs_notify_templates WHERE ($1='' OR event_kind=$1) ORDER BY event_kind, id`, c.Query("event_kind"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []NotifyTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out = append(out, *t)
	}
	httpx.OK(c, gin.H{"items": out, "defaults": builtinList(), "statuses": eventStatuses})
}

func builtinList() []builtinTemplate {
	out := make([]builtinTemplate, 0, len(eventKinds))
	for _, k := range eventKinds {
		out = append(out, builtinTemplates[k])
	}
	return out
}

func (m *Module) getTemplate(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	t, err := m.loadTemplate(c, id)
	respond(c, t, err)
}

func (m *Module) createTemplate(c *gin.Context) {
	var in templateInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	var id int64
	err := m.d.DB.QueryRow(c, `INSERT INTO obs_notify_templates (name,description,event_kind,channel_type,title_tpl,body_tpl,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, in.Name, in.Description, in.EventKind, in.ChannelType, in.TitleTpl, in.BodyTpl,
		rbac.PrincipalFrom(c).Username).Scan(&id)
	if err != nil {
		httpx.Fail(c, dupErr(err, "模板名称已存在"))
		return
	}
	audit.SetResource(c, "obs_notify_template", strconv.FormatInt(id, 10))
	t, err := m.loadTemplate(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, t)
}

func dupErr(err error, msg string) error {
	if err != nil && (strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "SQLSTATE 23505")) {
		return httpx.Conflict(msg)
	}
	return err
}

func (m *Module) updateTemplate(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cur, err := m.loadTemplate(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var in templateInput
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.EventKind != cur.EventKind {
		if n, _ := m.templateRefs(c, id); len(n) > 0 {
			httpx.Fail(c, httpx.Conflict("模板正被渠道使用，不能修改事件类型："+strings.Join(n, "、")))
			return
		}
	}
	_, err = m.d.DB.Exec(c, `UPDATE obs_notify_templates SET name=$2,description=$3,event_kind=$4,channel_type=$5,title_tpl=$6,body_tpl=$7,updated_at=now() WHERE id=$1`,
		id, in.Name, in.Description, in.EventKind, in.ChannelType, in.TitleTpl, in.BodyTpl)
	if err != nil {
		httpx.Fail(c, dupErr(err, "模板名称已存在"))
		return
	}
	audit.SetResource(c, "obs_notify_template", strconv.FormatInt(id, 10))
	t, err := m.loadTemplate(c, id)
	respond(c, t, err)
}

// templateRefs lists channel names whose templates map points at template id.
func (m *Module) templateRefs(ctx context.Context, id int64) ([]string, error) {
	rows, err := m.d.DB.Query(ctx, `SELECT name FROM obs_channels c WHERE EXISTS (
		SELECT 1 FROM jsonb_each_text(c.templates) t WHERE t.value = $1::text) ORDER BY name`, strconv.FormatInt(id, 10))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (m *Module) deleteTemplate(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if _, err := m.loadTemplate(c, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	refs, err := m.templateRefs(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if len(refs) > 0 && c.Query("force") != "true" {
		httpx.Fail(c, httpx.Conflict("模板正被渠道使用："+strings.Join(refs, "、")).WithDetails(gin.H{"channels": refs}))
		return
	}
	tx, err := m.d.DB.Begin(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer tx.Rollback(c) //nolint:errcheck
	// channels fall back to the built-in template for that kind
	if _, err := tx.Exec(c, `UPDATE obs_channels SET templates = COALESCE((SELECT jsonb_object_agg(e.k, e.v)
		FROM jsonb_each(templates) AS e(k, v) WHERE e.v #>> '{}' <> $1::text), '{}'::jsonb), updated_at=now()
		WHERE EXISTS (SELECT 1 FROM jsonb_each_text(templates) t WHERE t.value = $1::text)`, strconv.FormatInt(id, 10)); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := tx.Exec(c, `DELETE FROM obs_notify_templates WHERE id=$1`, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := tx.Commit(c); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_notify_template", strconv.FormatInt(id, 10))
	httpx.OK(c, gin.H{"deleted": id, "channels_reset": refs})
}

// previewTemplate renders a (possibly unsaved) template against sample data or
// caller-provided fields and returns the per-channel payload that would be sent.
func (m *Module) previewTemplate(c *gin.Context) {
	var in struct {
		EventKind   string       `json:"event_kind"`
		Status      string       `json:"status"`
		ChannelType string       `json:"channel_type"`
		TemplateID  int64        `json:"template_id"`
		TitleTpl    string       `json:"title_tpl"`
		BodyTpl     string       `json:"body_tpl"`
		Event       *NotifyEvent `json:"event"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if in.TemplateID > 0 {
		t, err := m.loadTemplate(c, in.TemplateID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		in.EventKind, in.TitleTpl, in.BodyTpl = t.EventKind, t.TitleTpl, t.BodyTpl
		if in.ChannelType == "" {
			in.ChannelType = t.ChannelType
		}
	}
	if in.EventKind == "" {
		in.EventKind = "alert"
	}
	if !validKind(in.EventKind) {
		httpx.Fail(c, httpx.Validation("事件类型不正确"))
		return
	}
	if in.TitleTpl == "" && in.BodyTpl == "" {
		b := builtinTemplates[in.EventKind]
		in.TitleTpl, in.BodyTpl = b.TitleTpl, b.BodyTpl
	}
	ev := sampleEvent(in.EventKind)
	if in.Event != nil {
		ev = *in.Event
		ev.Kind = in.EventKind
	}
	if in.Status != "" {
		ev.Status = in.Status
	}
	title, body, err := renderTemplate(in.TitleTpl, in.BodyTpl, &ev)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	res := gin.H{"title": title, "body": body, "variables": templateVariables}
	if in.ChannelType != "" {
		if !validChannelType(in.ChannelType) {
			httpx.Fail(c, httpx.Validation("渠道类型不正确"))
			return
		}
		ch := &Channel{Name: "预览", Type: in.ChannelType}
		p := buildPayload(ch, Message{Title: title, Body: body, Event: &ev}, time.Now())
		res["payload"] = p
	}
	httpx.OK(c, res)
}

// templateVariables documents what a template can reference (shown in the editor).
var templateVariables = []map[string]string{
	{"name": ".AlertName", "desc": "告警名 / 故障标题 / 流水线名 / 审批标题"},
	{"name": ".Severity", "desc": "级别：critical / warning / info"},
	{"name": ".SeverityText", "desc": "级别中文：严重 / 警告 / 提示"},
	{"name": ".SeverityRaw", "desc": "原始级别（故障为 P1~P4）"},
	{"name": ".Status", "desc": "状态：firing、resolved、declared、failed、pending 等"},
	{"name": ".StatusText", "desc": "状态中文"},
	{"name": ".Kind", "desc": "事件类型：alert / incident / pipeline / approval / test"},
	{"name": ".Env", "desc": "环境（.EnvText 为中文）"},
	{"name": ".Cluster", "desc": "集群"},
	{"name": ".Namespace", "desc": "命名空间"},
	{"name": ".Resource", "desc": "资源，例如 pod/xxx、服务名"},
	{"name": ".Summary", "desc": "摘要"},
	{"name": ".Description", "desc": "详情"},
	{"name": ".StartsAt", "desc": "开始时间（北京时间 2006-01-02 15:04:05）"},
	{"name": ".Link", "desc": "平台内查看链接（需在 AIOps 配置中填写平台访问地址）"},
	{"name": ".Actor", "desc": "操作人 / 触发人 / 申请人"},
	{"name": ".Labels", "desc": "标签，用 {{label \"pod\" .Labels}} 读取单个标签"},
	{"name": ".LabelsText", "desc": "全部标签 k=v 文本"},
	{"name": "函数", "desc": "default、upper、lower、trunc、join、date、label"},
}
