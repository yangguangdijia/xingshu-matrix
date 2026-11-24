package aiops

// cases.go — case lifecycle (create / investigate / review / close), the
// background investigation queue and the HTTP routes of /api/v1/aiops.
// Persistence is store.go; investigation is investigate.go; execution is
// executor.go; runbooks/policy are runbooks.go / policy.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ---------------------------------------------------------------- queue

const (
	caseQueueSize       = 64
	caseWorkerCount     = 2
	caseInvestigateTime = 3 * time.Minute
	caseMaxRequestLen   = 4000
	caseMaxTitleRunes   = 80
	caseMaxNoteLen      = 2000
)

// caseJob is one queued investigation. Principal is the operator who asked
// (nil for jobs recovered at start-up); it is only used to describe steps.
type caseJob struct {
	ID        int64
	Principal *rbac.Principal
}

// caseQueue is bounded; createCase never blocks on it (a full queue marks the
// case failed with a Chinese message so the operator can re-investigate).
var caseQueue = make(chan caseJob, caseQueueSize)

func caseEnqueue(j caseJob) bool {
	select {
	case caseQueue <- j:
		return true
	default:
		return false
	}
}

// runWorkers is started by Module.Start (module.go).
// It first lets executor.go fail executions orphaned by a restart, then
// re-queues orphaned investigations and starts the investigation workers.
func (m *Module) runWorkers(ctx context.Context) {
	if m.d != nil && m.d.DB != nil {
		m.recoverExecutions(ctx)
	}
	m.caseRecover(ctx)
	for i := 0; i < caseWorkerCount; i++ {
		go m.caseWorker(ctx)
	}
	<-ctx.Done()
}

func (m *Module) caseWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-caseQueue:
			m.caseRunInvestigation(ctx, j)
		}
	}
}

// caseRecover re-queues cases left in "investigating" by a previous process.
func (m *Module) caseRecover(ctx context.Context) {
	if m.d == nil || m.d.DB == nil {
		return
	}
	items, _, err := m.listCases(ctx, caseFilter{Status: CaseInvestigating, Size: caseQueueSize})
	if err != nil {
		return
	}
	for _, c := range items {
		if !caseEnqueue(caseJob{ID: c.ID}) {
			return
		}
	}
}

// caseRunInvestigation runs one queued investigation to concluded / failed.
func (m *Module) caseRunInvestigation(parent context.Context, j caseJob) {
	if j.Principal != nil {
		parent = rbac.WithPrincipal(parent, j.Principal)
	}
	ctx, cancel := context.WithTimeout(parent, caseInvestigateTime)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			m.caseFail(parent, j.ID, fmt.Sprintf("排查过程异常：%v", r))
		}
	}()
	c, err := m.getCase(ctx, j.ID)
	if err != nil {
		return
	}
	if c.Status != CaseInvestigating {
		return
	}
	conclusion, drafts, evidence, engine, err := m.investigate(ctx, c)
	if err != nil {
		m.caseFail(parent, c.ID, "排查失败："+err.Error())
		return
	}
	if engine == "" {
		engine = "rules"
	}
	if evidence == nil {
		evidence = []EvidenceItem{}
	}
	if err := m.saveEvidence(ctx, c.ID, evidence); err != nil {
		m.caseFail(parent, c.ID, "保存排查证据失败："+err.Error())
		return
	}
	if err := m.saveConclusion(ctx, c.ID, conclusion, engine); err != nil {
		m.caseFail(parent, c.ID, "保存排查结论失败："+err.Error())
		return
	}
	if err := m.supersedePlans(ctx, c.ID); err != nil {
		m.caseFail(parent, c.ID, "更新旧方案失败："+err.Error())
		return
	}
	ec := m.caseExecContext(c, j.Principal)
	stored, dropped := 0, []string{}
	for i, d := range drafts {
		if stored >= 3 {
			break
		}
		p, err := caseBuildPlan(c.ID, i+1, d)
		if err != nil {
			dropped = append(dropped, d.Title+"："+err.Error())
			continue
		}
		m.caseDescribeSteps(ctx, ec, p.Steps)
		p.Risk = planRisk(p.Steps)
		p.Rollback = caseRollbackSummary(p.Steps)
		if err := m.insertPlan(ctx, p); err != nil {
			m.caseFail(parent, c.ID, "保存方案失败："+err.Error())
			return
		}
		stored++
	}
	if len(dropped) > 0 {
		_ = m.appendTimeline(ctx, c.ID, "plan_dropped", "system", map[string]any{"reasons": dropped})
	}
	if err := m.updateCaseStatus(ctx, c.ID, CaseConcluded, ""); err != nil {
		m.caseFail(parent, c.ID, "更新排查单状态失败："+err.Error())
		return
	}
	_ = m.appendTimeline(ctx, c.ID, "concluded", "system", map[string]any{
		"engine": engine, "summary": conclusion.Summary, "confidence": conclusion.Confidence, "plans": stored,
	})
	c.Status = CaseConcluded
	m.autoRemediate(ctx, c, conclusion, j.Principal)
}

