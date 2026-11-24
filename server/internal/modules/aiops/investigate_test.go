package aiops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// invTestAction is a read-only fake registered only in tests.
type invTestAction struct{}

func (invTestAction) Name() string { return "inv.test.read" }
func (invTestAction) Spec() ActionSpec {
	return ActionSpec{Name: "inv.test.read", Title: "测试只读", Risk: RiskRead, Channel: "platform", Params: json.RawMessage(`{}`), ReadOnly: true}
}
func (invTestAction) Validate(context.Context, *ExecContext, json.RawMessage) (json.RawMessage, string, error) {
	return json.RawMessage(`{}`), RiskRead, nil
}
func (invTestAction) Describe(context.Context, *ExecContext, json.RawMessage) (string, string, string, error) {
	return "测试", "", "", nil
}
func (invTestAction) Execute(context.Context, *ExecContext, json.RawMessage) (string, error) {
	return "", nil
}
func (invTestAction) Rollback(context.Context, *ExecContext, json.RawMessage, string) (string, error) {
	return "", ErrNoRollback
}

func init() {
	if _, ok := ActionByName("inv.test.read"); !ok {
		registerAction(invTestAction{})
	}
}

func TestInvParseModelOutputFence(t *testing.T) {
	raw := "```json\n{\"conclusion\":{\"summary\":\"内存不足\",\"confidence\":\"high\"},\"plans\":[]}\n```"
	out, err := invParseModelOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Conclusion.Summary != "内存不足" || out.Conclusion.Confidence != "high" {
		t.Fatalf("unexpected: %+v", out.Conclusion)
	}
	// Leading prose around the object is tolerated.
	if _, err := invParseModelOutput("结论如下：{\"conclusion\":{\"summary\":\"x\"}} 完"); err != nil {
		t.Fatalf("prose-wrapped: %v", err)
	}
	if _, err := invParseModelOutput("not json at all"); err == nil {
		t.Fatal("expected error for garbage")
	}
}

func TestInvApplyModelFallback(t *testing.T) {
	rule := Conclusion{Summary: "规则结论", Findings: []Finding{{Analyzer: "crashloop"}}}
	rulePlans := []PlanDraft{{Title: "规则方案", Steps: []StepDraft{{Action: "inv.test.read"}}}}

	c, p, eng := invApplyModel("模型乱说一通", rule, rulePlans)
	if eng != invEngineRules || c.Summary != "规则结论" || len(p) != 1 {
		t.Fatalf("fallback failed: %s %+v %+v", eng, c, p)
	}
	c, _, eng = invApplyModel(`{"conclusion":{"summary":""}}`, rule, rulePlans)
	if eng != invEngineRules || c.Summary != "规则结论" {
		t.Fatalf("empty summary should fall back: %s", eng)
	}
	c, p, eng = invApplyModel(`{"conclusion":{"summary":"模型结论","confidence":"weird"},"plans":[]}`, rule, rulePlans)
	if eng != invEngineLLM || c.Summary != "模型结论" || c.Confidence != "low" {
		t.Fatalf("llm result: %s %+v", eng, c)
	}
	if len(c.Findings) != 1 || len(p) != 1 || p[0].Title != "规则方案" {
		t.Fatalf("rule findings/plans should be kept when model has no valid plan: %+v %+v", c.Findings, p)
	}
}

func TestInvUnregisteredActionDropped(t *testing.T) {
	raw := `{"conclusion":{"summary":"s"},"plans":[
		{"title":"A","steps":[{"action":"inv.test.read","params":{"a":1}},{"action":"rm.rf.everything","params":{}}],
		 "verify":[{"action":"made.up"},{"action":"inv.test.read","params":"bad"}]},
		{"title":"B","steps":[{"action":"nope.x"}]}]}`
	_, plans, eng := invApplyModel(raw, Conclusion{}, nil)
	if eng != invEngineLLM {
		t.Fatalf("engine %s", eng)
	}
	if len(plans) != 1 || plans[0].Title != "A" {
		t.Fatalf("plan B (no valid steps) should be dropped: %+v", plans)
	}
	if len(plans[0].Steps) != 1 || plans[0].Steps[0].Action != "inv.test.read" {
		t.Fatalf("unregistered step kept: %+v", plans[0].Steps)
	}
	if len(plans[0].Verify) != 1 || !json.Valid(plans[0].Verify[0].Params) {
		t.Fatalf("verify filtering: %+v", plans[0].Verify)
	}
}

