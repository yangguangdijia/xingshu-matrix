package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ---- fakes (not registered globally) ----

type execFakeAction struct {
	name     string
	risk     string
	perm     string
	fail     bool
	noRB     bool
	validErr error
	log      *execCallLog
}

type execCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *execCallLog) add(s string) { l.mu.Lock(); l.calls = append(l.calls, s); l.mu.Unlock() }
func (l *execCallLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (a *execFakeAction) Name() string { return a.name }
func (a *execFakeAction) Spec() ActionSpec {
	return ActionSpec{Name: a.name, Title: a.name, Risk: a.risk, Permission: a.perm}
}
func (a *execFakeAction) Validate(_ context.Context, _ *ExecContext, p json.RawMessage) (json.RawMessage, string, error) {
	return p, a.risk, a.validErr
}
func (a *execFakeAction) Describe(context.Context, *ExecContext, json.RawMessage) (string, string, string, error) {
	return a.name, "", "", nil
}
func (a *execFakeAction) Execute(context.Context, *ExecContext, json.RawMessage) (string, error) {
	a.log.add("exec:" + a.name)
	if a.fail {
		return "boom output", errors.New("boom")
	}
	return "ok " + a.name, nil
}
func (a *execFakeAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	a.log.add("rollback:" + a.name)
	if a.noRB {
		return "", ErrNoRollback
	}
	return "undone", nil
}

type execFakeStore struct {
	mu       sync.Mutex
	cs       *Case
	plan     *Plan
	timeline []string
	statuses []string
}

func (s *execFakeStore) getCase(context.Context, int64) (*Case, error) { return s.cs, nil }
func (s *execFakeStore) getPlan(context.Context, int64) (*Plan, error) { return s.plan, nil }
func (s *execFakeStore) updatePlan(context.Context, *Plan) error       { return nil }
func (s *execFakeStore) updateCase(context.Context, *Case) error       { return nil }
func (s *execFakeStore) updateCaseStatus(_ context.Context, _ int64, st, _ string) error {
	s.mu.Lock()
	s.statuses = append(s.statuses, st)
	s.mu.Unlock()
	return nil
}
func (s *execFakeStore) appendTimeline(_ context.Context, _ int64, kind, _ string, _ any) error {
	s.mu.Lock()
	s.timeline = append(s.timeline, kind)
	s.mu.Unlock()
	return nil
}
func (s *execFakeStore) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[len(s.statuses)-1]
}

func execFixture(env string, acts ...*execFakeAction) (func(string) (Action, bool), *execCallLog) {
	log := &execCallLog{}
	m := map[string]Action{}
	for _, a := range acts {
		a.log = log
		m[a.name] = a
	}
	return func(n string) (Action, bool) { a, ok := m[n]; return a, ok }, log
}

func execPlan(steps ...Step) *Plan {
	for i := range steps {
		steps[i].ID = int64(i + 1)
		steps[i].Seq = i + 1
		if steps[i].Phase == "" {
			steps[i].Phase = "main"
		}
		steps[i].Status = StepPending
		steps[i].Params = json.RawMessage(`{}`)
	}
	return &Plan{ID: 10, CaseID: 1, Status: execPlanAccepted, Steps: steps}
}

func execPrincipal(perms ...string) *rbac.Principal {
	return &rbac.Principal{UserID: 7, Username: "ops", Permissions: perms}
}

func execEngineFor(st *execFakeStore, lookup func(string) (Action, bool), mode string, p *rbac.Principal) *execEngine {
	ec := &ExecContext{Principal: p, Case: st.cs, Env: st.cs.Env}
	e := newExecEngine(st, ec, st.plan, mode, execNewRun(st.cs.ID, nil))
	e.lookup = lookup
	return e
}

// ---- tests ----

