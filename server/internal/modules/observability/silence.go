package observability

// 告警静默：
//   - Alertmanager 静默：handlers.go 中的 /observability/silences（列表 / 创建 / 提前结束），对接 Alertmanager v2 API；
//   - 平台本地静默：/observability/local-silences，只作用于平台自己发出的通知（通知中心 dispatch 之前过滤），
//     未接入 Alertmanager 的集群也可用；命中的通知记为「已抑制」，原因写明静默规则；
//   - 审批：静默策略（settings: observability.silence_policy）开启后，生产环境（或指定环境）的静默创建
//     先提交审批，审批通过后才在 Alertmanager 创建 / 本地规则才生效。

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	silencePolicyKey    = "observability.silence_policy"
	silenceApprovalType = "observability.silence.create"
	maxSilenceMatchers  = 16
	maxSilenceDuration  = 30 * 24 * time.Hour
)

// ------------------------------------------------------------------ matchers

// SilenceMatcher follows the Alertmanager v2 matcher schema.
type SilenceMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`
}

var silenceLabelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,127}$`)

type compiledMatcher struct {
	SilenceMatcher
	re *regexp.Regexp
}

func (m SilenceMatcher) equal() bool { return m.IsEqual == nil || *m.IsEqual }

// compileMatchers validates matchers (Alertmanager semantics: regexes are
// anchored) and requires at least one matcher that does not match the empty
// string, so a silence can never mute everything.
func compileMatchers(in []SilenceMatcher) ([]compiledMatcher, error) {
	if len(in) == 0 {
		return nil, httpx.Validation("至少需要一个匹配条件")
	}
	if len(in) > maxSilenceMatchers {
		return nil, httpx.Validation("匹配条件最多 16 个")
	}
	out := make([]compiledMatcher, 0, len(in))
	selective := false
	for _, m := range in {
		m.Name = strings.TrimSpace(m.Name)
		if !silenceLabelRe.MatchString(m.Name) {
			return nil, httpx.Validation("匹配条件的标签名不合法：" + m.Name)
		}
		if len(m.Value) > 512 {
			return nil, httpx.Validation("匹配条件的值过长")
		}
		cm := compiledMatcher{SilenceMatcher: m}
		if m.IsRegex {
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return nil, httpx.Validation("正则表达式不合法：" + m.Name)
			}
			cm.re = re
		}
		if !cm.matchValue("") {
			selective = true
		}
		out = append(out, cm)
	}
	if !selective {
		return nil, httpx.Validation("匹配条件过宽：至少一个条件不能匹配空值")
	}
	return out, nil
}

func (m compiledMatcher) matchValue(v string) bool {
	var ok bool
	if m.re != nil {
		ok = m.re.MatchString(v)
	} else {
		ok = v == m.Value
	}
	if !m.equal() {
		return !ok
	}
	return ok
}

func matchAll(ms []compiledMatcher, labels map[string]string) bool {
	for _, m := range ms {
		if !m.matchValue(labels[m.Name]) {
			return false
		}
	}
	return true
}

// eventMatchLabels is the label set a local silence sees for a notification:
// the event's own labels plus normalized fields (never overriding real labels).
func eventMatchLabels(ev *NotifyEvent) map[string]string {
	out := make(map[string]string, len(ev.Labels)+8)
	for k, v := range ev.Labels {
		out[k] = v
	}
	set := func(k, v string) {
		if _, ok := out[k]; !ok && v != "" {
			out[k] = v
		}
	}
	set("alertname", ev.AlertName)
	set("severity", firstNonEmpty(ev.SeverityRaw, ev.Severity))
	set("env", ev.Env)
	set("cluster", ev.Cluster)
	set("namespace", ev.Namespace)
	out["kind"] = ev.Kind
	out["status"] = ev.Status
	return out
}

