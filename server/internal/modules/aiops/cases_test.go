package aiops

import (
	"encoding/json"
	"strings"
	"testing"
)

func caseTestAction(t *testing.T) string {
	t.Helper()
	specs := ActionSpecs()
	if len(specs) == 0 {
		t.Skip("no registered actions")
	}
	return specs[0].Name
}

func TestCaseNormalizeInput(t *testing.T) {
	in := CreateCaseInput{Request: "  deploy nginx to 10.0.0.5\nmore details  "}
	if err := caseNormalizeInput(&in); err != nil {
		t.Fatal(err)
	}
	if in.Kind != KindRequest || in.Source != SourceManual || string(in.Target) != "{}" {
		t.Fatalf("defaults not applied: %+v", in)
	}
	if in.Title != "deploy nginx to 10.0.0.5" {
		t.Fatalf("title = %q", in.Title)
	}
	neg := int64(-1)
	bad := []CreateCaseInput{
		{Request: ""},
		{Request: "   "},
		{Request: "x", Kind: "bogus"},
		{Request: "x", Source: "bogus"},
		{Request: strings.Repeat("a", caseMaxRequestLen+1)},
		{Request: "x", Target: []byte(`[1,2]`)},
		{Request: "x", Target: []byte(`not json`)},
		{Request: "x", ClusterID: &neg},
		{Request: "x", AlertID: &neg},
		{Request: "x", AssetID: &neg},
		{Request: "x", Env: strings.Repeat("e", 65)},
	}
	for i, b := range bad {
		if err := caseNormalizeInput(&b); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	long := CreateCaseInput{Request: "r", Title: strings.Repeat("标", 200)}
	if err := caseNormalizeInput(&long); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(long.Title)); n != caseMaxTitleRunes+1 {
		t.Fatalf("title runes = %d", n)
	}
}

