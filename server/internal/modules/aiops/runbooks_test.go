package aiops

import (
	"encoding/json"
	"testing"
	"time"
)

func rbTestFirstAction(t *testing.T, risk string) string {
	t.Helper()
	for _, s := range ActionSpecs() {
		if s.Risk == risk {
			return s.Name
		}
	}
	return ""
}

func TestRunbookMatch(t *testing.T) {
	c := &Case{Kind: KindAlert, Env: "test",
		Target: json.RawMessage(`{"kind":"deployment","namespace":"shop","name":"web","alertname":"KubePodCrashLooping"}`)}
	fs := []Finding{{Analyzer: "crashloop"}}
	cases := []struct {
		name  string
		match string
		want  bool
	}{
		{"empty never matches", `{}`, false},
		{"null never matches", `null`, false},
		{"alertname", `{"alertname":"KubePodCrashLooping"}`, true},
		{"alertname case-insensitive", `{"alertname":"kubepodcrashlooping"}`, true},
		{"alertname mismatch", `{"alertname":"NodeDown"}`, false},
		{"reason from finding", `{"reason":"crashloop"}`, true},
		{"analyzer alias", `{"analyzer":"crashloop"}`, true},
		{"reason list", `{"reason":["oomkilled","crashloop"]}`, true},
		{"reason mismatch", `{"reason":"imagepull"}`, false},
		{"all keys", `{"alertname":"KubePodCrashLooping","reason":"crashloop","namespace":"shop","kind":"deployment"}`, true},
		{"namespace mismatch", `{"reason":"crashloop","namespace":"prod-shop"}`, false},
		{"kind mismatch", `{"kind":"statefulset"}`, false},
		{"env", `{"env":"test","reason":"crashloop"}`, true},
		{"wildcard namespace", `{"namespace":"*"}`, true},
		{"unknown key", `{"foo":"bar"}`, false},
		{"bad json", `[1]`, false},
	}
	f := rbCaseFacts(c, fs)
	for _, tc := range cases {
		if got := rbMatches(json.RawMessage(tc.match), f); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	// kind falls back to Case.Kind when the target has none
	c2 := &Case{Kind: KindDeploy, Target: json.RawMessage(`{}`)}
	if !rbMatches(json.RawMessage(`{"kind":"deploy"}`), rbCaseFacts(c2, nil)) {
		t.Error("kind fallback to case kind")
	}
}

func TestRunbookFilterOrder(t *testing.T) {
	c := &Case{Target: json.RawMessage(`{"namespace":"shop"}`)}
	fs := []Finding{{Analyzer: "oomkilled"}}
	list := []Runbook{
		{ID: 1, Match: json.RawMessage(`{"reason":"oomkilled"}`), Successes: 1},
		{ID: 2, Match: json.RawMessage(`{"reason":"oomkilled","namespace":"shop"}`)},
		{ID: 3, Match: json.RawMessage(`{"reason":"crashloop"}`)},
		{ID: 4, Match: json.RawMessage(`{"reason":"oomkilled"}`), Successes: 5},
	}
	got := rbFilterMatches(list, c, fs)
	ids := []int64{}
	for _, rb := range got {
		ids = append(ids, rb.ID)
	}
	want := []int64{2, 4, 1}
	if len(ids) != len(want) {
		t.Fatalf("got %v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v want %v", ids, want)
		}
	}
}

func TestRunbookDraftFromPlan(t *testing.T) {
	p := &Plan{Title: "t", Steps: []Step{
		{Action: "a", Phase: "main", Status: StepSucceeded},
		{Action: "b", Phase: "main", Status: StepSkipped},
		{Action: "c", Phase: "main", Status: StepManual},
		{Action: "d", Phase: "verify", Status: StepSucceeded},
		{Action: "e", Phase: "rollback", Status: StepSucceeded},
		{Action: "f", Phase: "main", Status: StepFailed},
	}}
	d, ran := rbDraftFromPlan(p)
	if ran != 2 || len(d.Steps) != 2 || d.Steps[0].Action != "a" || d.Steps[1].Action != "c" {
		t.Fatalf("steps: ran=%d %+v", ran, d.Steps)
	}
	if len(d.Verify) != 1 || d.Verify[0].Action != "d" {
		t.Fatalf("verify: %+v", d.Verify)
	}
}