// DefaultSilenceMatchers proposes matchers for silencing one alert: the alert
// name plus identifying labels that exist on it (pure, used by the UI hint API).
func DefaultSilenceMatchers(labels map[string]string) []SilenceMatcher {
	out := []SilenceMatcher{}
	for _, k := range []string{"alertname", "cluster", "namespace", "pod", "deployment", "statefulset", "daemonset", "job", "node", "service", "instance"} {
		if v := labels[k]; v != "" {
			out = append(out, SilenceMatcher{Name: k, Value: v})
		}
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// ------------------------------------------------------------------ policy

// SilencePolicy decides whether creating a silence needs approval.
type SilencePolicy struct {
	RequireApproval bool     `json:"require_approval"`
	Envs            []string `json:"envs"` // empty = production-like environments
}

// NeedsApproval: "*" (all environments) always counts as production.
func (p SilencePolicy) NeedsApproval(env string) bool {
	if !p.RequireApproval {
		return false
	}
	if env == "*" {
		return true
	}
	if len(p.Envs) > 0 {
		return inList(p.Envs, env)
	}
	return approval.IsProductionEnv(env)
}

func (m *Module) silencePolicy(ctx context.Context) SilencePolicy {
	p := SilencePolicy{Envs: []string{}}
	if m.d != nil && m.d.Settings != nil {
		_ = m.d.Settings.Get(ctx, silencePolicyKey, &p)
	}
	if p.Envs == nil {
		p.Envs = []string{}
	}
	return p
}

func unrestricted(p *rbac.Principal) bool {
	if p == nil {
		return false
	}
	if p.EnvScope == nil {
		return true
	}
	for _, e := range p.EnvScope {
		if e == "*" {
			return true
		}
	}
	return false
}

func (m *Module) getSilencePolicy(c *gin.Context) { httpx.OK(c, m.silencePolicy(c)) }

func (m *Module) putSilencePolicy(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	if !unrestricted(p) {
		httpx.Fail(c, httpx.Forbidden("修改静默审批策略需要不受环境限制的权限"))
		return
	}
	var in SilencePolicy
	if !httpx.Bind(c, &in) {
		return
	}
	in.Envs = cleanList(in.Envs)
	if len(in.Envs) > 32 {
		httpx.Fail(c, httpx.Validation("环境最多 32 个"))
		return
	}
	if m.d.Settings == nil {
		httpx.Fail(c, httpx.NewError(503, httpx.CodeUnavailable, "设置服务不可用"))
		return
	}
	raw, _ := json.Marshal(in)
	_, err := m.d.Settings.Put(c, silencePolicyKey, raw, false, "告警静默创建审批策略", p.UserID)
	audit.SetResource(c, "obs_silence_policy", "policy")
	respond(c, in, err)
}

// ------------------------------------------------------------------ local silences

// LocalSilence is a platform-side silence applied to outgoing notifications.
type LocalSilence struct {
	ID         int64            `json:"id"`
	Env        string           `json:"env"`
	Cluster    string           `json:"cluster"`
	Matchers   []SilenceMatcher `json:"matchers"`
	StartsAt   time.Time        `json:"starts_at"`
	EndsAt     time.Time        `json:"ends_at"`
	Comment    string           `json:"comment"`
	CreatedBy  string           `json:"created_by"`
	State      string           `json:"state"`  // pending | active | ended | rejected
	Status     string           `json:"status"` // pending_approval | scheduled | active | expired | ended | rejected
	ApprovalID *int64           `json:"approval_id,omitempty"`
	EndedBy    string           `json:"ended_by"`
	EndedAt    *time.Time       `json:"ended_at"`
	CreatedAt  time.Time        `json:"created_at"`
}

// localStatus derives the display status (pure).
func localStatus(state string, start, end, now time.Time) string {
	switch state {
	case "pending":
		return "pending_approval"
	case "ended", "rejected":
		return state
	}
	if !now.Before(end) {
		return "expired"
	}
	if now.Before(start) {
		return "scheduled"
	}
	return "active"
}

const localSilenceCols = `id,env,cluster,matchers,starts_at,ends_at,comment,created_by,state,approval_id,ended_by,ended_at,created_at`

func scanLocalSilence(row pgx.Row) (*LocalSilence, error) {
	var s LocalSilence
	var raw []byte
	if err := row.Scan(&s.ID, &s.Env, &s.Cluster, &raw, &s.StartsAt, &s.EndsAt, &s.Comment, &s.CreatedBy, &s.State,
		&s.ApprovalID, &s.EndedBy, &s.EndedAt, &s.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("静默规则不存在")
		}
		return nil, err
	}
	_ = json.Unmarshal(raw, &s.Matchers)
	if s.Matchers == nil {
		s.Matchers = []SilenceMatcher{}
	}
	s.Status = localStatus(s.State, s.StartsAt, s.EndsAt, time.Now())
	return &s, nil
}

type localSilenceInput struct {
	Env      string           `json:"env"`
	Cluster  string           `json:"cluster"`
	Matchers []SilenceMatcher `json:"matchers"`
	StartsAt string           `json:"startsAt"`
	EndsAt   string           `json:"endsAt"`
	Duration string           `json:"duration"`
	Comment  string           `json:"comment"`
	Reason   string           `json:"reason"`
}

// window resolves startsAt/endsAt/duration like the Alertmanager create flow.
func silenceWindow(startsAt, endsAt, duration string, now time.Time) (time.Time, time.Time, error) {
	start, err := parseTime(startsAt, now)
	if err != nil {
		return start, start, httpx.Validation("开始时间格式不正确")
	}
	var end time.Time
	if endsAt != "" {
		if end, err = parseTime(endsAt, now); err != nil {
			return start, end, httpx.Validation("结束时间格式不正确")
		}
	} else {
		dur := 2 * time.Hour
		if duration != "" {
			if dur, err = parseDuration(duration); err != nil {
				return start, end, httpx.Validation("静默时长格式不正确")
			}
		}
		end = start.Add(dur)
	}
	if !end.After(start) {
		return start, end, httpx.Validation("结束时间必须晚于开始时间")
	}
	if !end.After(now) {
		return start, end, httpx.Validation("结束时间必须晚于当前时间")
	}
	if end.Sub(start) > maxSilenceDuration {
		return start, end, httpx.Validation("静默时长不能超过 30 天")
	}
	return start, end, nil
}

func (in *localSilenceInput) normalize(p *rbac.Principal, now time.Time) (time.Time, time.Time, error) {
	in.Env = strings.TrimSpace(in.Env)
	in.Cluster = strings.TrimSpace(in.Cluster)
	in.Comment = strings.TrimSpace(in.Comment)
	if in.Env == "" {
		return time.Time{}, time.Time{}, httpx.Validation("请选择生效环境")
	}
	if len(in.Env) > 64 || len(in.Cluster) > 128 {
		return time.Time{}, time.Time{}, httpx.Validation("环境或集群名称过长")
	}
	if in.Env == "*" && !unrestricted(p) {
		return time.Time{}, time.Time{}, httpx.Forbidden("对全部环境生效的静默需要不受环境限制的权限")
	}
	if in.Env != "*" && !p.CanEnv(in.Env) {
		return time.Time{}, time.Time{}, httpx.Forbidden("无权操作该环境：" + in.Env)
	}
	if in.Comment == "" || len([]rune(in.Comment)) > 500 {
		return time.Time{}, time.Time{}, httpx.Validation("请填写 1~500 字的注释")
	}
	if _, err := compileMatchers(in.Matchers); err != nil {
		return time.Time{}, time.Time{}, err
	}
	for i := range in.Matchers {
		in.Matchers[i].Name = strings.TrimSpace(in.Matchers[i].Name)
	}
	return silenceWindow(in.StartsAt, in.EndsAt, in.Duration, now)
}

func (m *Module) listLocalSilences(c *gin.Context) {
	p := rbac.PrincipalFrom(c)
	q := `SELECT ` + localSilenceCols + ` FROM obs_local_silences WHERE ($1='' OR env=$1 OR env='*') AND ($2='' OR cluster=$2 OR cluster='')`
	if c.Query("include_history") != "true" {
		q += ` AND state IN ('pending','active') AND ends_at > now()`
	}
	q += ` ORDER BY id DESC LIMIT 500`
	rows, err := m.d.DB.Query(c, q, c.Query("env"), c.Query("cluster"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	defer rows.Close()
	out := []*LocalSilence{}
	for rows.Next() {
		s, err := scanLocalSilence(rows)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if s.Env == "*" || p.CanEnv(s.Env) {
			out = append(out, s)
		}
	}
	httpx.OK(c, gin.H{"items": out})
}

func (m *Module) createLocalSilence(c *gin.Context) {
	var in localSilenceInput
	if !httpx.Bind(c, &in) {
		return
	}
	p := rbac.PrincipalFrom(c)
	start, end, err := in.normalize(p, time.Now())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	needs := m.silencePolicy(c).NeedsApproval(in.Env)
	if needs && m.d.Approval == nil {
		httpx.Fail(c, httpx.NewError(503, httpx.CodeUnavailable, "审批服务不可用，静默未创建"))
		return
	}
	state := "active"
	if needs {
		state = "pending"
	}
	mt, _ := json.Marshal(in.Matchers)
	s, err := scanLocalSilence(m.d.DB.QueryRow(c, `INSERT INTO obs_local_silences (env,cluster,matchers,starts_at,ends_at,comment,created_by,state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+localSilenceCols, in.Env, in.Cluster, mt, start, end, in.Comment, p.Username, state))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_local_silence", strconv.FormatInt(s.ID, 10))
	audit.SetDetail(c, gin.H{"env": in.Env, "cluster": in.Cluster, "matchers": in.Matchers, "starts_at": start, "ends_at": end, "approval": needs})
	if needs {
		ticket, err := m.d.Approval.CreateOwned(c, p, approval.CreateInput{Type: silenceApprovalType, Env: in.Env,
			Title:  "平台静默：" + matcherText(in.Matchers),
			Reason: firstNonEmpty(in.Reason, in.Comment),
			Payload: silenceTicket{Kind: "local", LocalID: s.ID, Env: in.Env, Matchers: in.Matchers,
				StartsAt: start.UTC().Format(time.RFC3339), EndsAt: end.UTC().Format(time.RFC3339), Comment: in.Comment}})
		if err != nil {
			_, _ = m.d.DB.Exec(context.WithoutCancel(c), `DELETE FROM obs_local_silences WHERE id=$1 AND state='pending'`, s.ID)
			httpx.Fail(c, err)
			return
		}
		_, _ = m.d.DB.Exec(c, `UPDATE obs_local_silences SET approval_id=$2,updated_at=now() WHERE id=$1`, s.ID, ticket.ID)
		approval.RespondPending(c, ticket)
		return
	}
	localSilenceCache.invalidate()
	httpx.Created(c, s)
}

// expireLocalSilence ends a silence early (提前结束) or withdraws a pending one.
func (m *Module) expireLocalSilence(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	p := rbac.PrincipalFrom(c)
	s, err := scanLocalSilence(m.d.DB.QueryRow(c, `SELECT `+localSilenceCols+` FROM obs_local_silences WHERE id=$1`, id))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if (s.Env == "*" && !unrestricted(p)) || (s.Env != "*" && !p.CanEnv(s.Env)) {
		httpx.Fail(c, httpx.Forbidden("无权操作该环境：" + s.Env))
		return
	}
	if s.Status != "active" && s.Status != "scheduled" && s.Status != "pending_approval" {
		httpx.Fail(c, httpx.Conflict("静默规则已结束"))
		return
	}
	tag, err := m.d.DB.Exec(c, `UPDATE obs_local_silences SET state='ended',ended_by=$2,ended_at=now(),updated_at=now()
		WHERE id=$1 AND state IN ('pending','active')`, id, p.Username)
	if err == nil && tag.RowsAffected() == 0 {
		err = httpx.Conflict("静默规则状态已变化，请刷新")
	}
	localSilenceCache.invalidate()
	audit.SetResource(c, "obs_local_silence", strconv.FormatInt(id, 10))
	respond(c, gin.H{"id": id, "state": "ended"}, err)
}

func matcherText(ms []SilenceMatcher) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		op := "="
		switch {
		case m.IsRegex && !m.equal():
			op = "!~"
		case m.IsRegex:
			op = "=~"
		case !m.equal():
			op = "!="
		}
		parts = append(parts, m.Name+op+m.Value)
	}
	return firstN(strings.Join(parts, ", "), 200)
}

// ------------------------------------------------------------------ dispatch hook

type activeSilence struct {
	id       int64
	env      string
	cluster  string
	comment  string
	matchers []compiledMatcher
}

// silenceFor returns the first active silence matching ev (pure).
func silenceFor(list []activeSilence, ev *NotifyEvent) *activeSilence {
	if ev.Kind == "test" {
		return nil
	}
	labels := eventMatchLabels(ev)
	for i := range list {
		s := &list[i]
		if s.env != "*" && s.env != ev.Env {
			continue
		}
		if s.cluster != "" && s.cluster != ev.Cluster {
			continue
		}
		if matchAll(s.matchers, labels) {
			return s
		}
	}
	return nil
}

// localSilenceCache keeps active silences for a few seconds so bursts of
// notifications do not each hit the database.
var localSilenceCache = &silenceCache{}

type silenceCache struct {
	mu     sync.Mutex
	at     time.Time
	owner  *Module
	active []activeSilence
}

func (s *silenceCache) invalidate() {
	s.mu.Lock()
	s.at = time.Time{}
	s.mu.Unlock()
}

func (m *Module) activeSilences(ctx context.Context) []activeSilence {
	c := localSilenceCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner == m && time.Since(c.at) < 10*time.Second {
		return c.active
	}
	rows, err := m.d.DB.Query(ctx, `SELECT id,env,cluster,comment,matchers FROM obs_local_silences
		WHERE state='active' AND starts_at<=now() AND ends_at>now() ORDER BY id`)
	if err != nil {
		m.logWarn("load local silences", err)
		return nil // failing open here only means notifications are sent
	}
	defer rows.Close()
	out := []activeSilence{}
	for rows.Next() {
		var a activeSilence
		var raw []byte
		if rows.Scan(&a.id, &a.env, &a.cluster, &a.comment, &raw) != nil {
			continue
		}
		var ms []SilenceMatcher
		if json.Unmarshal(raw, &ms) != nil {
			continue
		}
		cm, err := compileMatchers(ms)
		if err != nil {
			continue
		}
		a.matchers = cm
		out = append(out, a)
	}
	c.owner, c.at, c.active = m, time.Now(), out
	return out
}

// splitSilenced separates events muted by a local silence (pure).
func splitSilenced(events []*NotifyEvent, list []activeSilence) (pass []*NotifyEvent, muted map[*NotifyEvent]*activeSilence) {
	muted = map[*NotifyEvent]*activeSilence{}
	for _, ev := range events {
		if s := silenceFor(list, ev); s != nil {
			muted[ev] = s
			continue
		}
		pass = append(pass, ev)
	}
	return pass, muted
}

// ------------------------------------------------------------------ approval

// silenceTicket is the approval payload for both silence kinds.
type silenceTicket struct {
	Kind       string           `json:"kind"` // local | alertmanager
	LocalID    int64            `json:"local_id,omitempty"`
	Datasource int64            `json:"datasource,omitempty"`
	ClusterID  string           `json:"cluster_id,omitempty"`
	Env        string           `json:"env"`
	Matchers   []SilenceMatcher `json:"matchers"`
	StartsAt   string           `json:"startsAt,omitempty"`
	EndsAt     string           `json:"endsAt,omitempty"`
	Duration   string           `json:"duration,omitempty"`
	Comment    string           `json:"comment"`
}

func (m *Module) registerSilenceApproval() {
	if m.d == nil || m.d.Approval == nil {
		return
	}
	if err := m.d.Approval.Register(approval.TypeSpec{
		Type: silenceApprovalType, Module: "observability", Description: "告警静默创建审批（静默策略要求审批的环境）",
		CreatePermission: PermSilence, ApproverPermission: "approval:approve", PolicyIndependent: true, Timeout: 2 * time.Minute,
		Handler: silenceApprovalHandler{m: m},
	}); err != nil {
		m.logWarn("register silence approval", err)
	}
}

type silenceApprovalHandler struct{ m *Module }

// applicant rebuilds the applicant's current access: revoked permission or
// environment scope after submission blocks execution.
func (h silenceApprovalHandler) applicant(ctx context.Context, t *approval.Ticket, env string) (*rbac.Principal, error) {
	p := &rbac.Principal{UserID: t.ApplicantID, Username: t.ApplicantName}
	if h.m.d.RBAC != nil {
		_, perms, scope, err := h.m.d.RBAC.UserAccess(ctx, t.ApplicantID)
		if err != nil {
			return nil, err
		}
		p.Permissions, p.EnvScope = perms, scope
		if !p.Can(PermSilence) {
			return nil, httpx.Forbidden("申请人已无静默权限")
		}
	}
	if (env == "*" && !unrestricted(p)) || (env != "*" && !p.CanEnv(env)) {
		return nil, httpx.Forbidden("申请人已无该环境权限")
	}
	return p, nil
}

func (h silenceApprovalHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var pl silenceTicket
	if err := t.DecodePayload(&pl); err != nil {
		return nil, httpx.BadRequest("静默审批内容无效")
	}
	p, err := h.applicant(ctx, t, pl.Env)
	if err != nil {
		return nil, err
	}
	switch pl.Kind {
	case "local":
		tag, err := h.m.d.DB.Exec(ctx, `UPDATE obs_local_silences SET state='active',updated_at=now()
			WHERE id=$1 AND state='pending' AND (approval_id=$2 OR approval_id IS NULL) AND ends_at>now()`, pl.LocalID, t.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, httpx.Conflict("静默规则已撤回或已过期")
		}
		localSilenceCache.invalidate()
		return gin.H{"local_silence_id": pl.LocalID}, nil
	case "alertmanager":
		ds, err := h.m.loadDS(ctx, nil, pl.Datasource, "alertmanager")
		if err != nil {
			return nil, err
		}
		scope, err := h.m.resolveSourceScope(ctx, p, ds, pl.ClusterID)
		if err != nil {
			return nil, err
		}
		if scope.Cluster.Env != pl.Env {
			return nil, httpx.Conflict("集群环境已变化，静默未创建")
		}
		cl := newClient(ds)
		cl.scope = scope
		in := &SilenceInput{Datasource: pl.Datasource, StartsAt: pl.StartsAt, EndsAt: pl.EndsAt, Duration: pl.Duration, Comment: pl.Comment, Matchers: pl.Matchers}
		id, err := cl.CreateSilence(ctx, in, p.Username)
		if err != nil {
			return nil, err
		}
		return gin.H{"silenceID": id}, nil
	}
	return nil, httpx.BadRequest("未知的静默类型")
}

func (h silenceApprovalHandler) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var pl silenceTicket
	if err := t.DecodePayload(&pl); err != nil || pl.Kind != "local" {
		return nil
	}
	_, err := h.m.d.DB.Exec(ctx, `UPDATE obs_local_silences SET state='rejected',updated_at=now() WHERE id=$1 AND state='pending' AND approval_id=$2`, pl.LocalID, t.ID)
	return err
}

// requestSilenceApproval submits an Alertmanager silence for approval instead
// of creating it. The request is validated up front so approvers never see an
// unusable silence.
func (m *Module) requestSilenceApproval(c *gin.Context, p *rbac.Principal, cl *client, in *SilenceInput) {
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.NewError(503, httpx.CodeUnavailable, "审批服务不可用，静默未创建"))
		return
	}
	if _, err := compileMatchers(in.Matchers); err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, _, err := silenceWindow(in.StartsAt, in.EndsAt, in.Duration, time.Now()); err != nil {
		httpx.Fail(c, err)
		return
	}
	if strings.TrimSpace(in.Comment) == "" {
		httpx.Fail(c, httpx.Validation("请填写注释"))
		return
	}
	env := cl.scope.Cluster.Env
	pl := silenceTicket{Kind: "alertmanager", Datasource: in.Datasource, ClusterID: strconv.FormatInt(cl.scope.Cluster.ID, 10), Env: env,
		Matchers: in.Matchers, StartsAt: in.StartsAt, EndsAt: in.EndsAt, Duration: in.Duration, Comment: in.Comment}
	if strings.TrimSpace(pl.StartsAt) == "now" {
		pl.StartsAt = "" // resolved when the approved silence is created
	}
	ticket, err := m.d.Approval.CreateOwned(c, p, approval.CreateInput{Type: silenceApprovalType, Env: env,
		Title:  "[" + cl.scope.Cluster.Name + "] Alertmanager 静默：" + matcherText(in.Matchers),
		Reason: in.Comment, Payload: pl})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "obs_silence", "approval:"+strconv.FormatInt(ticket.ID, 10))
	audit.SetDetail(c, pl)
	approval.RespondPending(c, ticket)
}
