package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

const (
	clusterToken  = "cluster-token-good"
	upstreamToken = "upstream-sa-token"
	fingerprint   = "11111111-2222-3333-4444-555555555555"
	clusterID     = int64(11)
)

var relayKey = []byte("0123456789abcdef0123456789abcdef")

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// ---- fake kube-apiserver ----

type fakeAPIServer struct {
	srv     *httptest.Server
	caPEM   []byte
	hits    atomic.Int64
	mu      sync.Mutex
	headers []http.Header
	release chan struct{}
}

func newFakeAPIServer(t *testing.T, tlsH2 bool) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{release: make(chan struct{})}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		switch r.URL.Path {
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(k8sversion.Info{Major: "1", Minor: "32", GitVersion: "v1.32.2-fake"})
		case "/upgrade":
			if !strings.EqualFold(r.Header.Get("Upgrade"), "test-proto") {
				http.Error(w, "missing upgrade", http.StatusBadRequest)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported (HTTP/2?)", http.StatusInternalServerError)
				return
			}
			conn, brw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-proto\r\n\r\nhello\n")
			_ = brw.Flush()
			for {
				line, err := brw.ReadString('\n')
				if err != nil {
					return
				}
				_, _ = brw.WriteString("echo:" + line)
				_ = brw.Flush()
			}
		case "/stream":
			fl := w.(http.Flusher)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"ADDED"}` + "\n"))
			fl.Flush()
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Second):
			}
			_, _ = w.Write([]byte(`{"type":"MODIFIED"}` + "\n"))
			fl.Flush()
		default:
			http.NotFound(w, r)
		}
	})
	if tlsH2 {
		f.srv = httptest.NewUnstartedServer(h)
		f.srv.EnableHTTP2 = true
		f.srv.StartTLS()
		f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	} else {
		f.srv = httptest.NewServer(h)
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPIServer) seenHeaders() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.headers...)
}

// ---- platform hub ----

type platform struct {
	hub      *tunnel.Hub
	srv      *httptest.Server
	reg      tunnel.Registry
	mu       sync.Mutex
	infos    []tunnel.AgentInfo
	sessions []string
}

func newPlatform(t *testing.T, reg tunnel.Registry) *platform {
	t.Helper()
	p := &platform{reg: reg}
	var hp atomic.Pointer[tunnel.Hub]
	m := http.NewServeMux()
	m.HandleFunc("/api/v1"+tunnel.ConnectPath, func(w http.ResponseWriter, r *http.Request) { hp.Load().ServeAgent(w, r) })
	m.HandleFunc("/api/v1"+tunnel.RelayPath, func(w http.ResponseWriter, r *http.Request) { hp.Load().ServeRelay(w, r) })
	p.srv = httptest.NewServer(m)
	h, err := tunnel.NewHub(tunnel.Options{
		Authenticate: func(_ context.Context, info tunnel.AgentInfo) (int64, error) {
			if info.Token != clusterToken {
				return 0, errors.New("unknown token")
			}
			return clusterID, nil
		},
		Registry: reg,
		SelfAddr: p.srv.URL,
		RelayKey: relayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnConnect: func(_ int64, info tunnel.AgentInfo, sid string) {
			p.mu.Lock()
			p.infos = append(p.infos, info)
			p.sessions = append(p.sessions, sid)
			p.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	hp.Store(h)
	p.hub = h
	t.Cleanup(func() { _ = h.Close(); p.srv.Close() })
	return p
}

func (p *platform) online() bool {
	ok, _ := p.hub.Online(context.Background(), clusterID)
	return ok
}

func (p *platform) restConfig(t *testing.T) (*rest.Config, string) {
	t.Helper()
	cfg, sid, err := p.hub.RESTConfig(context.Background(), clusterID)
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	return cfg, sid
}

// ---- agent ----

type runningAgent struct {
	a         *agent
	tokenFile string
	logs      *syncBuf
	cancel    context.CancelFunc
	done      chan struct{}
}

func startAgent(t *testing.T, p *platform, api *fakeAPIServer, token string) *runningAgent {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "cluster-token")
	saFile := filepath.Join(dir, "sa-token")
	writeFile(t, tokenFile, token)
	writeFile(t, saFile, upstreamToken)

	env := map[string]string{
		"MATRIX_SERVER":              p.srv.URL,
		"MATRIX_ALLOW_INSECURE_HTTP": "true",
		"MATRIX_CLUSTER_TOKEN_FILE":  tokenFile,
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := platformTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	kcfg := &rest.Config{Host: api.srv.URL, BearerTokenFile: saFile}
	if api.caPEM != nil {
		kcfg.TLSClientConfig.CAData = api.caPEM
	}
	proxy, err := newUpstreamHandler(kcfg, log)
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(cfg, tlsCfg, proxy, log)
	a.fingerprint = func(context.Context) (string, error) { return fingerprint, nil }
	a.minBackoff, a.maxBackoff, a.drainPeriod = 20*time.Millisecond, 100*time.Millisecond, 200*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ra := &runningAgent{a: a, tokenFile: tokenFile, logs: logs, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(ra.done); a.run(ctx) }()
	t.Cleanup(ra.stop)
	return ra
}

func (ra *runningAgent) stop() {
	ra.cancel()
	select {
	case <-ra.done:
	case <-time.After(10 * time.Second):
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func rawRequest(t *testing.T, cfg *rest.Config, path, bearer string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, cfg.Host+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func readyz(a *agent) int {
	rec := httptest.NewRecorder()
	a.healthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

// ---- tests ----

// Test 1: a bad token gets 401 and registers nothing; the agent keeps
// retrying, logs "token rejected" without the token, and picks up a rotated
// token from its file.
func TestBadTokenRejectedThenRotated(t *testing.T) {
	p := newPlatform(t, tunnel.NewMemoryRegistry())
	api := newFakeAPIServer(t, false)
	ra := startAgent(t, p, api, "cluster-token-wrong")

	eventually(t, "token rejected log", func() bool { return strings.Count(ra.logs.String(), "token rejected") >= 2 })
	if p.online() {
		t.Fatal("cluster online with a bad token")
	}
	if _, err := p.reg.Lookup(context.Background(), clusterID); !errors.Is(err, tunnel.ErrOffline) {
		t.Fatalf("registry: %v", err)
	}
	if readyz(ra.a) != http.StatusServiceUnavailable {
		t.Fatal("readyz should fail without a tunnel")
	}
	if err := ra.a.session(context.Background()); !errors.Is(err, errTokenRejected) {
		t.Fatalf("session: want errTokenRejected, got %v", err)
	}

	writeFile(t, ra.tokenFile, clusterToken)
	eventually(t, "online after rotation", p.online)
	eventually(t, "readyz", func() bool { return readyz(ra.a) == http.StatusOK })
	if logs := ra.logs.String(); strings.Contains(logs, "cluster-token-wrong") || strings.Contains(logs, clusterToken) {
		t.Fatal("agent logged the cluster token")
	}
}

// Tests 2, 3, 4, 5 against a plain-HTTP and a TLS+HTTP/2-capable apiserver.
func TestEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tlsH2 bool
	}{{"http", false}, {"tls-h2-capable", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlatform(t, tunnel.NewMemoryRegistry())
			api := newFakeAPIServer(t, tc.tlsH2)
			ra := startAgent(t, p, api, clusterToken)
			eventually(t, "online", p.online)
			cfg, sid := p.restConfig(t)

			t.Run("version", func(t *testing.T) { testVersion(t, p, api, cfg, sid) })
			t.Run("session secret required", func(t *testing.T) { testSecretRequired(t, api, cfg) })
			t.Run("upgrade", func(t *testing.T) { testUpgrade(t, cfg) })
			t.Run("streaming", func(t *testing.T) { testStreaming(t, api, cfg) })

			// Never forward the session secret upstream.
			for _, h := range api.seenHeaders() {
				for k, vs := range h {
					for _, v := range vs {
						if strings.Contains(v, cfg.BearerToken) {
							t.Fatalf("session secret forwarded upstream in header %s", k)
						}
					}
				}
			}

			ra.stop()
			eventually(t, "offline after agent shutdown", func() bool { return !p.online() })
			if readyz(ra.a) != http.StatusServiceUnavailable {
				t.Fatal("readyz still OK after shutdown")
			}
		})
	}
}

func testVersion(t *testing.T, p *platform, api *fakeAPIServer, cfg *rest.Config, sid string) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if v.GitVersion != "v1.32.2-fake" {
		t.Fatalf("version %+v", v)
	}
	seen := api.seenHeaders()
	last := seen[len(seen)-1]
	if got := last.Get("Authorization"); got != "Bearer "+upstreamToken {
		t.Fatalf("upstream Authorization = %q", got)
	}
	if !strings.HasPrefix(last.Get("User-Agent"), "xingshu-matrix") {
		t.Fatalf("User-Agent not passed through: %q", last.Get("User-Agent"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.infos) == 0 || p.sessions[len(p.sessions)-1] != sid {
		t.Fatal("OnConnect not called for this session")
	}
	info := p.infos[len(p.infos)-1]
	if info.Fingerprint != fingerprint || info.Version != version.Version || info.Token != "" {
		t.Fatalf("agent info: fingerprint=%q version=%q tokenSet=%v", info.Fingerprint, info.Version, info.Token != "")
	}
}

func testSecretRequired(t *testing.T, api *fakeAPIServer, cfg *rest.Config) {
	before := api.hits.Load()
	for _, bearer := range []string{"", "wrong-secret", cfg.BearerToken + "x"} {
		code, body := rawRequest(t, cfg, "/version", bearer)
		if code != http.StatusUnauthorized || !strings.Contains(body, "invalid tunnel session credentials") {
			t.Fatalf("bearer %q: got %d %q", bearer, code, body)
		}
	}
	if api.hits.Load() != before {
		t.Fatal("unauthenticated stream reached the apiserver")
	}
	if code, _ := rawRequest(t, cfg, "/version", cfg.BearerToken); code != http.StatusOK {
		t.Fatalf("valid secret: %d", code)
	}
}

func testUpgrade(t *testing.T, cfg *rest.Config) {
	c, err := net.Dial("tcp", strings.TrimPrefix(cfg.Host, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := http.NewRequest(http.MethodGet, cfg.Host+"/upgrade", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "test-proto")
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 101, got %d %q", resp.StatusCode, b)
	}
	if line, _ := br.ReadString('\n'); line != "hello\n" {
		t.Fatalf("server-first bytes: %q", line)
	}
	big := strings.Repeat("x", 300<<10) // larger than one yamux window chunk
	for _, msg := range []string{"ping", big, "pong"} {
		if _, err := io.WriteString(c, msg+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if line != "echo:"+msg+"\n" {
			t.Fatalf("echo mismatch (len %d)", len(line))
		}
	}
}

func testStreaming(t *testing.T, api *fakeAPIServer, cfg *rest.Config) {
	req, _ := http.NewRequest(http.MethodGet, cfg.Host+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 2)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- l
		}
	}()
	select {
	case l := <-lines:
		if l != `{"type":"ADDED"}`+"\n" {
			t.Fatalf("first event %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event buffered: not received before the stream finished")
	}
	close(api.release)
	select {
	case l := <-lines:
		if l != `{"type":"MODIFIED"}`+"\n" {
			t.Fatalf("second event %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second event not received")
	}
}

// Test 6 (end to end): agent on replica A, client on replica B via relay.
func TestRelayEndToEnd(t *testing.T) {
	reg := tunnel.NewMemoryRegistry()
	a := newPlatform(t, reg)
	b := newPlatform(t, reg)
	api := newFakeAPIServer(t, false)
	startAgent(t, a, api, clusterToken)
	eventually(t, "online on B", b.online)
	cfg, sid := b.restConfig(t)
	_, sidA := a.restConfig(t)
	if sid != sidA {
		t.Fatal("replicas disagree on the session")
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		v, err := cs.Discovery().ServerVersion()
		if err != nil || v.GitVersion != "v1.32.2-fake" {
			t.Fatalf("relay ServerVersion: %v %v", v, err)
		}
	}
	testUpgrade(t, cfg)
}

// Test 7 (end to end): a reconnect yields a new session id and secret.
func TestAgentReconnectNewSession(t *testing.T) {
	p := newPlatform(t, tunnel.NewMemoryRegistry())
	api := newFakeAPIServer(t, false)
	startAgent(t, p, api, clusterToken)
	eventually(t, "online", p.online)
	cfg1, sid1 := p.restConfig(t)
	p.hub.Disconnect(clusterID) // e.g. token rotation; agent reconnects
	eventually(t, "new session", func() bool {
		_, sid, err := p.hub.RESTConfig(context.Background(), clusterID)
		return err == nil && sid != sid1
	})
	cfg2, _ := p.restConfig(t)
	if cfg2.BearerToken == cfg1.BearerToken {
		t.Fatal("secret reused across sessions")
	}
	if code, _ := rawRequest(t, cfg2, "/version", cfg1.BearerToken); code != http.StatusUnauthorized {
		t.Fatalf("old secret accepted on the new session: %d", code)
	}
	if code, _ := rawRequest(t, cfg2, "/version", cfg2.BearerToken); code != http.StatusOK {
		t.Fatalf("new session: %d", code)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "tok")
	writeFile(t, tf, "  abc \n")
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := loadConfig(env(map[string]string{"MATRIX_SERVER": "https://m.example.com/", "MATRIX_CLUSTER_TOKEN_FILE": tf}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ConnectURL != "wss://m.example.com/api/v1/cluster-agent/connect" {
		t.Fatalf("connect URL %q", c.ConnectURL)
	}
	if tok, _ := readToken(c); tok != "abc" {
		t.Fatalf("token %q", tok)
	}
	c, err = loadConfig(env(map[string]string{"MATRIX_SERVER": "https://m.example.com/matrix/api/v1", "MATRIX_CLUSTER_TOKEN": "t"}))
	if err != nil || c.ConnectURL != "wss://m.example.com/matrix/api/v1/cluster-agent/connect" {
		t.Fatalf("prefixed: %q %v", c.ConnectURL, err)
	}
	c, err = loadConfig(env(map[string]string{"MATRIX_SERVER": "http://m:8080", "MATRIX_CLUSTER_TOKEN": "t", "MATRIX_ALLOW_INSECURE_HTTP": "true"}))
	if err != nil || c.ConnectURL != "ws://m:8080/api/v1/cluster-agent/connect" {
		t.Fatalf("insecure: %q %v", c.ConnectURL, err)
	}
	bad := []map[string]string{
		{},
		{"MATRIX_SERVER": "https://m"},
		{"MATRIX_SERVER": "http://m", "MATRIX_CLUSTER_TOKEN": "t"},
		{"MATRIX_SERVER": "https://u:p@m", "MATRIX_CLUSTER_TOKEN": "t"},
		{"MATRIX_SERVER": "ftp://m", "MATRIX_CLUSTER_TOKEN": "t"},
		{"MATRIX_SERVER": "https://m", "MATRIX_CLUSTER_TOKEN_FILE": filepath.Join(dir, "missing")},
		{"MATRIX_SERVER": "https://m", "MATRIX_CLUSTER_TOKEN": "t", "MATRIX_TLS_SKIP_VERIFY": "maybe"},
	}
	for i, m := range bad {
		if _, err := loadConfig(env(m)); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if _, err := platformTLS(config{CAFile: tf}); err == nil {
		t.Fatal("non-PEM CA accepted")
	}
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jitter(time.Second)
		if d < 500*time.Millisecond || d >= time.Second {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
}