func (m *Module) caseFail(ctx context.Context, id int64, msg string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := m.updateCaseStatus(ctx, id, CaseFailed, msg); err != nil {
		slog.Warn("aiops: mark case failed", "case", id, "err", err)
	}
	_ = m.appendTimeline(ctx, id, "investigate_failed", "system", map[string]any{"error": msg})
}

func (m *Module) caseExecContext(c *Case, p *rbac.Principal) *ExecContext {
	ec := &ExecContext{Deps: m.d, Principal: p, Case: c, Env: c.Env, Reason: "AI 运维排查单 #" + strconv.FormatInt(c.ID, 10)}
	if c.ClusterID != nil {
		ec.ClusterID = *c.ClusterID
	}
	return ec
}

// caseBuildPlan turns a draft into a storable plan. Every step must reference
// a registered action; steps keep the draft's params until described.
func caseBuildPlan(caseID int64, seq int, d PlanDraft) (*Plan, error) {
	if len(d.Steps) == 0 && len(d.Verify) == 0 {
		return nil, errors.New("方案没有步骤")
	}
	p := &Plan{CaseID: caseID, Seq: seq, Title: strings.TrimSpace(d.Title), Description: d.Description, Status: "proposed"}
	if p.Title == "" {
		p.Title = "方案 " + strconv.Itoa(seq)
	}
	add := func(sd StepDraft, phase string) error {
		a, ok := ActionByName(sd.Action)
		if !ok {
			return fmt.Errorf("动作 %q 未注册", sd.Action)
		}
		params := sd.Params
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		p.Steps = append(p.Steps, Step{
			Seq: len(p.Steps) + 1, Phase: phase, Action: sd.Action, Params: params,
			Title: a.Spec().Title, Risk: a.Spec().Risk, Manual: sd.Note, Executor: ModeAI, Status: StepPending,
		})
		return nil
	}
	for _, s := range d.Steps {
		if err := add(s, "main"); err != nil {
			return nil, err
		}
	}
	for _, s := range d.Verify {
		if err := add(s, "verify"); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// caseDescribeSteps validates and describes steps in place. Failures are
// recorded on the step (Error) rather than dropping the plan, so the reviewer
// sees exactly what is wrong.
func (m *Module) caseDescribeSteps(ctx context.Context, ec *ExecContext, steps []Step) {
	for i := range steps {
		s := &steps[i]
		a, ok := ActionByName(s.Action)
		if !ok {
			s.Error = "动作未注册"
			s.Status = StepBlocked
			continue
		}
		s.Error = ""
		params, risk, err := caseSafeValidate(ctx, a, ec, s.Params)
		if err != nil {
			s.Error = "参数校验失败：" + err.Error()
			s.Risk = MaxRisk(a.Spec().Risk, s.Risk)
			continue
		}
		s.Params = params
		s.Risk = MaxRisk(a.Spec().Risk, risk)
		if title, preview, manual, err := caseSafeDescribe(ctx, a, ec, params); err == nil {
			if title != "" {
				s.Title = title
			}
			s.Preview, s.Manual = preview, manual
		} else {
			s.Preview = "预览失败：" + err.Error()
		}
	}
}

func caseSafeValidate(ctx context.Context, a Action, ec *ExecContext, params json.RawMessage) (out json.RawMessage, risk string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("校验异常：%v", r)
		}
	}()
	out, risk, err = a.Validate(ctx, ec, params)
	if err == nil && len(out) == 0 {
		out = params
	}
	return
}

