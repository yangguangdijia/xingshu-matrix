package aiops

// executor.go: executes an accepted plan (AI / manual checklist / mixed),
// with risk grading, approval gate, change window, rollback and verification.
//
// Wiring (owned by other files):
//   - cases.go registerRoutes calls m.registerExecutor(g) (only there).
//   - cases.go runWorkers calls m.recoverExecutions(ctx) at start, so cases left
//     executing/verifying/paused by a restart are failed.
//   - automatic remediation starts through m.execStartAuto (calls rbRecordAutoExec).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	execTicketType     = "aiops.execute"
	execMaxOutput      = 8 << 10
	execExecutorAI     = "ai"
	execExecutorHuman  = "human"
	execPhaseVerify    = "verify"
	execPhaseRollback  = "rollback"
	execPlanAccepted   = "accepted"
	execRollbackTimout = 5 * time.Minute
)

// execStore is the persistence the engine needs; *Module satisfies it via store.go.
type execStore interface {
	getCase(ctx context.Context, id int64) (*Case, error)
	getPlan(ctx context.Context, id int64) (*Plan, error)
	updatePlan(ctx context.Context, p *Plan) error
	updateCase(ctx context.Context, c *Case) error
	updateCaseStatus(ctx context.Context, id int64, status, errMsg string) error
	appendTimeline(ctx context.Context, caseID int64, kind, actor string, detail any) error
}

// ---------------------------------------------------------------- run registry

type execCtl struct {
	stepID    int64
	kind      string // manual | run
	status    string // manual: done | skipped | failed
	note      string
	actor     string
	principal *rbac.Principal
	reply     chan error
}

type execRun struct {
	caseID int64
	cancel context.CancelFunc
	ctl    chan execCtl

	mu      sync.Mutex
	paused  bool
	resume  chan struct{}
	aborted bool
	phase   string // CaseExecuting | CaseVerifying: status to show when not paused
}

var execRuns = struct {
	sync.Mutex
	m map[int64]*execRun
}{m: map[int64]*execRun{}}

func execNewRun(caseID int64, cancel context.CancelFunc) *execRun {
	return &execRun{caseID: caseID, cancel: cancel, ctl: make(chan execCtl), phase: CaseExecuting}
}

func execRegister(caseID int64, cancel context.CancelFunc) (*execRun, bool) {
	execRuns.Lock()
	defer execRuns.Unlock()
	if _, busy := execRuns.m[caseID]; busy {
		return nil, false
	}
	r := execNewRun(caseID, cancel)
	execRuns.m[caseID] = r
	return r, true
}

func execUnregister(caseID int64, r *execRun) {
	execRuns.Lock()
	defer execRuns.Unlock()
	if execRuns.m[caseID] == r {
		delete(execRuns.m, caseID)
	}
}

func execLookupRun(caseID int64) *execRun {
	execRuns.Lock()
	defer execRuns.Unlock()
	return execRuns.m[caseID]
}

// execRunState reports whether a case has a live execution and whether it is
// paused (for GET /cases/:id in cases.go, optional).
func execRunState(caseID int64) (running, paused bool) {
	r := execLookupRun(caseID)
	if r == nil {
		return false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return true, r.paused
}

// pause marks the run paused and, under the run lock, writes CasePaused via set.
func (r *execRun) pause(set func(status string)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return false
	}
	r.paused = true
	r.resume = make(chan struct{})
	if set != nil {
		set(CasePaused)
	}
	return true
}

// resumeRun clears the pause and writes the phase status (executing/verifying).
func (r *execRun) resumeRun(set func(status string)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		return false
	}
	r.paused = false
	close(r.resume)
	if set != nil {
		set(r.phase)
	}
	return true
}

