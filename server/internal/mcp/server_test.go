package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	tokAlice   = "test-token-alice"   // has k8s:pod:read only
	tokAdmin   = "test-token-admin"   // has everything
	tokSession = "test-session-token" // valid but not an API token
)

type fixture struct {
	srv      *Server
	mu       sync.Mutex
	audits   []AuditRecord
	executed map[string]int
	pending  []string
}

func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	f := &fixture{executed: map[string]int{}}
	reg := ai.NewToolRegistry()
	reg.MustRegister(ai.Tool{Name: "k8s_get_pods", Module: "k8s", Description: "list pods", Permission: "k8s:pod:read",
		Parameters: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"}}}`),
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			if rbac.FromContext(ctx) == nil {
				return nil, errors.New("no principal in ctx")
			}
			f.mu.Lock()
			f.executed["k8s_get_pods"]++
			f.mu.Unlock()
			return map[string]any{"pods": []string{"web-1"}}, nil
		}})
	reg.MustRegister(ai.Tool{Name: "k8s_delete_pod", Module: "k8s", Description: "delete a pod", Permission: "k8s:pod:delete",
		RequiresConfirmation: true,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			f.mu.Lock()
			f.executed["k8s_delete_pod"]++
			f.mu.Unlock()
			return "deleted", nil
		}})
	reg.MustRegister(ai.Tool{Name: "secret_read", Module: "sys", Description: "admin only", Permission: "sys:secret:read",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			f.mu.Lock()
			f.executed["secret_read"]++
			f.mu.Unlock()
			return "s", nil
		}})
	reg.MustRegister(ai.Tool{Name: "k8s_fail", Module: "k8s", Description: "fails", Permission: "k8s:pod:read",
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) { return nil, errors.New("boom") }})

	authFn := func(ctx context.Context, tok, ip string) (*rbac.Principal, error) {
		switch tok {
		case tokAlice:
			return &rbac.Principal{UserID: 1, Username: "alice", TokenID: 11, AuthMethod: "token", Permissions: []string{"k8s:pod:read"}}, nil
		case tokAdmin:
			return &rbac.Principal{UserID: 2, Username: "admin", TokenID: 22, AuthMethod: "token", Permissions: []string{"*"}}, nil
		case tokSession:
			return &rbac.Principal{UserID: 1, Username: "alice", SessionID: "s1", AuthMethod: "session", Permissions: []string{"*"}}, nil
		}
		return nil, errors.New("bad token")
	}
	if o.Audit == nil {
		o.Audit = func(ctx context.Context, rec AuditRecord) {
			f.mu.Lock()
			f.audits = append(f.audits, rec)
			f.mu.Unlock()
		}
	}
	if o.Pending == nil {
		o.Pending = func(ctx context.Context, p *rbac.Principal, tool string, args json.RawMessage) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.pending = append(f.pending, tool)
			return "/api/v1/ai/tool-calls/pc-1/confirm", nil
		}
	}
	f.srv = NewWithOptions(reg, authFn, o)
	return f
}

func (f *fixture) do(t *testing.T, method, tok, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://matrix.example.com/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, req)
	return w
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder) rpcResp {
	t.Helper()
	var r rpcResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

func callTool(t *testing.T, f *fixture, tok, name, args string) rpcResp {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	w := f.do(t, http.MethodPost, tok, body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return decode(t, w)
}

type callRes struct {
	Content []struct {
		Type, Text string
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

func TestInitialize(t *testing.T) {
	f := newFixture(t, Options{Version: "1.33"})
	w := f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
	r := decode(t, w)
	if r.Error != nil || string(r.ID) != "1" {
		t.Fatalf("unexpected %s", w.Body.String())
	}
	var res struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      map[string]string          `json:"serverInfo"`
	}
	_ = json.Unmarshal(r.Result, &res)
	if res.ProtocolVersion != "2025-06-18" || res.Capabilities["tools"] == nil || res.ServerInfo["title"] != "星枢 Matrix" || res.ServerInfo["version"] != "1.33" {
		t.Fatalf("bad initialize result %s", r.Result)
	}
	// unknown client version -> server's own version
	r = decode(t, f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil))
	_ = json.Unmarshal(r.Result, &res)
	if res.ProtocolVersion != ProtocolVersion {
		t.Fatalf("want fallback version, got %s", res.ProtocolVersion)
	}
	// missing protocolVersion -> -32602
	r = decode(t, f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{}}`, nil))
	if r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatalf("want -32602, got %+v", r.Error)
	}
}

func TestNotificationAndPing(t *testing.T) {
	f := newFixture(t, Options{})
	w := f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", w.Code, w.Body.String())
	}
	r := decode(t, f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","id":"p","method":"ping"}`, nil))
	if r.Error != nil || string(r.Result) != "{}" || string(r.ID) != `"p"` {
		t.Fatalf("ping: %+v %s", r.Error, r.Result)
	}
}