func caseSafeDescribe(ctx context.Context, a Action, ec *ExecContext, params json.RawMessage) (title, preview, manual string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("预览异常：%v", r)
		}
	}()
	return a.Describe(ctx, ec, params)
}

func caseRollbackSummary(steps []Step) string {
	var parts []string
	for _, s := range steps {
		if s.Phase != "main" || s.Risk == RiskRead {
			continue
		}
		parts = append(parts, fmt.Sprintf("步骤 %d（%s）", s.Seq, s.Action))
	}
	if len(parts) == 0 {
		return "仅只读操作，无需回滚"
	}
	return "失败时按逆序回滚：" + strings.Join(parts, "、") + "；不支持自动回滚的步骤需人工处理"
}

// ---------------------------------------------------------------- createCase

// caseNormalizeInput validates and normalises a create request (pure).
func caseNormalizeInput(in *CreateCaseInput) error {
	in.Kind = strings.TrimSpace(in.Kind)
	if in.Kind == "" {
		in.Kind = KindRequest
	}
	switch in.Kind {
	case KindAlert, KindDeploy, KindRequest:
	default:
		return httpx.Validation("kind 只能是 alert / deploy / request")
	}
	in.Source = strings.TrimSpace(in.Source)
	if in.Source == "" {
		in.Source = SourceManual
	}
	switch in.Source {
	case SourceAlert, SourceChat, SourceManual, SourceAuto:
	default:
		return httpx.Validation("source 不合法")
	}
	in.Request = strings.TrimSpace(in.Request)
	if in.Request == "" {
		return httpx.Validation("请填写排查请求描述")
	}
	if len(in.Request) > caseMaxRequestLen {
		return httpx.Validation(fmt.Sprintf("请求描述过长（最多 %d 字节）", caseMaxRequestLen))
	}
	in.Env = strings.TrimSpace(in.Env)
	if len(in.Env) > 64 {
		return httpx.Validation("env 过长")
	}
	if in.ClusterID != nil && *in.ClusterID <= 0 {
		return httpx.Validation("cluster_id 不合法")
	}
	if in.AlertID != nil && *in.AlertID <= 0 {
		return httpx.Validation("alert_id 不合法")
	}
	if in.AssetID != nil && *in.AssetID <= 0 {
		return httpx.Validation("asset_id 不合法")
	}
	if len(in.Target) == 0 || string(in.Target) == "null" {
		in.Target = []byte(`{}`)
	} else {
		var obj map[string]any
		if err := json.Unmarshal(in.Target, &obj); err != nil || obj == nil {
			return httpx.Validation("target 必须是 JSON 对象")
		}
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		in.Title = caseTitleFrom(in.Request)
	}
	in.Title = caseTruncRunes(in.Title, caseMaxTitleRunes)
	return nil
}

func caseTitleFrom(req string) string {
	line := strings.TrimSpace(strings.SplitN(req, "\n", 2)[0])
	return caseTruncRunes(line, 40)
}

func caseTruncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func casePrincipalName(p *rbac.Principal) string {
	if p == nil {
		return "system"
	}
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Username
}

func casePrincipalID(p *rbac.Principal) int64 {
	if p == nil {
		return 0
	}
	return p.UserID
}