// setPhase records the engine's phase status; while paused the case keeps
// showing CasePaused and the phase is applied on resume.
func (r *execRun) setPhase(status string, set func(status string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = status
	if !r.paused {
		set(status)
	}
}

func (r *execRun) abort() {
	r.mu.Lock()
	r.aborted = true
	r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *execRun) waitIfPaused(ctx context.Context) error {
	for {
		r.mu.Lock()
		if !r.paused {
			r.mu.Unlock()
			return ctx.Err()
		}
		ch := r.resume
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// execSend hands a control message to the engine waiting on that step.
func execSend(r *execRun, msg execCtl) error {
	msg.reply = make(chan error, 1)
	select {
	case r.ctl <- msg:
	case <-time.After(3 * time.Second):
		return httpx.Conflict("该步骤当前不在等待人工处理")
	}
	select {
	case err := <-msg.reply:
		return err
	case <-time.After(10 * time.Second):
		return httpx.Conflict("执行引擎未响应，请稍后刷新")
	}
}

// ---------------------------------------------------------------- helpers

func execIsProd(env string) bool {
	e := strings.ToLower(strings.TrimSpace(env))
	return e == "prod" || e == "production" || approval.IsProductionEnv(env)
}

// execElevate raises change to high in production.
func execElevate(risk, env string) string {
	if risk == RiskChange && execIsProd(env) {
		return RiskHigh
	}
	return risk
}

func execTruncate(s string) string {
	if len(s) <= execMaxOutput {
		return s
	}
	b := s[:execMaxOutput]
	for i := 0; i < 3 && !utf8.ValidString(b); i++ {
		b = b[:len(b)-1]
	}
	return b + "\n…（输出已截断）"
}

func execActor(p *rbac.Principal) string {
	if p == nil {
		return "system"
	}
	if p.DisplayName != "" {
		return p.DisplayName
	}
	if p.Username != "" {
		return p.Username
	}
	return "user#" + strconv.FormatInt(p.UserID, 10)
}

func execStepExecutor(mode string, s Step) string {
	switch mode {
	case ModeManual:
		return execExecutorHuman
	case ModeAI:
		return execExecutorAI
	}
	if s.Executor == execExecutorHuman {
		return execExecutorHuman
	}
	return execExecutorAI
}

func execValidMode(mode string) bool {
	return mode == ModeAI || mode == ModeManual || mode == ModeMixed
}

func (m *Module) execContext(p *rbac.Principal, cs *Case) *ExecContext {
	ec := &ExecContext{Deps: m.d, Principal: p, Case: cs, Env: cs.Env,
		Reason: fmt.Sprintf("AI 运维单 #%d：%s", cs.ID, cs.Title)}
	if cs.ClusterID != nil {
		ec.ClusterID = *cs.ClusterID
	}
	return ec
}

// execPrepare checks permissions, applies executors, validates params and
// returns the effective plan risk (production change elevated to high).
// It mutates plan.Steps (Executor, Params, Risk) and plan.Risk.
func execPrepare(ctx context.Context, ec *ExecContext, plan *Plan, mode string, executors map[string]string, lookup func(string) (Action, bool)) (string, error) {
	if !execValidMode(mode) {
		return "", httpx.Validation("mode 必须为 ai、manual 或 mixed")
	}
	p := ec.Principal
	if p == nil {
		return "", httpx.Unauthorized("需要登录")
	}
	if !p.CanEnv(ec.Env) {
		return "", httpx.Forbidden("环境不在你的权限范围内：" + ec.Env)
	}
	known := map[string]bool{}
	for _, s := range plan.Steps {
		known[strconv.FormatInt(s.ID, 10)] = true
	}
	for k, v := range executors {
		if !known[k] {
			return "", httpx.Validation("executors 中的步骤不存在：" + k)
		}
		if v != execExecutorAI && v != execExecutorHuman {
			return "", httpx.Validation("executor 必须为 ai 或 human")
		}
	}
	if len(plan.Steps) == 0 {
		return "", httpx.Validation("方案没有可执行的步骤")
	}
	needAI := false
	for i := range plan.Steps {
		s := &plan.Steps[i]
		if s.Phase == execPhaseRollback {
			continue
		}
		a, ok := lookup(s.Action)
		if !ok {
			return "", httpx.Validation("方案包含未注册的动作：" + s.Action)
		}
		spec := a.Spec()
		if spec.Permission != "" && !p.Can(spec.Permission) {
			return "", httpx.Forbidden(fmt.Sprintf("缺少步骤「%s」所需权限：%s", s.Action, spec.Permission))
		}
		if mode == ModeMixed {
			if v, ok := executors[strconv.FormatInt(s.ID, 10)]; ok {
				s.Executor = v
			}
		}
		s.Executor = execStepExecutor(mode, *s)
		if s.Executor == execExecutorAI {
			needAI = true
		}
		norm, r, err := a.Validate(ctx, ec, s.Params)
		if err != nil {
			return "", httpx.Validation(fmt.Sprintf("步骤 %d（%s）参数无效：%v", s.Seq, s.Action, err))
		}
		if len(norm) > 0 {
			s.Params = norm
		}
		risk := spec.Risk
		if risk == "" {
			risk = RiskHigh // unknown risk fails closed
		}
		if s.Risk != "" {
			risk = MaxRisk(risk, s.Risk)
		}
		if r != "" {
			risk = MaxRisk(risk, r)
		}
		s.Risk = execElevate(risk, ec.Env)
	}
	if needAI && !p.Can(PermExecute) {
		return "", httpx.Forbidden("缺少权限：" + PermExecute + "（平台执行步骤需要）")
	}
	if !needAI && !p.Can(PermHandle) && !p.Can(PermExecute) {
		return "", httpx.Forbidden("缺少权限：" + PermHandle)
	}
	risk := execElevate(planRisk(plan.Steps), ec.Env)
	plan.Risk = risk
	return risk, nil
}

// ---------------------------------------------------------------- engine

type execEngine struct {
	st      execStore
	lookup  func(string) (Action, bool)
	ec      *ExecContext
	plan    *Plan
	mode    string
	actor   string
	r       *execRun
	outputs map[int64]string
	now     func() time.Time
}

func newExecEngine(st execStore, ec *ExecContext, plan *Plan, mode string, r *execRun) *execEngine {
	return &execEngine{st: st, lookup: ActionByName, ec: ec, plan: plan, mode: mode,
		actor: execActor(ec.Principal), r: r, outputs: map[int64]string{}, now: time.Now}
}

func (e *execEngine) caseID() int64 { return e.ec.Case.ID }

func (e *execEngine) save(bg context.Context) {
	if err := e.st.updatePlan(bg, e.plan); err != nil {
		e.tl(bg, "error", map[string]any{"message": "保存步骤状态失败：" + err.Error()})
	}
}

func (e *execEngine) tl(bg context.Context, kind string, detail any) {
	_ = e.st.appendTimeline(bg, e.caseID(), kind, e.actor, detail)
}

func (e *execEngine) setStatus(bg context.Context, status, msg string) {
	write := func(st string) {
		_ = e.st.updateCaseStatus(bg, e.caseID(), st, msg)
		e.ec.Case.Status = st
	}
	if status == CaseExecuting || status == CaseVerifying {
		e.r.setPhase(status, write)
		return
	}
	write(status)
}

// execOrder returns step indices of the given phase group sorted by Seq.
func execOrder(steps []Step, verify bool) []int {
	var idx []int
	for i, s := range steps {
		if s.Phase == execPhaseRollback {
			continue
		}
		if (s.Phase == execPhaseVerify) == verify {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return steps[idx[a]].Seq < steps[idx[b]].Seq })
	return idx
}

func (e *execEngine) stepDetail(s *Step, extra map[string]any) map[string]any {
	d := map[string]any{"step_id": s.ID, "seq": s.Seq, "action": s.Action, "title": s.Title, "phase": s.Phase, "status": s.Status}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// execute runs the whole plan synchronously. Callers run it in a goroutine.
func (e *execEngine) execute(ctx context.Context) {
	bg := context.WithoutCancel(ctx)
	e.setStatus(bg, CaseExecuting, "")
	e.tl(bg, "execute_start", map[string]any{"plan_id": e.plan.ID, "mode": e.mode, "risk": e.plan.Risk})

	for _, i := range execOrder(e.plan.Steps, false) {
		if err := e.r.waitIfPaused(ctx); err != nil {
			e.finishAborted(bg)
			return
		}
		ok, err := e.runStep(ctx, bg, i)
		if ctx.Err() != nil {
			e.finishAborted(bg)
			return
		}
		if !ok {
			msg := "步骤失败"
			if err != nil {
				msg = fmt.Sprintf("步骤 %d 失败：%v", e.plan.Steps[i].Seq, err)
			}
			e.tl(bg, "execute_failed", map[string]any{"step_id": e.plan.Steps[i].ID, "error": msg})
			e.rollbackDone(bg)
			e.setStatus(bg, CaseFailed, msg)
			e.tl(bg, "execute_finish", map[string]any{"status": CaseFailed})
			return
		}
	}

	// A read-only plan without verify steps only gathered information: the
	// case goes back to "concluded" so the operator can review further plans,
	// instead of being marked resolved while the fault is still there.
	if e.plan.Risk == RiskRead && len(execOrder(e.plan.Steps, true)) == 0 {
		e.setStatus(bg, CaseConcluded, "")
		e.tl(bg, "execute_finish", map[string]any{"status": CaseConcluded, "note": "只读诊断已完成，未做任何变更"})
		return
	}

	e.setStatus(bg, CaseVerifying, "")
	e.tl(bg, "verify_start", map[string]any{"plan_id": e.plan.ID})
	allOK := true
	for _, i := range execOrder(e.plan.Steps, true) {
		if err := e.r.waitIfPaused(ctx); err != nil {
			e.finishAborted(bg)
			return
		}
		ok, _ := e.runStep(ctx, bg, i)
		if ctx.Err() != nil {
			e.finishAborted(bg)
			return
		}
		if !ok {
			allOK = false
		}
	}
	if allOK {
		e.setStatus(bg, CaseResolved, "")
	} else {
		e.setStatus(bg, CaseFailed, "验证未通过")
	}
	e.tl(bg, "execute_finish", map[string]any{"status": e.ec.Case.Status})
}

func (e *execEngine) runStep(ctx, bg context.Context, i int) (bool, error) {
	s := &e.plan.Steps[i]
	switch s.Status {
	case StepSucceeded, StepManual, StepSkipped:
		return true, nil
	}
	if execStepExecutor(e.mode, *s) == execExecutorHuman {
		return e.waitHuman(ctx, bg, i)
	}
	return e.execAI(ctx, bg, i, e.ec)
}

func (e *execEngine) execAI(ctx, bg context.Context, i int, ec *ExecContext) (bool, error) {
	s := &e.plan.Steps[i]
	now := e.now()
	s.StartedAt, s.FinishedAt, s.Error = &now, nil, ""
	a, ok := e.lookup(s.Action)
	if !ok {
		err := errors.New("动作未注册：" + s.Action)
		s.Status, s.Error = StepFailed, err.Error()
		s.FinishedAt = &now
		e.save(bg)
		e.tl(bg, "step_failed", e.stepDetail(s, map[string]any{"error": s.Error}))
		return false, err
	}
	s.Status = StepRunning
	e.save(bg)
	e.tl(bg, "step_start", e.stepDetail(s, nil))

	out, err := a.Execute(ctx, ec, s.Params)
	if err == nil {
		if v, ok := a.(execVerifier); ok {
			vout, verr := v.Verify(ctx, ec, s.Params, out)
			if vout != "" {
				out = StripK8sState(out) + "\n[验证] " + vout + execStateSuffix(out)
			}
			err = verr
		}
	}
	e.outputs[s.ID] = out
	s.Output = execTruncate(StripK8sState(out)) + execStateSuffix(out)
	end := e.now()
	s.FinishedAt = &end
	s.DoneBy = execExecutorAI
	if err != nil {
		s.Status, s.Error = StepFailed, execTruncate(err.Error())
		e.save(bg)
		e.tl(bg, "step_failed", e.stepDetail(s, map[string]any{"error": s.Error}))
		return false, err
	}
	s.Status = StepSucceeded
	e.save(bg)
	e.tl(bg, "step_succeeded", e.stepDetail(s, nil))
	return true, nil
}

func (e *execEngine) waitHuman(ctx, bg context.Context, i int) (bool, error) {
	s := &e.plan.Steps[i]
	s.Status = StepBlocked
	e.save(bg)
	e.tl(bg, "step_waiting", e.stepDetail(s, map[string]any{"manual": s.Manual}))
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case msg := <-e.r.ctl:
			if msg.stepID != s.ID {
				msg.reply <- httpx.Conflict(fmt.Sprintf("当前等待的是步骤 %d", s.ID))
				continue
			}
			switch msg.kind {
			case "run":
				if err := execCheckActionPerm(msg.principal, e.lookup, s.Action); err != nil {
					msg.reply <- err
					continue
				}
				msg.reply <- nil
				ec := *e.ec
				ec.Principal = msg.principal
				e.tl(bg, "step_handover", e.stepDetail(s, map[string]any{"by": msg.actor}))
				return e.execAI(ctx, bg, i, &ec)
			case "manual":
				now := e.now()
				s.FinishedAt = &now
				if s.StartedAt == nil {
					s.StartedAt = &now
				}
				s.DoneBy = msg.actor
				ok := true
				var res error
				switch msg.status {
				case "done":
					s.Status, s.Output = StepManual, execTruncate(msg.note)
				case "skipped":
					s.Status, s.Output = StepSkipped, execTruncate(msg.note)
				default:
					s.Status, s.Error = StepFailed, execTruncate(msg.note)
					ok = false
					res = errors.New("人工标记失败：" + msg.note)
				}
				e.save(bg)
				e.tl(bg, "step_manual", e.stepDetail(s, map[string]any{"by": msg.actor, "note": msg.note}))
				msg.reply <- nil
				return ok, res
			default:
				msg.reply <- httpx.Validation("未知操作")
			}
		}
	}
}

// rollbackDone undoes succeeded main steps in reverse order. Steps without an
// automatic rollback (ErrNoRollback) and human steps are recorded and skipped.
func (e *execEngine) rollbackDone(bg context.Context) int {
	order := execOrder(e.plan.Steps, false)
	failed := 0
	e.tl(bg, "rollback_start", map[string]any{"plan_id": e.plan.ID})
	for k := len(order) - 1; k >= 0; k-- {
		s := &e.plan.Steps[order[k]]
		if s.Status == StepManual {
			e.tl(bg, "rollback_manual_needed", e.stepDetail(s, map[string]any{"manual": s.Manual}))
			continue
		}
		if s.Status != StepSucceeded {
			continue
		}
		a, ok := e.lookup(s.Action)
		if !ok {
			failed++
			e.tl(bg, "rollback_failed", e.stepDetail(s, map[string]any{"error": "动作未注册"}))
			continue
		}
		out, have := e.outputs[s.ID]
		if !have {
			out = s.Output
		}
		rctx, cancel := context.WithTimeout(bg, execRollbackTimout)
		rout, err := a.Rollback(rctx, e.ec, s.Params, out)
		cancel()
		if errors.Is(err, ErrNoRollback) {
			e.tl(bg, "rollback_unavailable", e.stepDetail(s, map[string]any{"error": err.Error()}))
			continue
		}
		if err != nil {
			failed++
			s.Error = execTruncate("回滚失败：" + err.Error())
			e.save(bg)
			e.tl(bg, "rollback_failed", e.stepDetail(s, map[string]any{"error": err.Error()}))
			continue
		}
		s.Status = StepRolledBack
		if rout != "" {
			s.Output = execTruncate(s.Output + "\n[回滚] " + rout)
		}
		e.save(bg)
		e.tl(bg, "rollback_done", e.stepDetail(s, nil))
	}
	e.tl(bg, "rollback_finish", map[string]any{"failed": failed})
	return failed
}

func (e *execEngine) finishAborted(bg context.Context) {
	for i := range e.plan.Steps {
		s := &e.plan.Steps[i]
		switch s.Status {
		case StepPending, StepBlocked, StepRunning:
			s.Status, s.Error = StepSkipped, "执行已中止"
		}
	}
	e.save(bg)
	e.setStatus(bg, CaseCancelled, "执行已中止")
	e.tl(bg, "execute_aborted", map[string]any{"plan_id": e.plan.ID})
}

func execCheckActionPerm(p *rbac.Principal, lookup func(string) (Action, bool), name string) error {
	if p == nil {
		return httpx.Unauthorized("需要登录")
	}
	if !p.Can(PermExecute) {
		return httpx.Forbidden("缺少权限：" + PermExecute)
	}
	a, ok := lookup(name)
	if !ok {
		return httpx.Validation("动作未注册：" + name)
	}
	if perm := a.Spec().Permission; perm != "" && !p.Can(perm) {
		return httpx.Forbidden("缺少动作权限：" + perm)
	}
	return nil
}

// ---------------------------------------------------------------- launch

func (m *Module) execStart(cs *Case, plan *Plan, mode string, p *rbac.Principal) error {
	ctx, cancel := context.WithCancel(context.Background())
	r, ok := execRegister(cs.ID, cancel)
	if !ok {
		cancel()
		return httpx.Conflict("该排查单已有进行中的执行")
	}
	e := newExecEngine(m, m.execContext(p, cs), plan, mode, r)
	go func() {
		defer execUnregister(cs.ID, r)
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				bg := context.Background()
				_ = m.updateCaseStatus(bg, cs.ID, CaseFailed, fmt.Sprintf("执行引擎异常：%v", v))
				_ = m.appendTimeline(bg, cs.ID, "error", "system", map[string]any{"panic": fmt.Sprint(v)})
			}
		}()
		e.execute(ctx)
	}()
	return nil
}

