package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// registerRunbooks mounts runbook (预案), policy and host-onboarding routes.
// module.go only calls registerRunbooks, so policy.go's routes are mounted here.
func (m *Module) registerRunbooks(g *gin.RouterGroup) {
	read := rbac.RequirePerm(PermRead)
	manage := rbac.RequirePerm(PermManage)

	g.GET("/runbooks", read, m.handleListRunbooks)
	g.POST("/runbooks", manage, m.handleCreateRunbook)
	g.GET("/runbooks/:id", read, m.handleGetRunbook)
	g.PUT("/runbooks/:id", manage, m.handleUpdateRunbook)
	g.DELETE("/runbooks/:id", manage, m.handleDeleteRunbook)
	g.POST("/cases/:id/save-runbook", manage, m.handleSaveRunbook)

	g.GET("/policy", read, m.handleGetPolicy)
	g.PUT("/policy", manage, m.handlePutPolicy)
	g.POST("/hosts/onboard", rbac.RequirePerm(PermExecute), m.handleOnboardHost)
}

// ---------- HTTP ----------

type rbInput struct {
	Name        string          `json:"name"`
	Match       json.RawMessage `json:"match"`
	Plan        PlanDraft       `json:"plan"`
	AutoExecute bool            `json:"auto_execute"`
	AutoScope   json.RawMessage `json:"auto_scope"`
}

type rbSaveInput struct {
	Name        string          `json:"name"`
	Match       json.RawMessage `json:"match"`
	AutoExecute bool            `json:"auto_execute"`
	AutoScope   json.RawMessage `json:"auto_scope"`
}

func rbFail(c *gin.Context, err error) {
	if errors.Is(err, errStoreNotFound) {
		httpx.Fail(c, httpx.NotFound("预案不存在"))
		return
	}
	httpx.Fail(c, err)
}

func rbActor(c *gin.Context) string {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		return ""
	}
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Username
}

func (m *Module) handleListRunbooks(c *gin.Context) {
	list, err := m.listRunbooks(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if list == nil {
		list = []Runbook{}
	}
	httpx.OK(c, gin.H{"items": list, "total": len(list)})
}

func (m *Module) handleGetRunbook(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	rb, err := m.getRunbook(c.Request.Context(), id)
	if err != nil {
		rbFail(c, err)
		return
	}
	httpx.OK(c, rb)
}

func (m *Module) handleCreateRunbook(c *gin.Context) {
	var in rbInput
	if !httpx.Bind(c, &in) {
		return
	}
	rb := Runbook{Name: in.Name, Match: in.Match, Plan: in.Plan, AutoExecute: in.AutoExecute,
		AutoScope: in.AutoScope, CreatedBy: rbActor(c)}
	if err := rbNormalize(&rb); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.insertRunbook(c.Request.Context(), &rb); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "aiops_runbook", fmt.Sprint(rb.ID))
	audit.SetDetail(c, gin.H{"name": rb.Name, "risk": rb.Risk, "auto_execute": rb.AutoExecute})
	httpx.Created(c, rb)
}

func (m *Module) handleUpdateRunbook(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in rbInput
	if !httpx.Bind(c, &in) {
		return
	}
	ctx := c.Request.Context()
	cur, err := m.getRunbook(ctx, id)
	if err != nil {
		rbFail(c, err)
		return
	}
	cur.Name, cur.Match, cur.Plan, cur.AutoExecute, cur.AutoScope = in.Name, in.Match, in.Plan, in.AutoExecute, in.AutoScope
	if err := rbNormalize(cur); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.updateRunbook(ctx, cur); err != nil {
		rbFail(c, err)
		return
	}
	audit.SetResource(c, "aiops_runbook", fmt.Sprint(id))
	audit.SetDetail(c, gin.H{"name": cur.Name, "risk": cur.Risk, "auto_execute": cur.AutoExecute})
	httpx.OK(c, cur)
}

func (m *Module) handleDeleteRunbook(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	if err := m.deleteRunbook(c.Request.Context(), id); err != nil {
		rbFail(c, err)
		return
	}
	audit.SetResource(c, "aiops_runbook", fmt.Sprint(id))
	httpx.OK(c, gin.H{"id": id, "deleted": true})
}