// createCase stores a case and queues its investigation.
func (m *Module) createCase(ctx context.Context, p *rbac.Principal, in CreateCaseInput) (*Case, error) {
	if err := caseNormalizeInput(&in); err != nil {
		return nil, err
	}
	if p != nil && in.Env != "" && !p.CanEnv(in.Env) {
		return nil, httpx.Forbidden("无权操作该环境：" + in.Env)
	}
	if p != nil && in.ClusterID != nil && !p.CanCluster(*in.ClusterID) {
		return nil, httpx.Forbidden("无权访问该集群")
	}
	c := &Case{
		Kind: in.Kind, Source: in.Source, Title: in.Title, Status: CaseInvestigating, Env: in.Env,
		ClusterID: in.ClusterID, AlertID: in.AlertID, AssetID: in.AssetID, Request: in.Request,
		Target: json.RawMessage(in.Target), Evidence: json.RawMessage(`[]`), Conclusion: json.RawMessage(`{}`),
		CreatedBy: casePrincipalID(p), CreatedName: casePrincipalName(p),
	}
	if err := m.insertCase(ctx, c); err != nil {
		return nil, err
	}
	_ = m.appendTimeline(ctx, c.ID, "created", c.CreatedName, map[string]any{"kind": c.Kind, "source": c.Source, "request": c.Request})
	if !caseEnqueue(caseJob{ID: c.ID, Principal: p}) {
		msg := "排查队列已满，请稍后点击“重新排查”"
		_ = m.updateCaseStatus(ctx, c.ID, CaseFailed, msg)
		_ = m.appendTimeline(ctx, c.ID, "investigate_failed", "system", map[string]any{"error": msg})
		c.Status, c.Error = CaseFailed, msg
		return c, nil
	}
	_ = m.appendTimeline(ctx, c.ID, "investigating", "system", nil)
	return c, nil
}

// ---------------------------------------------------------------- routes

// registerCases is called by Module.Register; it mounts every aiops route.
func (m *Module) registerCases(g *gin.RouterGroup) { m.registerRoutes(g) }

// registerActionsAPI is kept for module.go; GET /actions is mounted by registerRoutes.
func (m *Module) registerActionsAPI(g *gin.RouterGroup) {}

// registerRoutes mounts the case routes and the executor routes. Runbook,
// policy and host-onboarding routes are mounted by registerRunbooks
// (runbooks.go), which module.go calls separately; registering them here as
// well would make gin panic on duplicate routes.
//
// Permissions: read aiops:read; investigate / review / close aiops:handle;
// execute aiops:execute (executor.go decides per route); manage aiops:manage.
func (m *Module) registerRoutes(g *gin.RouterGroup) {
	read := rbac.RequirePerm(PermRead)
	handle := rbac.RequirePerm(PermHandle)

	g.GET("/cases", read, m.handleListCases)
	g.POST("/cases", handle, m.handleCreateCase)
	g.POST("/cases/from-alert/:alert_id", handle, m.handleCaseFromAlert)
	g.GET("/cases/:id", read, m.handleGetCase)
	g.POST("/cases/:id/reinvestigate", handle, m.handleReinvestigate)
	g.POST("/cases/:id/plans/:pid/review", handle, m.handleReviewPlan)
	g.POST("/cases/:id/close", handle, m.handleCloseCase)
	g.GET("/actions", read, m.handleListActions)

	// executor.go: execute / steps run+manual / pause / resume / abort / rollback
	// (also registers the aiops.execute approval handler).
	m.registerExecutor(g)
}

// ---------------------------------------------------------------- handlers

func caseRespondErr(c *gin.Context, err error) {
	if errors.Is(err, errStoreNotFound) {
		httpx.Fail(c, httpx.NotFound("排查单或方案不存在"))
		return
	}
	httpx.Fail(c, err)
}