func TestCaseValidateReview(t *testing.T) {
	ok := []caseReviewBody{{Decision: "accept"}, {Decision: " reject ", Note: "不对"}}
	for _, b := range ok {
		if err := caseValidateReview(&b); err != nil {
			t.Errorf("unexpected: %v", err)
		}
	}
	bad := []caseReviewBody{
		{Decision: ""},
		{Decision: "approve"},
		{Decision: "reject", Steps: []caseStepEdit{{ID: 1}}},
		{Decision: "accept", Note: strings.Repeat("n", caseMaxNoteLen+1)},
	}
	for i, b := range bad {
		if err := caseValidateReview(&b); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestCaseApplyStepEdits(t *testing.T) {
	act := caseTestAction(t)
	mk := func() []Step {
		return []Step{
			{ID: 11, Action: act, Params: json.RawMessage(`{"a":1}`), Executor: ModeAI},
			{ID: 12, Action: act, Params: json.RawMessage(`{"b":2}`), Executor: ModeAI},
			{ID: 13, Action: "no.such.action", Params: json.RawMessage(`{}`), Executor: ModeAI},
		}
	}
	steps := mk()
	changed, err := caseApplyStepEdits(steps, []caseStepEdit{
		{ID: 11, Params: json.RawMessage(`{"a":5}`)},
		{ID: 12, Executor: "human"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != 0 {
		t.Fatalf("changed = %v", changed)
	}
	if string(steps[0].Params) != `{"a":5}` || steps[1].Executor != "human" || string(steps[1].Params) != `{"b":2}` {
		t.Fatalf("edits not applied: %+v", steps)
	}
	if steps[0].Action != act {
		t.Fatal("action must not change")
	}

	bad := [][]caseStepEdit{
		{{ID: 99, Executor: "human"}},                           // not in plan
		{{ID: 0, Executor: "human"}},                            // zero id
		{{ID: 11, Executor: "robot"}},                           // bad executor
		{{ID: 11, Params: json.RawMessage(`[1]`)}},              // params not object
		{{ID: 11, Params: json.RawMessage(`oops`)}},             // invalid JSON
		{{ID: 11, Action: "other.action"}},                      // action change
		{{ID: 13, Executor: "human"}},                           // unregistered action
		{{ID: 11, Executor: "human"}, {ID: 11, Executor: "ai"}}, // duplicate
	}
	for i, e := range bad {
		if _, err := caseApplyStepEdits(mk(), e); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	// same action name is allowed (no-op).
	if _, err := caseApplyStepEdits(mk(), []caseStepEdit{{ID: 11, Action: act}}); err != nil {
		t.Errorf("same action rejected: %v", err)
	}
}

func TestCaseBuildPlan(t *testing.T) {
	act := caseTestAction(t)
	p, err := caseBuildPlan(7, 1, PlanDraft{
		Steps:  []StepDraft{{Action: act, Params: json.RawMessage(`{"x":1}`)}},
		Verify: []StepDraft{{Action: act}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.CaseID != 7 || p.Status != "proposed" || p.Title == "" || len(p.Steps) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if p.Steps[0].Phase != "main" || p.Steps[1].Phase != "verify" || string(p.Steps[1].Params) != "{}" || p.Steps[1].Seq != 2 {
		t.Fatalf("steps = %+v", p.Steps)
	}
	if _, err := caseBuildPlan(7, 1, PlanDraft{Steps: []StepDraft{{Action: "no.such"}}}); err == nil {
		t.Fatal("unregistered action must be rejected")
	}
	if _, err := caseBuildPlan(7, 1, PlanDraft{}); err == nil {
		t.Fatal("empty plan must be rejected")
	}
}

func TestCaseInputFromAlert(t *testing.T) {
	a := &caseAlert{ID: 5, Alertname: "KubePodCrashLooping", Severity: "critical", Status: "firing",
		Labels:      map[string]string{"cluster_id": "3", "env": "prod", "namespace": "web", "pod": "web-1"},
		Annotations: map[string]string{"summary": "pod restarting"}}
	in := caseInputFromAlert(a)
	if in.Kind != KindAlert || in.Source != SourceAlert || *in.AlertID != 5 || in.Env != "prod" || in.ClusterID == nil || *in.ClusterID != 3 {
		t.Fatalf("in = %+v", in)
	}
	var tgt map[string]string
	_ = json.Unmarshal(in.Target, &tgt)
	if tgt["namespace"] != "web" || tgt["kind"] != "pod" || tgt["name"] != "web-1" {
		t.Fatalf("target = %v", tgt)
	}
	if !strings.Contains(in.Request, "pod restarting") || !strings.Contains(in.Title, "KubePodCrashLooping") {
		t.Fatalf("request/title = %q / %q", in.Request, in.Title)
	}
	if err := caseNormalizeInput(&in); err != nil {
		t.Fatal(err)
	}
	b := caseInputFromAlert(&caseAlert{ID: 6, Alertname: "X", Labels: map[string]string{"cluster_id": "abc"}})
	if b.ClusterID != nil || b.Env != "" {
		t.Fatalf("bad labels should be ignored: %+v", b)
	}
}

func TestCaseStatusGuards(t *testing.T) {
	for _, s := range []string{CaseExecuting, CaseVerifying, CaseAwaitApproval, CaseInvestigating} {
		if caseCanReinvestigate(s) {
			t.Errorf("reinvestigate allowed in %s", s)
		}
	}
	if !caseCanReinvestigate(CaseConcluded) || !caseCanReinvestigate(CaseFailed) {
		t.Error("reinvestigate should be allowed")
	}
	for _, s := range []string{CaseExecuting, CaseVerifying, CaseAwaitApproval, CaseResolved} {
		if caseCanClose(s) {
			t.Errorf("close allowed in %s", s)
		}
	}
	if !caseCanClose(CaseConcluded) {
		t.Error("close should be allowed")
	}
	if caseHasProposed([]Plan{{Status: "rejected"}}) || !caseHasProposed([]Plan{{Status: "proposed"}}) {
		t.Error("caseHasProposed")
	}
	if caseRollbackSummary([]Step{{Phase: "main", Risk: RiskRead}}) != "仅只读操作，无需回滚" {
		t.Error("rollback summary")
	}
}

func TestCaseQueueBounded(t *testing.T) {
	// drain anything left by other tests, then fill.
	for len(caseQueue) > 0 {
		<-caseQueue
	}
	for i := 0; i < caseQueueSize; i++ {
		if !caseEnqueue(caseJob{ID: int64(i + 1)}) {
			t.Fatalf("enqueue %d failed", i)
		}
	}
	if caseEnqueue(caseJob{ID: 999}) {
		t.Fatal("queue should be full")
	}
	for len(caseQueue) > 0 {
		<-caseQueue
	}
}