func TestRunbookDeriveMatch(t *testing.T) {
	c := &Case{Kind: KindAlert, Target: json.RawMessage(`{"kind":"Deployment","namespace":"shop"}`),
		Conclusion: json.RawMessage(`{"findings":[{"analyzer":"crashloop"},{"analyzer":"crashloop"},{"analyzer":"oomkilled"}]}`)}
	var m map[string]any
	if err := json.Unmarshal(rbDeriveMatch(c), &m); err != nil {
		t.Fatal(err)
	}
	if m["namespace"] != "shop" || m["kind"] != "deployment" {
		t.Fatalf("%v", m)
	}
	if r, _ := m["reason"].([]any); len(r) != 2 {
		t.Fatalf("reason %v", m["reason"])
	}
}

func TestRunbookParseHours(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		a, b int
	}{{"09-21", true, 9, 21}, {"22-06", true, 22, 6}, {"0-24", true, 0, 24}, {"9-9", false, 0, 0}, {"25-3", false, 0, 0}, {"x", false, 0, 0}}
	for _, tc := range cases {
		a, b, ok := rbParseHours(tc.in)
		if ok != tc.ok || (ok && (a != tc.a || b != tc.b)) {
			t.Errorf("%q: got %d %d %v", tc.in, a, b, ok)
		}
	}
}