func caseParamID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, httpx.BadRequest("无效的 "+name))
		return 0, false
	}
	return id, true
}

// caseLoad loads :id and checks the principal's env / cluster scope.
func (m *Module) caseLoad(c *gin.Context) (*Case, bool) {
	id, ok := caseParamID(c, "id")
	if !ok {
		return nil, false
	}
	cs, err := m.getCase(c, id)
	if err != nil {
		caseRespondErr(c, err)
		return nil, false
	}
	if err := caseCheckScope(rbac.PrincipalFrom(c), cs); err != nil {
		httpx.Fail(c, err)
		return nil, false
	}
	return cs, true
}

func caseCheckScope(p *rbac.Principal, cs *Case) error {
	if p == nil {
		return nil
	}
	if cs.Env != "" && !p.CanEnv(cs.Env) {
		return httpx.Forbidden("无权访问该环境的排查单")
	}
	if cs.ClusterID != nil && !p.CanCluster(*cs.ClusterID) {
		return httpx.Forbidden("无权访问该集群的排查单")
	}
	return nil
}

func (m *Module) handleListCases(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	size, _ := strconv.Atoi(c.Query("size"))
	f := caseFilter{Status: c.Query("status"), Kind: c.Query("kind"), Q: strings.TrimSpace(c.Query("q")), Page: page, Size: size}
	items, total, err := m.listCases(c, f)
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	out := make([]Case, 0, len(items))
	for i := range items {
		if caseCheckScope(p, &items[i]) == nil {
			out = append(out, items[i])
		}
	}
	sz, _ := normPage(page, size)
	if page <= 0 {
		page = 1
	}
	httpx.OK(c, gin.H{"items": out, "total": total, "page": page, "size": sz})
}

type caseCreateBody struct {
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Request   string          `json:"request"`
	Env       string          `json:"env"`
	ClusterID *int64          `json:"cluster_id"`
	AlertID   *int64          `json:"alert_id"`
	AssetID   *int64          `json:"asset_id"`
	Target    json.RawMessage `json:"target"`
}

func (m *Module) handleCreateCase(c *gin.Context) {
	var b caseCreateBody
	if err := c.ShouldBindJSON(&b); err != nil {
		httpx.Fail(c, httpx.BadRequest("请求体不是合法 JSON"))
		return
	}
	cs, err := m.createCase(c, rbac.PrincipalFrom(c), CreateCaseInput{
		Kind: b.Kind, Source: SourceManual, Title: b.Title, Request: b.Request, Env: b.Env,
		ClusterID: b.ClusterID, AlertID: b.AlertID, AssetID: b.AssetID, Target: b.Target,
	})
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	httpx.Created(c, cs)
}

// caseAlert is the subset of obs_alerts a case needs.
type caseAlert struct {
	ID          int64
	Alertname   string
	Severity    string
	Status      string
	Labels      map[string]string
	Annotations map[string]string
}

func (m *Module) caseLoadAlert(ctx context.Context, id int64) (*caseAlert, error) {
	var a caseAlert
	var labels, ann []byte
	err := m.d.DB.QueryRow(ctx, `SELECT id, alertname, severity, status, labels, annotations FROM obs_alerts WHERE id=$1`, id).
		Scan(&a.ID, &a.Alertname, &a.Severity, &a.Status, &labels, &ann)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" {
			return nil, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "告警表不存在：请先启用可观测性（告警）模块")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("告警不存在")
		}
		return nil, err
	}
	_ = json.Unmarshal(labels, &a.Labels)
	_ = json.Unmarshal(ann, &a.Annotations)
	return &a, nil
}