func listNames(t *testing.T, f *fixture, tok string) map[string]mcpTool {
	t.Helper()
	r := decode(t, f.do(t, http.MethodPost, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil))
	var res struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]mcpTool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

func TestToolsListFiltered(t *testing.T) {
	f := newFixture(t, Options{})
	alice := listNames(t, f, tokAlice)
	if _, ok := alice["k8s_get_pods"]; !ok || len(alice) != 2 {
		t.Fatalf("alice sees %v", alice)
	}
	if _, ok := alice["secret_read"]; ok {
		t.Fatal("alice must not see secret_read")
	}
	admin := listNames(t, f, tokAdmin)
	if len(admin) != 4 {
		t.Fatalf("admin sees %d tools", len(admin))
	}
	g := admin["k8s_get_pods"].Annotations
	d := admin["k8s_delete_pod"].Annotations
	if !g.ReadOnlyHint || g.DestructiveHint || d.ReadOnlyHint || !d.DestructiveHint {
		t.Fatalf("annotations: get=%+v delete=%+v", g, d)
	}
	if !json.Valid(admin["k8s_delete_pod"].InputSchema) {
		t.Fatal("inputSchema invalid")
	}
}

func TestToolsCallSuccess(t *testing.T) {
	f := newFixture(t, Options{})
	r := callTool(t, f, tokAlice, "k8s_get_pods", `{"namespace":"default","password":"hunter2"}`)
	var res callRes
	_ = json.Unmarshal(r.Result, &res)
	if r.Error != nil || res.IsError || !strings.Contains(res.Content[0].Text, "web-1") {
		t.Fatalf("call: %+v %s", r.Error, r.Result)
	}
	if f.executed["k8s_get_pods"] != 1 {
		t.Fatal("handler not executed")
	}
	if len(f.audits) != 1 {
		t.Fatalf("audits %d", len(f.audits))
	}
	a := f.audits[0]
	if a.Outcome != "ok" || a.UserID != 1 || a.TokenID != 11 || a.Tool != "k8s_get_pods" {
		t.Fatalf("audit %+v", a)
	}
	if strings.Contains(a.Args, "hunter2") || !strings.Contains(a.Args, `"password":"***"`) || !strings.Contains(a.Args, "default") {
		t.Fatalf("args not redacted: %s", a.Args)
	}
}

func TestToolsCallHandlerError(t *testing.T) {
	f := newFixture(t, Options{})
	var res callRes
	r := callTool(t, f, tokAlice, "k8s_fail", `{}`)
	_ = json.Unmarshal(r.Result, &res)
	if r.Error != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "boom") || f.audits[0].Outcome != "error" {
		t.Fatalf("got %+v %s", r.Error, r.Result)
	}
}

func TestToolsCallForbidden(t *testing.T) {
	f := newFixture(t, Options{})
	r := callTool(t, f, tokAlice, "secret_read", `{}`)
	var res callRes
	_ = json.Unmarshal(r.Result, &res)
	if r.Error != nil || !res.IsError || !strings.Contains(res.Content[0].Text, "权限不足") {
		t.Fatalf("got %+v %s", r.Error, r.Result)
	}
	if f.executed["secret_read"] != 0 {
		t.Fatal("forbidden tool executed")
	}
	if f.audits[0].Outcome != "forbidden" {
		t.Fatalf("audit %+v", f.audits[0])
	}
}