// execStartAuto starts an automatic (runbook) execution in AI mode. The caller
// (runbooks/cases auto policy) has already decided the runbook may auto-run
// (autoExecutable). p is the principal the execution acts as. High-risk plans
// are never auto-executed. Records the run for the hourly cap (rbRecordAutoExec).
func (m *Module) execStartAuto(ctx context.Context, cs *Case, plan *Plan, p *rbac.Principal) error {
	if plan.CaseID != cs.ID || plan.Status != execPlanAccepted {
		return httpx.Conflict("自动处置的方案必须已采纳")
	}
	risk, err := execPrepare(ctx, m.execContext(p, cs), plan, ModeAI, nil, ActionByName)
	if err != nil {
		return err
	}
	if risk == RiskHigh {
		return httpx.Forbidden("高风险方案不允许自动执行")
	}
	if err := m.updatePlan(ctx, plan); err != nil {
		return err
	}
	cs.Mode, cs.PlanID, cs.ReviewedBy = ModeAI, &plan.ID, "auto"
	if err := m.updateCase(ctx, cs); err != nil {
		return err
	}
	if err := m.execStart(cs, plan, ModeAI, p); err != nil {
		return err
	}
	rbRecordAutoExec()
	_ = m.appendTimeline(ctx, cs.ID, "auto_execute", "system", map[string]any{"plan_id": plan.ID, "risk": risk})
	return nil
}