func TestExecSequentialResolves(t *testing.T) {
	lookup, log := execFixture("test",
		&execFakeAction{name: "a1", risk: RiskChange}, &execFakeAction{name: "a2", risk: RiskChange},
		&execFakeAction{name: "a3", risk: RiskRead}, &execFakeAction{name: "v1", risk: RiskRead})
	// verify step listed first to prove phase ordering.
	st := &execFakeStore{cs: &Case{ID: 1, Env: "test"}, plan: execPlan(
		Step{Action: "v1", Phase: "verify"}, Step{Action: "a1"}, Step{Action: "a2"}, Step{Action: "a3"})}
	e := execEngineFor(st, lookup, ModeAI, execPrincipal("*"))
	e.execute(context.Background())

	want := "exec:a1,exec:a2,exec:a3,exec:v1"
	if got := strings.Join(log.get(), ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	if st.last() != CaseResolved {
		t.Fatalf("case status = %s", st.last())
	}
	for _, s := range st.plan.Steps {
		if s.Status != StepSucceeded || s.StartedAt == nil || s.FinishedAt == nil {
			t.Fatalf("step %s status=%s", s.Action, s.Status)
		}
	}
}

func TestExecFailureRollsBackInReverse(t *testing.T) {
	lookup, log := execFixture("test",
		&execFakeAction{name: "a1", risk: RiskChange}, &execFakeAction{name: "a2", risk: RiskChange, noRB: true},
		&execFakeAction{name: "a3", risk: RiskChange, fail: true}, &execFakeAction{name: "a4", risk: RiskChange},
		&execFakeAction{name: "v1", risk: RiskRead})
	st := &execFakeStore{cs: &Case{ID: 1, Env: "test"}, plan: execPlan(
		Step{Action: "a1"}, Step{Action: "a2"}, Step{Action: "a3"}, Step{Action: "a4"}, Step{Action: "v1", Phase: "verify"})}
	e := execEngineFor(st, lookup, ModeAI, execPrincipal("*"))
	e.execute(context.Background())

	want := "exec:a1,exec:a2,exec:a3,rollback:a2,rollback:a1"
	if got := strings.Join(log.get(), ","); got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	got := map[string]string{}
	for _, s := range st.plan.Steps {
		got[s.Action] = s.Status
	}
	if got["a1"] != StepRolledBack || got["a2"] != StepSucceeded || got["a3"] != StepFailed || got["a4"] != StepPending || got["v1"] != StepPending {
		t.Fatalf("statuses = %v", got)
	}
	if st.last() != CaseFailed {
		t.Fatalf("case status = %s", st.last())
	}
	if !strings.Contains(strings.Join(st.timeline, ","), "rollback_unavailable") {
		t.Fatalf("ErrNoRollback not recorded: %v", st.timeline)
	}
}

func TestExecVerifyFailureFailsCase(t *testing.T) {
	lookup, _ := execFixture("test", &execFakeAction{name: "a1", risk: RiskChange}, &execFakeAction{name: "v1", risk: RiskRead, fail: true})
	st := &execFakeStore{cs: &Case{ID: 1, Env: "test"}, plan: execPlan(Step{Action: "a1"}, Step{Action: "v1", Phase: "verify"})}
	execEngineFor(st, lookup, ModeAI, execPrincipal("*")).execute(context.Background())
	if st.last() != CaseFailed {
		t.Fatalf("case status = %s", st.last())
	}
}

func TestExecRiskElevation(t *testing.T) {
	cases := []struct {
		env, stepRisk, want string
	}{
		{"prod", RiskChange, RiskHigh},
		{"production", RiskChange, RiskHigh},
		{"test", RiskChange, RiskChange},
		{"prod", RiskRead, RiskRead},
		{"test", RiskHigh, RiskHigh},
	}
	for _, tc := range cases {
		lookup, _ := execFixture(tc.env, &execFakeAction{name: "x", risk: tc.stepRisk, perm: "workload:write"})
		plan := execPlan(Step{Action: "x"})
		ec := &ExecContext{Principal: execPrincipal("*"), Case: &Case{ID: 1, Env: tc.env}, Env: tc.env}
		got, err := execPrepare(context.Background(), ec, plan, ModeAI, nil, lookup)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.env, tc.stepRisk, err)
		}
		if got != tc.want || plan.Risk != tc.want || plan.Steps[0].Risk != tc.want {
			t.Fatalf("%s/%s: risk=%s plan=%s step=%s, want %s", tc.env, tc.stepRisk, got, plan.Risk, plan.Steps[0].Risk, tc.want)
		}
	}
}