// caseInputFromAlert builds a create request from an alert (pure).
func caseInputFromAlert(a *caseAlert) CreateCaseInput {
	l := a.Labels
	if l == nil {
		l = map[string]string{}
	}
	in := CreateCaseInput{Kind: KindAlert, Source: SourceAlert, AlertID: &a.ID}
	in.Title = "告警排查：" + a.Alertname
	if a.Severity != "" {
		in.Title += "（" + a.Severity + "）"
	}
	for _, k := range []string{"env", "environment"} {
		if v := strings.TrimSpace(l[k]); v != "" {
			in.Env = v
			break
		}
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(l["cluster_id"]), 10, 64); err == nil && v > 0 {
		in.ClusterID = &v
	}
	target := map[string]string{}
	if ns := l["namespace"]; ns != "" {
		target["namespace"] = ns
	}
	for _, k := range []string{"deployment", "statefulset", "daemonset", "pod", "node"} {
		if v := l[k]; v != "" {
			target["kind"], target["name"] = k, v
			break
		}
	}
	in.Target, _ = json.Marshal(target)
	var sb strings.Builder
	fmt.Fprintf(&sb, "告警 %s（级别 %s，状态 %s）", a.Alertname, a.Severity, a.Status)
	if s := a.Annotations["summary"]; s != "" {
		sb.WriteString("\n摘要：" + s)
	}
	if s := a.Annotations["description"]; s != "" {
		sb.WriteString("\n描述：" + s)
	}
	in.Request = caseTruncRunes(sb.String(), 1500)
	return in
}

func (m *Module) handleCaseFromAlert(c *gin.Context) {
	id, ok := caseParamID(c, "alert_id")
	if !ok {
		return
	}
	a, err := m.caseLoadAlert(c, id)
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	cs, err := m.createCase(c, rbac.PrincipalFrom(c), caseInputFromAlert(a))
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	httpx.Created(c, cs)
}

func (m *Module) handleGetCase(c *gin.Context) {
	cs, ok := m.caseLoad(c)
	if !ok {
		return
	}
	plans, err := m.listPlans(c, cs.ID)
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	tl, err := m.listTimeline(c, cs.ID)
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	if plans == nil {
		plans = []Plan{}
	}
	if tl == nil {
		tl = []TimelineEntry{}
	}
	for i := range plans {
		for j := range plans[i].Steps {
			plans[i].Steps[j].Output = StripK8sState(plans[i].Steps[j].Output)
		}
	}
	httpx.OK(c, gin.H{"case": cs, "plans": plans, "timeline": tl})
}

// caseCanReinvestigate reports whether a case may go back to investigating.
func caseCanReinvestigate(status string) bool {
	switch status {
	case CaseConcluded, CaseRejected, CaseFailed, CaseAccepted, CaseResolved, CaseCancelled:
		return true
	}
	return false
}

func (m *Module) handleReinvestigate(c *gin.Context) {
	cs, ok := m.caseLoad(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	if len(b.Note) > caseMaxNoteLen {
		httpx.Fail(c, httpx.Validation("备注过长"))
		return
	}
	if !caseCanReinvestigate(cs.Status) {
		httpx.Fail(c, httpx.Conflict("当前状态（"+cs.Status+"）不能重新排查"))
		return
	}
	p := rbac.PrincipalFrom(c)
	if err := m.updateCaseStatus(c, cs.ID, CaseInvestigating, ""); err != nil {
		caseRespondErr(c, err)
		return
	}
	_ = m.appendTimeline(c, cs.ID, "reinvestigate", casePrincipalName(p), map[string]any{"note": strings.TrimSpace(b.Note)})
	if !caseEnqueue(caseJob{ID: cs.ID, Principal: p}) {
		msg := "排查队列已满，请稍后重试"
		_ = m.updateCaseStatus(c, cs.ID, CaseFailed, msg)
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, msg))
		return
	}
	cs.Status, cs.Error = CaseInvestigating, ""
	httpx.OK(c, cs)
}

// caseStepEdit is one reviewer change to a step: only params and executor.
type caseStepEdit struct {
	ID       int64           `json:"id"`
	Params   json.RawMessage `json:"params"`
	Executor string          `json:"executor"`
	// Any other field (action, risk, ...) is rejected.
	Action string `json:"action,omitempty"`
}