func (m *Module) execWindowCheck(ctx context.Context) error {
	pol, err := m.loadPolicy(ctx)
	if err != nil {
		return httpx.Conflict("无法读取变更窗口策略，高风险执行已拒绝")
	}
	if !inChangeWindow(pol, time.Now()) {
		return httpx.Forbidden("当前不在变更窗口内，高风险方案不能执行")
	}
	return nil
}

// ---------------------------------------------------------------- approval

type execTicket struct {
	CaseID int64  `json:"case_id"`
	PlanID int64  `json:"plan_id"`
	Mode   string `json:"mode"`
}

var execApprovalOnce sync.Once

func (m *Module) execRegisterApproval() {
	if m.d == nil || m.d.Approval == nil {
		return
	}
	execApprovalOnce.Do(func() {
		err := m.d.Approval.Register(approval.TypeSpec{
			Type:             execTicketType,
			Module:           "aiops",
			Description:      "AI 运维高风险方案执行",
			CreatePermission: PermExecute,
			Handler:          execApprovalHandler{m: m},
		})
		if err != nil && m.d.Logger != nil {
			m.d.Logger.Error("aiops: register approval type", "err", err)
		}
	})
}

type execApprovalHandler struct{ m *Module }

func (h execApprovalHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	m := h.m
	var pl execTicket
	if err := json.Unmarshal(t.Payload, &pl); err != nil {
		return nil, fmt.Errorf("aiops: bad ticket payload: %w", err)
	}
	p := rbac.FromContext(ctx)
	cs, err := m.getCase(ctx, pl.CaseID)
	if err != nil {
		return nil, err
	}
	if cs.Status != CaseAwaitApproval {
		return nil, fmt.Errorf("排查单状态为 %s，不再等待审批", cs.Status)
	}
	plan, err := m.getPlan(ctx, pl.PlanID)
	if err != nil {
		return nil, err
	}
	if plan.CaseID != cs.ID || plan.Status != execPlanAccepted {
		return nil, errors.New("方案已不是已采纳状态")
	}
	if _, err := execPrepare(ctx, m.execContext(p, cs), plan, pl.Mode, nil, ActionByName); err != nil {
		_ = m.updateCaseStatus(ctx, cs.ID, CaseAccepted, "审批通过但权限校验失败："+err.Error())
		return nil, err
	}
	if err := m.execWindowCheck(ctx); err != nil {
		_ = m.updateCaseStatus(ctx, cs.ID, CaseAccepted, err.Error())
		return nil, err
	}
	if err := m.updatePlan(ctx, plan); err != nil {
		return nil, err
	}
	_ = m.appendTimeline(ctx, cs.ID, "approval_approved", t.ApproverName, map[string]any{"approval_id": t.ID})
	if err := m.execStart(cs, plan, pl.Mode, p); err != nil {
		return nil, err
	}
	return map[string]any{"case_id": cs.ID, "plan_id": plan.ID, "status": CaseExecuting}, nil
}