func TestExecPermissionDenied(t *testing.T) {
	lookup, _ := execFixture("test", &execFakeAction{name: "k", risk: RiskChange, perm: "workload:write"})
	ec := func(p *rbac.Principal) *ExecContext {
		return &ExecContext{Principal: p, Case: &Case{ID: 1, Env: "test"}, Env: "test"}
	}
	// lacks the action permission
	if _, err := execPrepare(context.Background(), ec(execPrincipal(PermExecute, PermHandle)), execPlan(Step{Action: "k"}), ModeAI, nil, lookup); err == nil {
		t.Fatal("expected forbidden without workload:write")
	}
	// lacks aiops:execute for an AI-run plan
	if _, err := execPrepare(context.Background(), ec(execPrincipal(PermHandle, "workload:write")), execPlan(Step{Action: "k"}), ModeAI, nil, lookup); err == nil {
		t.Fatal("expected forbidden without aiops:execute")
	}
	// manual checklist only needs handle + action permission
	if _, err := execPrepare(context.Background(), ec(execPrincipal(PermHandle, "workload:write")), execPlan(Step{Action: "k"}), ModeManual, nil, lookup); err != nil {
		t.Fatalf("manual mode: %v", err)
	}
	// unknown action / bad mode / bad executor key
	if _, err := execPrepare(context.Background(), ec(execPrincipal("*")), execPlan(Step{Action: "nope"}), ModeAI, nil, lookup); err == nil {
		t.Fatal("expected error for unregistered action")
	}
	if _, err := execPrepare(context.Background(), ec(execPrincipal("*")), execPlan(Step{Action: "k"}), "auto", nil, lookup); err == nil {
		t.Fatal("expected error for bad mode")
	}
	if _, err := execPrepare(context.Background(), ec(execPrincipal("*")), execPlan(Step{Action: "k"}), ModeMixed, map[string]string{"99": "ai"}, lookup); err == nil {
		t.Fatal("expected error for unknown executor step")
	}
}

func TestExecMixedHumanStepAndAbort(t *testing.T) {
	lookup, log := execFixture("test", &execFakeAction{name: "a1", risk: RiskChange}, &execFakeAction{name: "h1", risk: RiskChange},
		&execFakeAction{name: "h2", risk: RiskChange})
	st := &execFakeStore{cs: &Case{ID: 1, Env: "test"}, plan: execPlan(Step{Action: "a1"}, Step{Action: "h1"}, Step{Action: "h2"})}
	ec := &ExecContext{Principal: execPrincipal("*"), Case: st.cs, Env: "test"}
	if _, err := execPrepare(context.Background(), ec, st.plan, ModeMixed, map[string]string{"2": "human", "3": "human"}, lookup); err != nil {
		t.Fatal(err)
	}
	e := execEngineFor(st, lookup, ModeMixed, execPrincipal("*"))
	ctx, cancel := context.WithCancel(context.Background())
	e.r.cancel = cancel
	done := make(chan struct{})
	go func() { e.execute(ctx); close(done) }()

	// wrong step is refused while step 2 waits
	deadline := time.Now().Add(3 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = execSend(e.r, execCtl{stepID: 2, kind: "manual", status: "done", note: "已手工处理", actor: "ops"}); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("manual done: %v", err)
	}
	// step 3 now waits; abort
	for time.Now().Before(deadline) {
		if st.plan.Steps[2].Status == StepBlocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.r.abort()
	<-done
	if got := strings.Join(log.get(), ","); got != "exec:a1" {
		t.Fatalf("calls = %s", got)
	}
	if st.plan.Steps[1].Status != StepManual || st.plan.Steps[2].Status != StepSkipped {
		t.Fatalf("statuses = %s,%s", st.plan.Steps[1].Status, st.plan.Steps[2].Status)
	}
	if st.last() != CaseCancelled {
		t.Fatalf("case status = %s", st.last())
	}
}

func TestExecTruncateAndInterrupted(t *testing.T) {
	s := strings.Repeat("中", 5000) // 15000 bytes
	out := execTruncate(s)
	if !strings.HasSuffix(out, "（输出已截断）") || len(out) > execMaxOutput+64 {
		t.Fatalf("truncate len=%d", len(out))
	}
	p := &Plan{Steps: []Step{{Status: StepRunning}, {Status: StepBlocked}, {Status: StepSucceeded}}}
	execMarkInterrupted(p)
	if p.Steps[0].Status != StepFailed || p.Steps[1].Status != StepSkipped || p.Steps[2].Status != StepSucceeded {
		t.Fatalf("interrupted = %+v", p.Steps)
	}
}

func TestExecPauseResumeStatus(t *testing.T) {
	r := execNewRun(1, nil)
	var got []string
	set := func(s string) { got = append(got, s) }
	if !r.pause(set) || r.pause(set) {
		t.Fatal("pause should succeed once")
	}
	r.setPhase(CaseVerifying, set) // engine moves on while paused: no write
	if !r.resumeRun(set) {
		t.Fatal("resume failed")
	}
	if strings.Join(got, ",") != CasePaused+","+CaseVerifying {
		t.Fatalf("writes = %v", got)
	}
	r2 := execNewRun(2, nil)
	got = nil
	r2.pause(set)
	r2.resumeRun(set)
	if strings.Join(got, ",") != CasePaused+","+CaseExecuting {
		t.Fatalf("writes = %v", got)
	}
}