func TestInvSecretMasking(t *testing.T) {
	cases := []string{
		`password=hunter2xyz`,
		`{"api_key":"sk-abcdef123456"}`,
		`Authorization: Bearer abcdefghijklmnop`,
		`mysql://root:s3cretPW@db:3306/app`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
		`DB_TOKEN: tkn_9988776655`,
	}
	leaks := []string{"hunter2xyz", "sk-abcdef123456", "abcdefghijklmnop", "s3cretPW", "MIIEow", "tkn_9988776655"}
	for i, s := range cases {
		got := invSecretText(s)
		if strings.Contains(got, leaks[i]) {
			t.Errorf("secret leaked: %q -> %q", s, got)
		}
	}
	if got := invSecretText("pod web-1 CrashLoopBackOff"); got != "pod web-1 CrashLoopBackOff" {
		t.Errorf("benign text changed: %q", got)
	}
	long := strings.Repeat("中", 100)
	if got := invSafeEvidence(long, 10); !strings.HasSuffix(got, "…(已截断)") || len(got) > 10+len("…(已截断)") {
		t.Errorf("truncation: %q", got)
	}
}

func TestInvSystemPromptAndRejections(t *testing.T) {
	p := invSystemPrompt([]ActionSpec{invTestAction{}.Spec()}, []string{"不要重启生产 token=abc12345"})
	if !strings.Contains(p, "inv.test.read") || !strings.Contains(p, "不要重启生产") || strings.Contains(p, "abc12345") {
		t.Fatalf("prompt content wrong:\n%s", p)
	}
	if got := invRejectionNote(`{"reason":"风险太高","title":"重启"}`); got != "方案《重启》：风险太高" {
		t.Fatalf("rejection note: %q", got)
	}
	if invRejectionNote(`{}`) != "" || invRejectionNote(`bad`) != "" {
		t.Fatal("empty rejection should be ignored")
	}
}

func TestInvCollectK8sFake(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "demo"}, Spec: corev1.PodSpec{NodeName: "n1"},
			Status: corev1.PodStatus{Phase: corev1.PodPending}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "demo"}, Data: map[string][]byte{"password": []byte("x")}},
	)
	var ev []EvidenceItem
	in := invCollectK8s(context.Background(), cs, invScope{ClusterID: 1, Namespace: "demo"}, AnalyzerInput{ClusterID: 1, Namespace: "demo"}, &ev)
	if len(in.Pods) != 1 || len(in.Nodes) != 1 || len(in.Secrets) != 1 || in.Secrets[0] != "db" {
		t.Fatalf("collect: pods=%d nodes=%d secrets=%v", len(in.Pods), len(in.Nodes), in.Secrets)
	}
	for _, e := range ev {
		if e.Status == "unavailable" {
			t.Errorf("unexpected unavailable: %+v", e)
		}
	}
	_ = runAnalyzers(in)
}

func TestInvParseStartArgs(t *testing.T) {
	in, err := invParseStartArgs(json.RawMessage(`{"kind":"deploy","request":"部署 nginx 到 10.0.0.5 password=abc123"}`))
	if err != nil || in.Kind != KindDeploy || in.Source != SourceChat || strings.Contains(in.Request, "abc123") {
		t.Fatalf("%v %+v", err, in)
	}
	if _, err := invParseStartArgs(json.RawMessage(`{"kind":"alert","request":"x"}`)); err == nil {
		t.Fatal("alert without alert_id must fail")
	}
}