func (h execApprovalHandler) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var pl execTicket
	if err := json.Unmarshal(t.Payload, &pl); err != nil {
		return err
	}
	cs, err := h.m.getCase(ctx, pl.CaseID)
	if err != nil {
		return err
	}
	if cs.Status == CaseAwaitApproval {
		_ = h.m.updateCaseStatus(ctx, cs.ID, CaseAccepted, "审批未通过："+t.Comment)
	}
	return h.m.appendTimeline(ctx, cs.ID, "approval_rejected", t.ApproverName, map[string]any{"approval_id": t.ID, "comment": t.Comment, "status": t.Status})
}

// ---------------------------------------------------------------- routes

func (m *Module) registerExecutor(g *gin.RouterGroup) {
	m.execRegisterApproval()
	anyExec := rbac.RequireAnyPerm(PermExecute, PermHandle)
	g.POST("/cases/:id/execute", anyExec, m.handleExecute)
	g.POST("/cases/:id/steps/:sid/run", rbac.RequirePerm(PermExecute), m.handleStepRun)
	g.POST("/cases/:id/steps/:sid/manual", rbac.RequirePerm(PermHandle), m.handleStepManual)
	g.POST("/cases/:id/pause", anyExec, m.handlePause)
	g.POST("/cases/:id/resume", anyExec, m.handleResume)
	g.POST("/cases/:id/abort", anyExec, m.handleAbort)
	g.POST("/cases/:id/rollback", rbac.RequirePerm(PermExecute), m.handleRollback)
}