func TestToolsCallNeedsConfirmation(t *testing.T) {
	f := newFixture(t, Options{})
	r := callTool(t, f, tokAdmin, "k8s_delete_pod", `{"name":"web-1"}`)
	var res callRes
	_ = json.Unmarshal(r.Result, &res)
	if r.Error != nil || res.IsError {
		t.Fatalf("got %+v %s", r.Error, r.Result)
	}
	if !strings.Contains(res.Content[0].Text, "该操作需在平台内确认") || !strings.Contains(res.Content[0].Text, "/api/v1/ai/tool-calls/pc-1/confirm") {
		t.Fatalf("text %q", res.Content[0].Text)
	}
	if res.StructuredContent["status"] != "pending_confirmation" {
		t.Fatalf("structured %v", res.StructuredContent)
	}
	if f.executed["k8s_delete_pod"] != 0 {
		t.Fatal("confirmation tool executed")
	}
	if len(f.pending) != 1 || f.audits[0].Outcome != "pending_confirmation" {
		t.Fatalf("pending %v audits %+v", f.pending, f.audits)
	}
}

func TestToolsCallInvalid(t *testing.T) {
	f := newFixture(t, Options{})
	for name, body := range map[string]string{
		"unknown tool":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`,
		"no name":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		"no params":     `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`,
		"args not obj":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get_pods","arguments":[1]}}`,
		"params wrong":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`,
		"init bad type": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
	} {
		r := decode(t, f.do(t, http.MethodPost, tokAlice, body, nil))
		if r.Error == nil || r.Error.Code != CodeInvalidParams {
			t.Errorf("%s: want -32602, got %+v", name, r.Error)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	f := newFixture(t, Options{})
	cases := []struct {
		name, body string
		code       int
	}{
		{"parse", `{"jsonrpc":"2.0",`, CodeParseError},
		{"empty", ``, CodeParseError},
		{"bad version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, CodeInvalidRequest},
		{"no method", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, CodeInvalidRequest},
		{"scalar", `42`, CodeInvalidRequest},
		{"empty batch", `[]`, CodeInvalidRequest},
		{"bad params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":3}`, CodeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, CodeMethodNotFound},
	}
	for _, c := range cases {
		w := f.do(t, http.MethodPost, tokAlice, c.body, nil)
		r := decode(t, w)
		if r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s: want %d, got %+v (%s)", c.name, c.code, r.Error, w.Body.String())
		}
	}
}

func TestInternalError(t *testing.T) {
	f := newFixture(t, Options{Pending: func(ctx context.Context, p *rbac.Principal, tool string, args json.RawMessage) (string, error) {
		return "", errors.New("db down")
	}})
	r := callTool(t, f, tokAdmin, "k8s_delete_pod", `{}`)
	if r.Error == nil || r.Error.Code != CodeInternalError {
		t.Fatalf("want -32603, got %+v", r.Error)
	}
	if f.executed["k8s_delete_pod"] != 0 {
		t.Fatal("executed despite pending failure")
	}
}