type caseReviewBody struct {
	Decision string         `json:"decision"`
	Note     string         `json:"note"`
	Steps    []caseStepEdit `json:"steps"`
}

// caseValidateReview checks the body shape (pure).
func caseValidateReview(b *caseReviewBody) error {
	b.Decision = strings.TrimSpace(b.Decision)
	if b.Decision != "accept" && b.Decision != "reject" {
		return httpx.Validation("decision 只能是 accept 或 reject")
	}
	b.Note = strings.TrimSpace(b.Note)
	if len(b.Note) > caseMaxNoteLen {
		return httpx.Validation("备注过长")
	}
	if b.Decision == "reject" && len(b.Steps) > 0 {
		return httpx.Validation("驳回时不能修改步骤")
	}
	return nil
}

// caseApplyStepEdits applies reviewer edits to plan steps in place (pure).
// Only params and executor may change; the step's action must be registered.
// It returns the indexes of steps whose params changed (need re-validation).
func caseApplyStepEdits(steps []Step, edits []caseStepEdit) ([]int, error) {
	idx := map[int64]int{}
	for i, s := range steps {
		idx[s.ID] = i
	}
	seen := map[int64]bool{}
	var changed []int
	for _, e := range edits {
		i, ok := idx[e.ID]
		if !ok || e.ID == 0 {
			return nil, httpx.Validation(fmt.Sprintf("步骤 %d 不属于该方案", e.ID))
		}
		if seen[e.ID] {
			return nil, httpx.Validation(fmt.Sprintf("步骤 %d 重复修改", e.ID))
		}
		seen[e.ID] = true
		s := &steps[i]
		if e.Action != "" && e.Action != s.Action {
			return nil, httpx.Validation("不允许修改步骤的动作，只能修改参数和执行者")
		}
		if _, ok := ActionByName(s.Action); !ok {
			return nil, httpx.Validation(fmt.Sprintf("步骤 %d 的动作 %q 未注册", e.ID, s.Action))
		}
		switch e.Executor {
		case "":
		case ModeAI, "human":
			s.Executor = e.Executor
		default:
			return nil, httpx.Validation("executor 只能是 ai 或 human")
		}
		if len(e.Params) > 0 && string(e.Params) != "null" {
			var obj map[string]any
			if err := json.Unmarshal(e.Params, &obj); err != nil || obj == nil {
				return nil, httpx.Validation(fmt.Sprintf("步骤 %d 的 params 必须是 JSON 对象", e.ID))
			}
			s.Params = append(json.RawMessage(nil), e.Params...)
			changed = append(changed, i)
		}
	}
	return changed, nil
}

func caseHasProposed(plans []Plan) bool {
	for _, p := range plans {
		if p.Status == "proposed" {
			return true
		}
	}
	return false
}