type execRequest struct {
	PlanID    int64             `json:"plan_id"`
	Mode      string            `json:"mode"`
	Executors map[string]string `json:"executors"`
}

func (m *Module) execLoadCase(c *gin.Context) (*Case, *rbac.Principal, bool) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return nil, nil, false
	}
	p := rbac.PrincipalFrom(c)
	cs, err := m.getCase(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, execNotFound(err))
		return nil, nil, false
	}
	if p == nil || !p.CanEnv(cs.Env) {
		httpx.Fail(c, httpx.Forbidden("环境不在你的权限范围内："+cs.Env))
		return nil, nil, false
	}
	return cs, p, true
}

func (m *Module) handleExecute(c *gin.Context) {
	ctx := c.Request.Context()
	cs, p, ok := m.execLoadCase(c)
	if !ok {
		return
	}
	var req execRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, httpx.Validation("请求格式错误："+err.Error()))
		return
	}
	if req.Mode == "" {
		req.Mode = ModeAI
	}
	if cs.Status != CaseAccepted {
		httpx.Fail(c, httpx.Conflict("排查单当前状态不能执行："+cs.Status))
		return
	}
	plan, err := m.getPlan(ctx, req.PlanID)
	if err != nil || plan.CaseID != cs.ID {
		httpx.Fail(c, httpx.NotFound("方案不存在"))
		return
	}
	if plan.Status != execPlanAccepted {
		httpx.Fail(c, httpx.Conflict("方案尚未采纳，不能执行"))
		return
	}
	risk, err := execPrepare(ctx, m.execContext(p, cs), plan, req.Mode, req.Executors, ActionByName)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.updatePlan(ctx, plan); err != nil {
		httpx.Fail(c, err)
		return
	}
	cs.Mode, cs.PlanID, cs.ReviewedBy = req.Mode, &plan.ID, execActor(p)

	if risk == RiskHigh {
		if err := m.execWindowCheck(ctx); err != nil {
			httpx.Fail(c, err)
			return
		}
		if m.d == nil || m.d.Approval == nil {
			httpx.Fail(c, httpx.Conflict("审批服务不可用，高风险执行已拒绝"))
			return
		}
		t, pending, err := m.d.Approval.Gate(ctx, p, approval.CreateInput{
			Type:    execTicketType,
			Title:   execTruncateTitle(fmt.Sprintf("AI 运维执行 #%d：%s", cs.ID, plan.Title)),
			Env:     cs.Env,
			Reason:  execTruncateTitle(cs.Title),
			Payload: execTicket{CaseID: cs.ID, PlanID: plan.ID, Mode: req.Mode},
		})
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if pending {
			cs.Status, cs.ApprovalID = CaseAwaitApproval, &t.ID
			if err := m.updateCase(ctx, cs); err != nil {
				httpx.Fail(c, err)
				return
			}
			_ = m.updateCaseStatus(ctx, cs.ID, CaseAwaitApproval, "")
			_ = m.appendTimeline(ctx, cs.ID, "approval_requested", execActor(p), map[string]any{"approval_id": t.ID, "risk": risk, "plan_id": plan.ID, "mode": req.Mode})
			approval.RespondPending(c, t)
			return
		}
	}
	if err := m.updateCase(ctx, cs); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.execStart(cs, plan, req.Mode, p); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"case_id": cs.ID, "plan_id": plan.ID, "mode": req.Mode, "risk": risk, "status": CaseExecuting})
}