// handleSaveRunbook: POST /cases/:id/save-runbook — build a runbook from the
// steps of the case's accepted plan that actually ran (succeeded / done by hand).
func (m *Module) handleSaveRunbook(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in rbSaveInput
	if !httpx.Bind(c, &in) {
		return
	}
	ctx := c.Request.Context()
	cs, err := m.getCase(ctx, id)
	if err != nil {
		if errors.Is(err, errStoreNotFound) {
			httpx.Fail(c, httpx.NotFound("排查单不存在"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	if cs.PlanID == nil {
		httpx.Fail(c, httpx.Validation("该排查单尚未采纳并执行方案，无法存为预案"))
		return
	}
	plan, err := m.getPlan(ctx, *cs.PlanID)
	if err != nil {
		if errors.Is(err, errStoreNotFound) {
			httpx.Fail(c, httpx.NotFound("方案不存在"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	draft, ran := rbDraftFromPlan(plan)
	if ran == 0 {
		httpx.Fail(c, httpx.Validation("该方案没有已成功执行的步骤，无法存为预案"))
		return
	}
	match := in.Match
	if rbEmptyJSON(match) {
		match = rbDeriveMatch(cs)
	}
	src := cs.ID
	rb := Runbook{Name: in.Name, Match: match, Plan: draft, AutoExecute: in.AutoExecute,
		AutoScope: in.AutoScope, SourceCase: &src, CreatedBy: rbActor(c)}
	if err := rbNormalize(&rb); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.insertRunbook(ctx, &rb); err != nil {
		httpx.Fail(c, err)
		return
	}
	rid := rb.ID
	cs.RunbookID = &rid
	if err := m.updateCase(ctx, cs); err != nil && m.d.Logger != nil {
		m.d.Logger.Warn("aiops: link runbook to case failed", "case_id", cs.ID, "runbook_id", rid, "err", err)
	}
	_ = m.appendTimeline(ctx, cs.ID, "runbook_saved", rbActor(c), gin.H{"runbook_id": rid, "name": rb.Name})
	audit.SetResource(c, "aiops_runbook", fmt.Sprint(rid))
	audit.SetDetail(c, gin.H{"name": rb.Name, "source_case": cs.ID, "risk": rb.Risk, "auto_execute": rb.AutoExecute})
	httpx.Created(c, rb)
}

// ---------- validation / helpers ----------

// rbAllowedMatchKeys are the keys matchRunbooks understands.
var rbAllowedMatchKeys = map[string]bool{
	"alertname": true, "reason": true, "analyzer": true, "namespace": true, "kind": true, "env": true,
}

// rbNormalize validates a runbook and computes its risk from the registered
// actions. Errors are Chinese validation errors.
func rbNormalize(rb *Runbook) error {
	rb.Name = strings.TrimSpace(rb.Name)
	if rb.Name == "" {
		return httpx.Validation("预案名称不能为空")
	}
	if len([]rune(rb.Name)) > 100 {
		return httpx.Validation("预案名称不能超过 100 个字符")
	}
	m, err := rbParseMatch(rb.Match)
	if err != nil {
		return err
	}
	if len(m) == 0 {
		return httpx.Validation("匹配条件不能为空（至少填写 alertname/reason/namespace/kind 之一）")
	}
	for k := range m {
		if !rbAllowedMatchKeys[k] {
			return httpx.Validation("不支持的匹配条件：" + k)
		}
	}
	if len(rb.Plan.Steps) == 0 {
		return httpx.Validation("预案至少需要一个执行步骤")
	}
	risk, err := rbPlanRisk(rb.Plan)
	if err != nil {
		return err
	}
	rb.Risk = risk
	if rb.AutoExecute && risk == RiskHigh {
		return httpx.Validation("高风险预案不能设为自动处置")
	}
	if rbEmptyJSON(rb.AutoScope) {
		rb.AutoScope = json.RawMessage(`{}`)
	} else {
		var sc rbScope
		if err := json.Unmarshal(rb.AutoScope, &sc); err != nil {
			return httpx.Validation("自动处置范围格式错误")
		}
		if sc.Hours != "" {
			if _, _, ok := rbParseHours(sc.Hours); !ok {
				return httpx.Validation("自动处置时段格式错误，应为 09-21")
			}
		}
	}
	if rb.AutoExecute {
		var sc rbScope
		_ = json.Unmarshal(rb.AutoScope, &sc)
		if len(sc.Envs) == 0 {
			return httpx.Validation("开启自动处置时必须指定适用环境（auto_scope.envs）")
		}
	}
	return nil
}

// rbPlanRisk returns the max minimum-risk of the plan's actions; unknown actions are rejected.
func rbPlanRisk(p PlanDraft) (string, error) {
	risk := RiskRead
	all := append(append([]StepDraft{}, p.Steps...), p.Verify...)
	for _, s := range all {
		a, ok := ActionByName(s.Action)
		if !ok {
			return "", httpx.Validation("未注册的操作：" + s.Action)
		}
		risk = MaxRisk(risk, a.Spec().Risk)
	}
	return risk, nil
}

func rbEmptyJSON(b json.RawMessage) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "null" || s == "{}"
}

// rbParseMatch decodes a match object. Values may be a string or a list of strings.
func rbParseMatch(raw json.RawMessage) (map[string][]string, error) {
	out := map[string][]string{}
	if rbEmptyJSON(raw) {
		return out, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, httpx.Validation("匹配条件必须是 JSON 对象")
	}
	for k, v := range obj {
		k = strings.ToLower(strings.TrimSpace(k))
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out[k] = []string{s}
			}
			continue
		}
		var ss []string
		if err := json.Unmarshal(v, &ss); err != nil {
			return nil, httpx.Validation("匹配条件 " + k + " 只能是字符串或字符串数组")
		}
		var vals []string
		for _, x := range ss {
			if x = strings.TrimSpace(x); x != "" {
				vals = append(vals, x)
			}
		}
		if len(vals) > 0 {
			out[k] = vals
		}
	}
	return out, nil
}

// rbFacts are the case attributes runbooks match on (lower-cased).
type rbFacts struct {
	alertname string
	namespace string
	kind      string
	env       string
	reasons   []string // finding analyzers
}

func rbCaseFacts(c *Case, fs []Finding) rbFacts {
	var f rbFacts
	if c == nil {
		return f
	}
	f.env = strings.ToLower(c.Env)
	var t map[string]any
	_ = json.Unmarshal(c.Target, &t)
	str := func(k string) string {
		if v, ok := t[k].(string); ok {
			return strings.ToLower(strings.TrimSpace(v))
		}
		return ""
	}
	f.alertname = str("alertname")
	f.namespace = str("namespace")
	f.kind = str("kind")
	if f.kind == "" {
		f.kind = strings.ToLower(c.Kind)
	}
	if r := str("reason"); r != "" {
		f.reasons = append(f.reasons, r)
	}
	for _, x := range fs {
		if x.Analyzer != "" {
			f.reasons = append(f.reasons, strings.ToLower(x.Analyzer))
		}
	}
	return f
}

func rbAnyEq(want []string, got ...string) bool {
	for _, w := range want {
		w = strings.ToLower(w)
		for _, g := range got {
			if g != "" && (w == g || w == "*") {
				return true
			}
		}
	}
	return false
}

// rbMatches reports whether every condition in match holds for the case.
// An empty match never matches (no accidental catch-all runbooks).
func rbMatches(match json.RawMessage, f rbFacts) bool {
	m, err := rbParseMatch(match)
	if err != nil || len(m) == 0 {
		return false
	}
	for k, want := range m {
		var ok bool
		switch k {
		case "alertname":
			ok = rbAnyEq(want, f.alertname)
		case "reason", "analyzer":
			ok = rbAnyEq(want, f.reasons...)
		case "namespace":
			ok = rbAnyEq(want, f.namespace)
		case "kind":
			ok = rbAnyEq(want, f.kind)
		case "env":
			ok = rbAnyEq(want, f.env)
		default:
			ok = false
		}
		if !ok {
			return false
		}
	}
	return true
}

// rbFilterMatches returns the runbooks matching the case, most specific first
// (more conditions), then by success count.
func rbFilterMatches(list []Runbook, c *Case, fs []Finding) []Runbook {
	f := rbCaseFacts(c, fs)
	var out []Runbook
	for _, rb := range list {
		if rbMatches(rb.Match, f) {
			out = append(out, rb)
		}
	}
	spec := func(rb Runbook) int { m, _ := rbParseMatch(rb.Match); return len(m) }
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if spec(b) > spec(a) || (spec(b) == spec(a) && b.Successes > a.Successes) {
				out[j-1], out[j] = b, a
			} else {
				break
			}
		}
	}
	return out
}

// matchRunbooks returns stored runbooks whose match fits the case and findings.
func (m *Module) matchRunbooks(ctx context.Context, c *Case, fs []Finding) []Runbook {
	if c == nil || m.d == nil || m.d.DB == nil {
		return nil
	}
	list, err := m.listRunbooks(ctx)
	if err != nil {
		if m.d.Logger != nil {
			m.d.Logger.Warn("aiops: list runbooks for match failed", "err", err)
		}
		return nil
	}
	return rbFilterMatches(list, c, fs)
}

// rbDeriveMatch builds a default match from the case target and conclusion.
func rbDeriveMatch(c *Case) json.RawMessage {
	f := rbCaseFacts(c, nil)
	var concl Conclusion
	_ = json.Unmarshal(c.Conclusion, &concl)
	m := map[string]any{}
	if f.alertname != "" {
		m["alertname"] = f.alertname
	}
	if f.namespace != "" {
		m["namespace"] = f.namespace
	}
	if f.kind != "" {
		m["kind"] = f.kind
	}
	var reasons []string
	seen := map[string]bool{}
	for _, x := range concl.Findings {
		if x.Analyzer != "" && !seen[x.Analyzer] {
			seen[x.Analyzer] = true
			reasons = append(reasons, x.Analyzer)
		}
	}
	if len(reasons) > 0 {
		m["reason"] = reasons
	}
	b, _ := json.Marshal(m)
	return b
}

// rbDraftFromPlan keeps the main/verify steps that ran (succeeded or done by hand).
func rbDraftFromPlan(p *Plan) (PlanDraft, int) {
	d := PlanDraft{Title: p.Title, Description: p.Description, Steps: []StepDraft{}, Verify: []StepDraft{}}
	ran := 0
	for _, s := range p.Steps {
		if s.Status != StepSucceeded && s.Status != StepManual {
			continue
		}
		sd := StepDraft{Action: s.Action, Params: s.Params, Note: s.Title}
		switch s.Phase {
		case "verify":
			d.Verify = append(d.Verify, sd)
		case "rollback":
			continue
		default:
			d.Steps = append(d.Steps, sd)
			ran++
		}
	}
	return d, ran
}

// ---------- auto execution ----------

// rbScope is Runbook.AutoScope.
type rbScope struct {
	ClusterIDs []int64  `json:"cluster_ids"`
	Envs       []string `json:"envs"`
	Hours      string   `json:"hours"` // "09-21" (Asia/Shanghai, end exclusive)
}

// rbNow is overridable in tests.
var rbNow = time.Now

// rbAutoLog keeps the timestamps of recent automatic executions (process-local).
var rbAutoLog struct {
	sync.Mutex
	at []time.Time
}

// rbRecordAutoExec must be called by the executor when it starts an automatic execution.
func rbRecordAutoExec() {
	rbAutoLog.Lock()
	defer rbAutoLog.Unlock()
	rbAutoLog.at = append(rbAutoLog.at, rbNow())
}

// rbAutoExecLastHour counts automatic executions in the last hour and prunes older ones.
func rbAutoExecLastHour() int {
	rbAutoLog.Lock()
	defer rbAutoLog.Unlock()
	cut := rbNow().Add(-time.Hour)
	kept := rbAutoLog.at[:0]
	for _, t := range rbAutoLog.at {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	rbAutoLog.at = kept
	return len(kept)
}

func rbParseHours(s string) (int, int, bool) {
	var a, b int
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%d-%d", &a, &b); err != nil || n != 2 {
		return 0, 0, false
	}
	if a < 0 || a > 23 || b < 1 || b > 24 || a == b {
		return 0, 0, false
	}
	return a, b, true
}

func rbIsProd(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "prod", "production", "prd", "生产":
		return true
	}
	return false
}

// autoExecutable decides whether a matched runbook may run without a human:
// never for high risk (production change counts as high), only when the policy
// enables auto execution, under the hourly cap, and within the runbook's scope.
func (m *Module) autoExecutable(rb Runbook, c *Case, pol Policy) bool {
	if c == nil || !rb.AutoExecute || !pol.AutoExecute.Enabled {
		return false
	}
	risk := rb.Risk
	if r, err := rbPlanRisk(rb.Plan); err == nil {
		risk = MaxRisk(risk, r)
	} else {
		return false
	}
	if risk == "" || risk == RiskHigh {
		return false
	}
	if risk == RiskChange && rbIsProd(c.Env) {
		return false
	}
	if pol.AutoExecute.MaxPerHour <= 0 || rbAutoExecLastHour() >= pol.AutoExecute.MaxPerHour {
		return false
	}
	var sc rbScope
	if rbEmptyJSON(rb.AutoScope) || json.Unmarshal(rb.AutoScope, &sc) != nil {
		return false
	}
	if len(sc.Envs) == 0 || !rbAnyEq(sc.Envs, strings.ToLower(c.Env)) {
		return false
	}
	if len(sc.ClusterIDs) > 0 {
		if c.ClusterID == nil {
			return false
		}
		found := false
		for _, id := range sc.ClusterIDs {
			if id == *c.ClusterID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if sc.Hours != "" {
		a, b, ok := rbParseHours(sc.Hours)
		if !ok {
			return false
		}
		h := rbNow().In(policyLoc()).Hour()
		if a < b {
			if h < a || h >= b {
				return false
			}
		} else if h < a && h >= b { // overnight e.g. 22-06
			return false
		}
	}
	return true
}
