package bastion

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func rules() []FilterRule {
	return []FilterRule{
		{ID: 1, Name: "rm-root", Re: regexp.MustCompile(`rm\s+-rf\s+/`), Action: "deny"},
		{ID: 2, Name: "reboot", Re: regexp.MustCompile(`^(reboot|shutdown)\b`), Action: "confirm"},
	}
}

func TestDenyCommand(t *testing.T) {
	p := NewInputProcessor(rules())
	remote, client, ev := p.Input([]byte("ls -l\rrm -rf /tmp\r"))
	if len(ev) != 2 || ev[0].Action != "allow" || ev[1].Action != "deny" || ev[1].RuleID != 1 {
		t.Fatalf("events = %+v", ev)
	}
	if !strings.HasSuffix(string(remote), "rm -rf /tmp\x15\r") {
		t.Fatalf("remote = %q", remote)
	}
	if !strings.Contains(string(client), "命令被拒绝") {
		t.Fatalf("client = %q", client)
	}
}

func TestBackspaceAndConfirm(t *testing.T) {
	p := NewInputProcessor(rules())
	_, _, ev := p.Input([]byte("rebootx\x7f\r"))
	if len(ev) != 0 || p.pending == nil {
		t.Fatalf("expected pending confirm, ev=%+v", ev)
	}
	remote, _, ev := p.Input([]byte("yes\r"))
	if len(ev) != 1 || ev[0].Action != "confirmed" || string(remote) != "reboot\r" {
		t.Fatalf("ev=%+v remote=%q", ev, remote)
	}
	p.Input([]byte("shutdown now\r"))
	remote, _, ev = p.Input([]byte("n\r"))
	if ev[0].Action != "cancelled" || string(remote) != "\r" {
		t.Fatalf("ev=%+v remote=%q", ev, remote)
	}
}

func TestHistoryFallsBackToScreen(t *testing.T) {
	p := NewInputProcessor(rules())
	p.Input([]byte("\x1b[A")) // up arrow recalls history
	p.Output([]byte("\r\x1b[Kuser@h:~$ rm -rf /var"))
	_, _, ev := p.Input([]byte("\r"))
	if len(ev) != 1 || ev[0].Action != "deny" {
		t.Fatalf("ev=%+v", ev)
	}
}

func TestRecorderAsciicast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.cast")
	r, err := NewRecorder(path, 80, 24, "t")
	if err != nil {
		t.Fatal(err)
	}
	r.Output([]byte("hello\r\n"))
	r.Resize(100, 30)
	r.Close()
	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	var hdr map[string]any
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil || hdr["version"].(float64) != 2 || hdr["width"].(float64) != 80 {
		t.Fatalf("hdr=%v err=%v", hdr, err)
	}
	sc.Scan()
	var ev []any
	_ = json.Unmarshal(sc.Bytes(), &ev)
	if ev[1] != "o" || ev[2] != "hello\r\n" {
		t.Fatalf("event=%v", ev)
	}
}

func TestRoutesAndGuacdHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	m := New()
	m.guacdAddr = ln.Addr().String()
	e := gin.New()
	m.Register(e.Group("/api/v1"), &app.Deps{}) // must not panic (static + :id routes)
	h := gin.New()
	h.GET("/h", m.guacdHealth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/h", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reachable":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// 以下用例不依赖 PostgreSQL：覆盖请求校验、权限门、脱敏、连接器不可达与会话强制断开。
// 资产/授权/审计的 SQL 查询本身需要真实数据库，本轮明确未联调，不在单测里伪装通过。

func TestAssetInValidation(t *testing.T) {
	cases := []struct {
		name string
		in   assetIn
		err  bool
	}{
		{"valid ssh defaults env", assetIn{Name: "web-01", Host: "10.0.0.1", Port: 22, Protocol: "SSH"}, false},
		{"rdp accepted", assetIn{Name: "win-01", Host: "10.0.0.2", Port: 3389, Protocol: "RDP"}, false},
		{"postgres accepted", assetIn{Name: "db-01", Host: "10.0.0.3", Port: 5432, Protocol: "postgres"}, false},
		{"invalid protocol", assetIn{Name: "w", Host: "h", Port: 22, Protocol: "telnet"}, true},
		{"options rejects password", assetIn{Name: "w", Host: "h", Port: 22, Protocol: "ssh", Options: map[string]any{"password": "x"}}, true},
		{"options rejects private key", assetIn{Name: "w", Host: "h", Port: 22, Protocol: "ssh", Options: map[string]any{"Private_Key": "x"}}, true},
		{"options rejects token", assetIn{Name: "w", Host: "h", Port: 22, Protocol: "ssh", Options: map[string]any{"access_token": "x"}}, true},
		{"options keeps safe protocol fields", assetIn{Name: "w", Host: "h", Port: 5432, Protocol: "postgres", Options: map[string]any{"database": "app", "tls": "require", "readonly": true}}, false},
	}
	for _, c := range cases {
		in := c.in
		err := in.check()
		if c.err && err == nil {
			t.Fatalf("%s: expected validation error", c.name)
		}
		if !c.err && err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		if err == nil {
			if in.Env != "dev" {
				t.Fatalf("%s: env default = %q, want dev", c.name, in.Env)
			}
			if in.Options == nil {
				t.Fatalf("%s: options must default to an empty object", c.name)
			}
		}
	}
	in := assetIn{Protocol: "RDP"}
	if err := in.check(); err != nil {
		t.Fatal(err)
	}
	if in.Protocol != "rdp" {
		t.Fatalf("protocol = %q, want lowercased rdp", in.Protocol)
	}
}

func TestAssetOptionsSizeLimit(t *testing.T) {
	in := assetIn{Name: "w", Host: "h", Port: 22, Protocol: "ssh", Options: map[string]any{"note": strings.Repeat("x", 5000)}}
	if err := in.check(); err == nil {
		t.Fatal("expected options size validation error")
	}
}

func TestGuacdUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	m := New()
	m.guacdAddr = addr
	gin.SetMode(gin.TestMode)
	h := gin.New()
	h.GET("/h", m.guacdHealth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/h", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reachable":false`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestKillSessionForcesDisconnect(t *testing.T) {
	m := New()
	cancelled := 0
	m.register(42, func() { cancelled++ })
	if !m.kill(42) {
		t.Fatal("kill returned false for a live session")
	}
	if cancelled != 1 {
		t.Fatalf("cancel called %d times, want 1", cancelled)
	}
	if !m.unregister(42) {
		t.Fatal("unregister must report the session was killed")
	}
	if m.kill(42) {
		t.Fatal("kill must be false after the session was unregistered")
	}
	if m.unregister(42) {
		t.Fatal("unregister must be false for an unknown session id")
	}
}

func TestAccountSecretMasked(t *testing.T) {
	a := &Account{Secret: "super-secret-password-value"}
	maskAccount(a)
	if strings.Contains(a.Secret, "secret-password") {
		t.Fatalf("masked secret leaks the plaintext: %q", a.Secret)
	}
	if a.Secret == "" {
		t.Fatal("masked secret must not be empty")
	}
	short := &Account{Secret: "abc"}
	maskAccount(short)
	if short.Secret != "********" {
		t.Fatalf("short secret = %q", short.Secret)
	}
}

func TestBastionPermissionGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := New()

	// 未认证：401
	e := gin.New()
	m.Register(e.Group("/api/v1"), &app.Deps{})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/bastion/assets", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d %s", w.Code, w.Body)
	}

	// 只有 connect、没有 read：403（无授权边界）
	e2 := gin.New()
	e2.Use(func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Username: "ops", Permissions: []string{"bastion:connect"}})
	})
	m.Register(e2.Group("/api/v1"), &app.Deps{})

	w2 := httptest.NewRecorder()
	e2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/bastion/assets", nil))
	if w2.Code != http.StatusForbidden {
		t.Fatalf("read without bastion:read: %d %s", w2.Code, w2.Body)
	}

	// 只读主体不能写资产：403
	w3 := httptest.NewRecorder()
	e2.ServeHTTP(w3, httptest.NewRequest(http.MethodPost, "/api/v1/bastion/assets", strings.NewReader("{}")))
	if w3.Code != http.StatusForbidden {
		t.Fatalf("write without bastion:manage: %d %s", w3.Code, w3.Body)
	}
}

func TestEnvScopeDeniesOutOfScopeAsset(t *testing.T) {
	p := &rbac.Principal{EnvScope: []string{"dev"}}
	if p.CanEnv("prod") {
		t.Fatal("prod must be out of scope")
	}
	if !p.CanEnv("dev") {
		t.Fatal("dev must be in scope")
	}
	if !(&rbac.Principal{}).CanEnv("prod") {
		t.Fatal("an empty scope means all environments")
	}
}