func execTruncateTitle(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	return string([]rune(s)[:200]) + "…"
}

func (m *Module) execActiveRun(c *gin.Context) (*Case, *rbac.Principal, *execRun, bool) {
	cs, p, ok := m.execLoadCase(c)
	if !ok {
		return nil, nil, nil, false
	}
	r := execLookupRun(cs.ID)
	if r == nil {
		httpx.Fail(c, httpx.Conflict("该排查单没有进行中的执行"))
		return nil, nil, nil, false
	}
	return cs, p, r, true
}

func (m *Module) handleStepRun(c *gin.Context) {
	cs, p, r, ok := m.execActiveRun(c)
	if !ok {
		return
	}
	sid, ok := httpx.ParamID(c, "sid")
	if !ok {
		return
	}
	if err := execSend(r, execCtl{stepID: sid, kind: "run", actor: execActor(p), principal: p}); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"case_id": cs.ID, "step_id": sid, "status": StepRunning})
}

type execManualRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

func (m *Module) handleStepManual(c *gin.Context) {
	cs, p, r, ok := m.execActiveRun(c)
	if !ok {
		return
	}
	sid, ok := httpx.ParamID(c, "sid")
	if !ok {
		return
	}
	var req execManualRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, httpx.Validation("请求格式错误："+err.Error()))
		return
	}
	if req.Status != "done" && req.Status != "skipped" && req.Status != "failed" {
		httpx.Fail(c, httpx.Validation("status 必须为 done、skipped 或 failed"))
		return
	}
	if utf8.RuneCountInString(req.Note) > 4000 {
		httpx.Fail(c, httpx.Validation("备注过长"))
		return
	}
	if err := execSend(r, execCtl{stepID: sid, kind: "manual", status: req.Status, note: req.Note, actor: execActor(p), principal: p}); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"case_id": cs.ID, "step_id": sid, "status": req.Status})
}