func TestRunbookNormalize(t *testing.T) {
	read := rbTestFirstAction(t, RiskRead)
	high := rbTestFirstAction(t, RiskHigh)
	if read == "" {
		t.Skip("no read action registered")
	}
	step := func(a string) PlanDraft {
		return PlanDraft{Steps: []StepDraft{{Action: a, Params: json.RawMessage(`{}`)}}}
	}
	ok := Runbook{Name: " 重启 ", Match: json.RawMessage(`{"reason":"crashloop"}`), Plan: step(read)}
	if err := rbNormalize(&ok); err != nil || ok.Name != "重启" || ok.Risk != RiskRead || string(ok.AutoScope) != "{}" {
		t.Fatalf("ok: %v %+v", err, ok)
	}
	bad := []Runbook{
		{Name: "", Match: json.RawMessage(`{"reason":"x"}`), Plan: step(read)},
		{Name: "n", Match: json.RawMessage(`{}`), Plan: step(read)},
		{Name: "n", Match: json.RawMessage(`{"foo":"x"}`), Plan: step(read)},
		{Name: "n", Match: json.RawMessage(`{"reason":"x"}`)},
		{Name: "n", Match: json.RawMessage(`{"reason":"x"}`), Plan: step("no.such.action")},
		{Name: "n", Match: json.RawMessage(`{"reason":"x"}`), Plan: step(read), AutoExecute: true},
		{Name: "n", Match: json.RawMessage(`{"reason":"x"}`), Plan: step(read), AutoScope: json.RawMessage(`{"hours":"bad"}`)},
	}
	if high != "" {
		bad = append(bad, Runbook{Name: "n", Match: json.RawMessage(`{"reason":"x"}`), Plan: step(high), AutoExecute: true,
			AutoScope: json.RawMessage(`{"envs":["test"]}`)})
	}
	for i, rb := range bad {
		rb := rb
		if err := rbNormalize(&rb); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
}

func TestRunbookAutoExecutable(t *testing.T) {
	read := rbTestFirstAction(t, RiskRead)
	change := rbTestFirstAction(t, RiskChange)
	high := rbTestFirstAction(t, RiskHigh)
	if read == "" {
		t.Skip("no read action registered")
	}
	loc := policyLoc()
	defer func() { rbNow = time.Now }()
	rbNow = func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, loc) }
	reset := func() { rbAutoLog.Lock(); rbAutoLog.at = nil; rbAutoLog.Unlock() }
	reset()
	defer reset()

	mk := func(action, risk, scope string, auto bool) Runbook {
		return Runbook{Risk: risk, AutoExecute: auto, AutoScope: json.RawMessage(scope),
			Plan: PlanDraft{Steps: []StepDraft{{Action: action}}}}
	}
	on := Policy{AutoExecute: PolicyAutoExecute{Enabled: true, MaxPerHour: 2}}
	cid := int64(7)
	testCase := &Case{Env: "test", ClusterID: &cid}
	prodCase := &Case{Env: "prod", ClusterID: &cid}
	m := &Module{}

	type tc struct {
		name string
		rb   Runbook
		c    *Case
		pol  Policy
		want bool
	}
	tests := []tc{
		{"ok", mk(read, RiskRead, `{"envs":["test"]}`, true), testCase, on, true},
		{"runbook not whitelisted", mk(read, RiskRead, `{"envs":["test"]}`, false), testCase, on, false},
		{"policy off", mk(read, RiskRead, `{"envs":["test"]}`, true), testCase, Policy{}, false},
		{"cap zero", mk(read, RiskRead, `{"envs":["test"]}`, true), testCase, Policy{AutoExecute: PolicyAutoExecute{Enabled: true}}, false},
		{"nil case", mk(read, RiskRead, `{"envs":["test"]}`, true), nil, on, false},
		{"stored risk high", mk(read, RiskHigh, `{"envs":["test"]}`, true), testCase, on, false},
		{"no scope", mk(read, RiskRead, `{}`, true), testCase, on, false},
		{"env mismatch", mk(read, RiskRead, `{"envs":["prod"]}`, true), testCase, on, false},
		{"cluster ok", mk(read, RiskRead, `{"envs":["test"],"cluster_ids":[7]}`, true), testCase, on, true},
		{"cluster mismatch", mk(read, RiskRead, `{"envs":["test"],"cluster_ids":[8]}`, true), testCase, on, false},
		{"hours ok", mk(read, RiskRead, `{"envs":["test"],"hours":"09-21"}`, true), testCase, on, true},
		{"hours out", mk(read, RiskRead, `{"envs":["test"],"hours":"12-18"}`, true), testCase, on, false},
		{"hours overnight out", mk(read, RiskRead, `{"envs":["test"],"hours":"22-06"}`, true), testCase, on, false},
		{"unknown action", mk("no.such", RiskRead, `{"envs":["test"]}`, true), testCase, on, false},
	}
	if change != "" {
		tests = append(tests,
			tc{"change in test", mk(change, RiskChange, `{"envs":["test"]}`, true), testCase, on, true},
			tc{"change in prod is high", mk(change, RiskChange, `{"envs":["prod"]}`, true), prodCase, on, false},
		)
	}
	if high != "" {
		tests = append(tests, tc{"high action never", mk(high, RiskRead, `{"envs":["test"]}`, true), testCase, on, false})
	}
	for _, tt := range tests {
		reset()
		if got := m.autoExecutable(tt.rb, tt.c, tt.pol); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}

	// hourly cap
	reset()
	rb := mk(read, RiskRead, `{"envs":["test"]}`, true)
	rbRecordAutoExec()
	if !m.autoExecutable(rb, testCase, on) {
		t.Fatal("1 of 2 should pass")
	}
	rbRecordAutoExec()
	if m.autoExecutable(rb, testCase, on) {
		t.Fatal("cap reached should block")
	}
	rbNow = func() time.Time { return time.Date(2026, 11, 24, 11, 1, 0, 0, loc) }
	if !m.autoExecutable(rb, testCase, on) {
		t.Fatal("old executions should expire after an hour")
	}
}