func TestBatch(t *testing.T) {
	f := newFixture(t, Options{})
	body := `[
	 {"jsonrpc":"2.0","id":1,"method":"ping"},
	 {"jsonrpc":"2.0","method":"notifications/initialized"},
	 {"jsonrpc":"2.0","id":2,"method":"nope"},
	 {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"k8s_get_pods"}},
	 5
	]`
	w := f.do(t, http.MethodPost, tokAlice, body, nil)
	var rs []rpcResp
	if err := json.Unmarshal(w.Body.Bytes(), &rs); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	if len(rs) != 4 {
		t.Fatalf("want 4 responses, got %d: %s", len(rs), w.Body.String())
	}
	if rs[0].Error != nil || string(rs[0].ID) != "1" {
		t.Errorf("ping %+v", rs[0])
	}
	if rs[1].Error == nil || rs[1].Error.Code != CodeMethodNotFound {
		t.Errorf("nope %+v", rs[1])
	}
	if rs[2].Error != nil || string(rs[2].ID) != "3" {
		t.Errorf("call %+v", rs[2])
	}
	if rs[3].Error == nil || rs[3].Error.Code != CodeInvalidRequest || string(rs[3].ID) != "null" {
		t.Errorf("scalar %+v", rs[3])
	}
	// batch of notifications only -> 202
	w = f.do(t, http.MethodPost, tokAlice, `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("notif batch %d", w.Code)
	}
}

func TestGETNotAllowed(t *testing.T) {
	f := newFixture(t, Options{})
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		w := f.do(t, m, tokAlice, "", nil)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: %d", m, w.Code)
		}
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t, Options{})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	// cookie only
	w := f.do(t, http.MethodPost, "", ping, map[string]string{"Cookie": "session=abc"})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Cookie") {
		t.Fatalf("cookie: %d %s", w.Code, w.Body.String())
	}
	// none
	if w = f.do(t, http.MethodPost, "", ping, nil); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("none: %d", w.Code)
	}
	// session token (not API token)
	if w = f.do(t, http.MethodPost, tokSession, ping, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("session token: %d", w.Code)
	}
	// wrong scheme / bad token
	if w = f.do(t, http.MethodPost, "", ping, map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}); w.Code != http.StatusUnauthorized {
		t.Fatalf("basic: %d", w.Code)
	}
	if w = f.do(t, http.MethodPost, "bogus", ping, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bogus: %d", w.Code)
	}
	// API token + a stray cookie is fine: the cookie is ignored
	if w = f.do(t, http.MethodPost, tokAlice, ping, map[string]string{"Cookie": "session=abc"}); w.Code != http.StatusOK {
		t.Fatalf("token+cookie: %d", w.Code)
	}
}

func TestOrigin(t *testing.T) {
	f := newFixture(t, Options{AllowedOrigins: []string{"https://ide.example.org"}})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if w := f.do(t, http.MethodPost, tokAlice, ping, map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("evil origin: %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, tokAlice, ping, map[string]string{"Origin": "http://matrix.example.com"}); w.Code != http.StatusOK {
		t.Fatalf("same origin: %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, tokAlice, ping, map[string]string{"Origin": "https://ide.example.org"}); w.Code != http.StatusOK {
		t.Fatalf("allowed origin: %d", w.Code)
	}
}

func TestBodyLimitAndProtocolHeader(t *testing.T) {
	f := newFixture(t, Options{})
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}}`
	if w := f.do(t, http.MethodPost, tokAlice, big, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, tokAlice, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad protocol header: %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, tokAlice, `{}`, map[string]string{"Content-Type": "text/plain"}); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", w.Code)
	}
}

func TestRateLimitPerUser(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newFixture(t, Options{RatePerSecond: 1, Burst: 3, Now: clock})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for i := 0; i < 3; i++ {
		if w := f.do(t, http.MethodPost, tokAlice, ping, nil); w.Code != http.StatusOK {
			t.Fatalf("req %d: %d", i, w.Code)
		}
	}
	if w := f.do(t, http.MethodPost, tokAlice, ping, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", w.Code)
	}
	// another user has their own bucket
	if w := f.do(t, http.MethodPost, tokAdmin, ping, nil); w.Code != http.StatusOK {
		t.Fatalf("admin: %d", w.Code)
	}
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	if w := f.do(t, http.MethodPost, tokAlice, ping, nil); w.Code != http.StatusOK {
		t.Fatalf("after refill: %d", w.Code)
	}
}

func TestSummarizeArgs(t *testing.T) {
	s := summarizeArgs(json.RawMessage(`{"a":{"api_key":"k","Token":"t","list":[1,2]},"note":"` + strings.Repeat("x", 200) + `"}`))
	if strings.Contains(s, `"k"`) || strings.Contains(s, `"t"`) || len(s) > maxArgSummary+4 || !strings.Contains(s, "…") {
		t.Fatalf("summary %s", s)
	}
	if summarizeArgs(nil) != "{}" || summarizeArgs(json.RawMessage(`{bad`)) != "<invalid json>" {
		t.Fatal("edge cases")
	}
}