func (m *Module) handlePause(c *gin.Context) {
	cs, p, r, ok := m.execActiveRun(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if r.pause(func(st string) { _ = m.updateCaseStatus(ctx, cs.ID, st, "") }) {
		_ = m.appendTimeline(ctx, cs.ID, "execute_paused", execActor(p), map[string]any{})
	}
	httpx.OK(c, gin.H{"case_id": cs.ID, "paused": true, "status": CasePaused})
}

func (m *Module) handleResume(c *gin.Context) {
	cs, p, r, ok := m.execActiveRun(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	status := ""
	if r.resumeRun(func(st string) { status = st; _ = m.updateCaseStatus(ctx, cs.ID, st, "") }) {
		_ = m.appendTimeline(ctx, cs.ID, "execute_resumed", execActor(p), map[string]any{})
	}
	if status == "" {
		status = CaseExecuting
	}
	httpx.OK(c, gin.H{"case_id": cs.ID, "paused": false, "status": status})
}

func (m *Module) handleAbort(c *gin.Context) {
	cs, p, ok := m.execLoadCase(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if r := execLookupRun(cs.ID); r != nil {
		_ = m.appendTimeline(ctx, cs.ID, "execute_abort_requested", execActor(p), map[string]any{})
		r.abort()
		httpx.OK(c, gin.H{"case_id": cs.ID, "status": CaseCancelled})
		return
	}
	if cs.Status == CaseAwaitApproval {
		_ = m.updateCaseStatus(ctx, cs.ID, CaseAccepted, "执行申请已撤回")
		_ = m.appendTimeline(ctx, cs.ID, "execute_withdrawn", execActor(p), map[string]any{"approval_id": cs.ApprovalID})
		httpx.OK(c, gin.H{"case_id": cs.ID, "status": CaseAccepted})
		return
	}
	httpx.Fail(c, httpx.Conflict("该排查单没有进行中的执行"))
}

func (m *Module) handleRollback(c *gin.Context) {
	cs, p, ok := m.execLoadCase(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	switch cs.Status {
	case CaseFailed, CaseCancelled, CaseResolved:
	default:
		httpx.Fail(c, httpx.Conflict("当前状态不能回滚："+cs.Status))
		return
	}
	if cs.PlanID == nil {
		httpx.Fail(c, httpx.Conflict("该排查单没有已执行的方案"))
		return
	}
	plan, err := m.getPlan(ctx, *cs.PlanID)
	if err != nil {
		httpx.Fail(c, execNotFound(err))
		return
	}
	for _, s := range plan.Steps {
		if s.Status == StepSucceeded {
			if err := execCheckActionPerm(p, ActionByName, s.Action); err != nil {
				httpx.Fail(c, err)
				return
			}
		}
	}
	rctx, cancel := context.WithCancel(context.Background())
	r, ok := execRegister(cs.ID, cancel)
	if !ok {
		cancel()
		httpx.Fail(c, httpx.Conflict("该排查单已有进行中的执行"))
		return
	}
	e := newExecEngine(m, m.execContext(p, cs), plan, cs.Mode, r)
	go func() {
		defer execUnregister(cs.ID, r)
		defer cancel()
		bg := context.WithoutCancel(rctx)
		failed := e.rollbackDone(bg)
		msg := "已人工触发回滚"
		if failed > 0 {
			msg = fmt.Sprintf("已人工触发回滚，%d 个步骤回滚失败", failed)
		}
		e.setStatus(bg, CaseFailed, msg)
	}()
	httpx.OK(c, gin.H{"case_id": cs.ID, "status": "rolling_back"})
}

// ---------------------------------------------------------------- recovery

// recoverExecutions marks cases left executing/verifying by a previous process
// as failed (reason: service restart). Call once from Start.
func (m *Module) recoverExecutions(ctx context.Context) {
	for _, st := range []string{CaseExecuting, CaseVerifying, CasePaused} {
		cases, err := m.execListAll(ctx, st)
		if err != nil && m.d != nil && m.d.Logger != nil {
			m.d.Logger.Error("aiops: recover executions", "status", st, "err", err)
		}
		for i := range cases {
			cs := &cases[i]
			if execLookupRun(cs.ID) != nil {
				continue
			}
			if cs.PlanID != nil {
				if plan, err := m.getPlan(ctx, *cs.PlanID); err == nil {
					execMarkInterrupted(plan)
					_ = m.updatePlan(ctx, plan)
				}
			}
			_ = m.updateCaseStatus(ctx, cs.ID, CaseFailed, "服务重启，执行中断")
			_ = m.appendTimeline(ctx, cs.ID, "execute_interrupted", "system", map[string]any{"reason": "服务重启"})
		}
	}
}

func execMarkInterrupted(plan *Plan) {
	for i := range plan.Steps {
		s := &plan.Steps[i]
		switch s.Status {
		case StepRunning:
			s.Status, s.Error = StepFailed, "服务重启，执行中断"
		case StepBlocked:
			s.Status, s.Error = StepSkipped, "服务重启，执行中断"
		}
	}
}

// execListAll pages through every case with the given status.
func (m *Module) execListAll(ctx context.Context, status string) ([]Case, error) {
	var all []Case
	for page := 1; page <= 100; page++ {
		items, total, err := m.listCases(ctx, caseFilter{Status: status, Page: page, Size: 100})
		if err != nil {
			return all, err
		}
		all = append(all, items...)
		if len(items) == 0 || len(all) >= total {
			break
		}
	}
	return all, nil
}

func execNotFound(err error) error {
	err = notFound(err)
	if errors.Is(err, errStoreNotFound) {
		return httpx.NotFound("记录不存在")
	}
	return err
}

// execVerifier is optionally implemented by actions whose effect completes
// asynchronously (e.g. component.install); it runs right after Execute.
type execVerifier interface {
	Verify(ctx context.Context, ec *ExecContext, params json.RawMessage, out string) (string, error)
}

// execStateSuffix returns the internal rollback-state line of an output so it
// survives truncation (the display strips it).
func execStateSuffix(out string) string {
	if i := strings.LastIndex(out, k8sStateMarker); i >= 0 {
		return out[i:]
	}
	return ""
}