func (m *Module) handleReviewPlan(c *gin.Context) {
	cs, ok := m.caseLoad(c)
	if !ok {
		return
	}
	pid, ok := caseParamID(c, "pid")
	if !ok {
		return
	}
	var b caseReviewBody
	if err := c.ShouldBindJSON(&b); err != nil {
		httpx.Fail(c, httpx.BadRequest("请求体不是合法 JSON"))
		return
	}
	if err := caseValidateReview(&b); err != nil {
		httpx.Fail(c, err)
		return
	}
	if cs.Status != CaseConcluded && cs.Status != CaseRejected {
		httpx.Fail(c, httpx.Conflict("排查单当前状态（"+cs.Status+"）不能审阅方案"))
		return
	}
	plan, err := m.getPlan(c, pid)
	if err != nil {
		caseRespondErr(c, err)
		return
	}
	if plan.CaseID != cs.ID {
		httpx.Fail(c, httpx.NotFound("方案不属于该排查单"))
		return
	}
	if plan.Status != "proposed" {
		httpx.Fail(c, httpx.Conflict("该方案已处理（"+plan.Status+"）"))
		return
	}
	p := rbac.PrincipalFrom(c)
	actor := casePrincipalName(p)

	if b.Decision == "reject" {
		plan.Status, plan.RejectNote = "rejected", b.Note
		if err := m.updatePlan(c, plan); err != nil {
			caseRespondErr(c, err)
			return
		}
		_ = m.appendTimeline(c, cs.ID, "plan_rejected", actor, map[string]any{"plan_id": plan.ID, "title": plan.Title, "note": b.Note})
		plans, err := m.listPlans(c, cs.ID)
		if err == nil && !caseHasProposed(plans) {
			_ = m.updateCaseStatus(c, cs.ID, CaseRejected, "")
			cs.Status = CaseRejected
		}
		httpx.OK(c, gin.H{"case": cs, "plan": plan})
		return
	}

	changed, err := caseApplyStepEdits(plan.Steps, b.Steps)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if len(changed) > 0 {
		ec := m.caseExecContext(cs, p)
		for _, i := range changed {
			m.caseDescribeSteps(c, ec, plan.Steps[i:i+1])
			if plan.Steps[i].Error != "" {
				httpx.Fail(c, httpx.Validation(fmt.Sprintf("步骤 %d：%s", plan.Steps[i].ID, plan.Steps[i].Error)))
				return
			}
		}
		plan.Risk = planRisk(plan.Steps)
		plan.Rollback = caseRollbackSummary(plan.Steps)
	}
	plan.Status = "accepted"
	if err := m.updatePlan(c, plan); err != nil {
		caseRespondErr(c, err)
		return
	}
	cs.PlanID = &plan.ID
	cs.Status = CaseAccepted
	cs.ReviewedBy = actor
	if err := m.updateCase(c, cs); err != nil {
		caseRespondErr(c, err)
		return
	}
	_ = m.appendTimeline(c, cs.ID, "plan_accepted", actor, map[string]any{
		"plan_id": plan.ID, "title": plan.Title, "risk": plan.Risk, "note": b.Note, "edited_steps": len(changed),
	})
	httpx.OK(c, gin.H{"case": cs, "plan": plan})
}

// caseCanClose reports whether a case may be closed by hand.
func caseCanClose(status string) bool {
	switch status {
	case CaseExecuting, CaseVerifying, CaseAwaitApproval, CaseResolved, CaseCancelled:
		return false
	}
	return true
}

func (m *Module) handleCloseCase(c *gin.Context) {
	cs, ok := m.caseLoad(c)
	if !ok {
		return
	}
	var b struct {
		Resolution string `json:"resolution"`
		Status     string `json:"status"` // resolved (default) | cancelled
	}
	_ = c.ShouldBindJSON(&b)
	b.Resolution = strings.TrimSpace(b.Resolution)
	if len(b.Resolution) > caseMaxNoteLen {
		httpx.Fail(c, httpx.Validation("处理说明过长"))
		return
	}
	status := CaseResolved
	if b.Status == CaseCancelled {
		status = CaseCancelled
	} else if b.Status != "" && b.Status != CaseResolved {
		httpx.Fail(c, httpx.Validation("status 只能是 resolved 或 cancelled"))
		return
	}
	if !caseCanClose(cs.Status) {
		httpx.Fail(c, httpx.Conflict("当前状态（"+cs.Status+"）不能关闭，执行中请先中止"))
		return
	}
	if err := m.updateCaseStatus(c, cs.ID, status, ""); err != nil {
		caseRespondErr(c, err)
		return
	}
	_ = m.appendTimeline(c, cs.ID, "closed", casePrincipalName(rbac.PrincipalFrom(c)), map[string]any{"status": status, "resolution": b.Resolution})
	cs.Status = status
	httpx.OK(c, cs)
}

func (m *Module) handleListActions(c *gin.Context) {
	httpx.OK(c, ActionSpecs())
}
