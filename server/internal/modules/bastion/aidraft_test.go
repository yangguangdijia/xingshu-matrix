package bastion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

type fakeDraftModel struct {
	out    string
	system string
	user   string
}

func (f *fakeDraftModel) Complete(_ context.Context, _ *rbac.Principal, system, user string) (string, error) {
	f.system, f.user = system, user
	return f.out, nil
}

func aiDraftRequest(t *testing.T, model draftModel, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	old := newDraftModel
	newDraftModel = func(*Module) draftModel { return model }
	t.Cleanup(func() { newDraftModel = old })
	m := &Module{}
	r := gin.New()
	r.POST("/bastion/command-filters/ai-draft", m.aiDraftFilter)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bastion/command-filters/ai-draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return w.Code, env
}

func decodeAIDraft(t *testing.T, env map[string]any) AIDraftResult {
	t.Helper()
	b, _ := json.Marshal(env["data"])
	var res AIDraftResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

const aiDraftBody = `{"description":"禁止删除根目录","samples_block":["rm -rf /","ls; sudo /bin/rm -rf /"," "],"samples_allow":["rm -rf ./tmp","ls -l"]}`

func TestAIDraftValid(t *testing.T) {
	f := &fakeDraftModel{out: "```json\n" + `{"name":"禁止删除根目录","match_type":"regex","pattern":"^rm\\s+-[a-zA-Z]*r[a-zA-Z]*f?\\s+/(\\s|$)","action":"deny","reason":"拦截 rm -rf /"}` + "\n```"}
	code, env := aiDraftRequest(t, f, aiDraftBody)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, env)
	}
	res := decodeAIDraft(t, env)
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("expected valid draft, got %+v", res)
	}
	if res.Draft.Action != "deny" || res.Draft.MatchType != "regex" {
		t.Fatalf("draft %+v", res.Draft)
	}
	if len(res.Preview) != 4 {
		t.Fatalf("blank sample must be dropped, preview=%+v", res.Preview)
	}
	// 「ls; sudo /bin/rm -rf /」只有拆段（去 sudo、去路径）后才能命中。
	if p := res.Preview[1]; !p.Matched || !p.Pass || p.Expected != "block" {
		t.Fatalf("segment match expected: %+v", p)
	}
	if p := res.Preview[2]; p.Matched || !p.Pass || p.Expected != "allow" {
		t.Fatalf("allow sample: %+v", p)
	}
	if !strings.Contains(f.system, "regex") || !strings.Contains(f.user, "rm -rf ./tmp") {
		t.Fatalf("prompt missing match types or samples: %q / %q", f.system, f.user)
	}
}

func TestAIDraftInvalidRegex(t *testing.T) {
	f := &fakeDraftModel{out: `{"name":"坏正则","match_type":"regex","pattern":"^rm (?=-rf)","action":"deny","reason":"x"}`}
	code, env := aiDraftRequest(t, f, aiDraftBody)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	res := decodeAIDraft(t, env)
	if res.Valid || len(res.Preview) != 0 || !strings.Contains(strings.Join(res.Errors, ";"), "正则无法编译") {
		t.Fatalf("expected compile error, got %+v", res)
	}

	long := validateAIDraft(AIDraft{Name: "x", MatchType: "regex", Pattern: "^" + strings.Repeat("a", 1001), Action: "deny"}, nil, nil)
	if long.Valid || !strings.Contains(strings.Join(long.Errors, ";"), "1000") {
		t.Fatalf("expected length error, got %+v", long)
	}
	empty := validateAIDraft(AIDraft{Name: "x", MatchType: "keyword", Pattern: "a*", Action: "drop"}, nil, nil)
	joined := strings.Join(empty.Errors, ";")
	if empty.Valid || !strings.Contains(joined, "规则校验未通过") || !strings.Contains(joined, "匹配方式") || !strings.Contains(joined, "动作") {
		t.Fatalf("expected check()/type/action errors, got %+v", empty)
	}
}

func TestAIDraftPreviewMismatch(t *testing.T) {
	// 过宽的正则会误拦「应放行」样例；过窄的不会拆段命中。
	f := &fakeDraftModel{out: `{"name":"删除","match_type":"regex","pattern":"^rm\\b","action":"deny","reason":"x"}`}
	_, env := aiDraftRequest(t, f, aiDraftBody)
	res := decodeAIDraft(t, env)
	if res.Valid {
		t.Fatalf("expected mismatch, got %+v", res)
	}
	if p := res.Preview[2]; p.Command != "rm -rf ./tmp" || !p.Matched || p.Pass {
		t.Fatalf("allow sample should be flagged: %+v", p)
	}
	if !strings.Contains(strings.Join(res.Errors, ";"), "1 条样例与预期不符") {
		t.Fatalf("errors %+v", res.Errors)
	}
}

func TestAIDraftNoModel(t *testing.T) {
	// 默认工厂在 Deps 缺少 AI 服务时返回 nil。
	if newDraftModel(&Module{}) != nil {
		t.Fatal("expected nil model without deps")
	}
	code, env := aiDraftRequest(t, nil, aiDraftBody)
	if code != http.StatusServiceUnavailable || env["message"] != "未配置大模型" {
		t.Fatalf("status %d env %v", code, env)
	}
}

func TestAIDraftBadJSON(t *testing.T) {
	code, _ := aiDraftRequest(t, &fakeDraftModel{out: "好的，规则如下"}, aiDraftBody)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
}
