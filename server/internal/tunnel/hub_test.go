package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// tokenAuth accepts "tok-<id>".
func tokenAuth(_ context.Context, info AgentInfo) (int64, error) {
	if !strings.HasPrefix(info.Token, "tok-") {
		return 0, errors.New("bad token")
	}
	return strconv.ParseInt(strings.TrimPrefix(info.Token, "tok-"), 10, 64)
}

type eventLog struct {
	mu   sync.Mutex
	list []string
}

func (e *eventLog) add(s string) { e.mu.Lock(); e.list = append(e.list, s); e.mu.Unlock() }
func (e *eventLog) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

type testHub struct {
	hub    *Hub
	srv    *httptest.Server
	events *eventLog
}

func newTestHub(t *testing.T, reg Registry, mod func(*Options)) *testHub {
	t.Helper()
	var hp atomic.Pointer[Hub]
	m := http.NewServeMux()
	m.HandleFunc("/api/v1"+ConnectPath, func(w http.ResponseWriter, r *http.Request) { hp.Load().ServeAgent(w, r) })
	m.HandleFunc("/api/v1"+RelayPath, func(w http.ResponseWriter, r *http.Request) { hp.Load().ServeRelay(w, r) })
	srv := httptest.NewServer(m)
	ev := &eventLog{}
	o := Options{
		Authenticate: tokenAuth,
		Registry:     reg,
		SelfAddr:     srv.URL,
		RelayKey:     testKey,
		Logger:       quietLogger(),
		OnConnect: func(id int64, info AgentInfo, sid string) {
			if info.Token != "" {
				ev.add("token-leaked")
			}
			ev.add(fmt.Sprintf("connect %d %s", id, sid))
		},
		OnDisconnect: func(id int64, sid string) { ev.add(fmt.Sprintf("disconnect %d %s", id, sid)) },
	}
	if mod != nil {
		mod(&o)
	}
	h, err := NewHub(o)
	if err != nil {
		t.Fatal(err)
	}
	hp.Store(h)
	t.Cleanup(func() { _ = h.Close(); srv.Close() })
	return &testHub{hub: h, srv: srv, events: ev}
}

type fakeAgent struct {
	mux       *yamux.Session
	sessionID string
	secret    string
}

func connectURL(base string) string {
	return "ws" + strings.TrimPrefix(base, "http") + "/api/v1" + ConnectPath
}

func dialFakeAgent(t *testing.T, base, token string) (*fakeAgent, *http.Response, error) {
	t.Helper()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	ws, resp, err := websocket.DefaultDialer.Dial(connectURL(base), hdr)
	if err != nil {
		return nil, resp, err
	}
	fa := &fakeAgent{sessionID: resp.Header.Get(wire.HeaderSessionID), secret: resp.Header.Get(wire.HeaderSessionSecret)}
	mux, err := yamux.Server(wire.NewConn(ws), wire.YamuxConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	fa.mux = mux
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wire.SecretEqual(wire.BearerToken(r.Header), fa.secret) {
			http.Error(w, "bad secret", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "session=%s path=%s", fa.sessionID, r.URL.Path)
	})}
	go func() { _ = srv.Serve(mux) }()
	t.Cleanup(func() { _ = mux.Close() })
	return fa, resp, nil
}

func mustDialFakeAgent(t *testing.T, base, token string) *fakeAgent {
	t.Helper()
	fa, _, err := dialFakeAgent(t, base, token)
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	return fa
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func online(h *Hub, id int64) func() bool {
	return func() bool { ok, _ := h.Online(context.Background(), id); return ok }
}

func get(t *testing.T, cfg *rest.Config, path string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, cfg.Host+path, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestNewHubValidation(t *testing.T) {
	reg := NewMemoryRegistry()
	cases := []Options{
		{Registry: reg},
		{Authenticate: tokenAuth},
		{Authenticate: tokenAuth, Registry: reg, SelfAddr: "http://10.0.0.1:8080"},
		{Authenticate: tokenAuth, Registry: reg, SelfAddr: "http://10.0.0.1:8080", RelayKey: []byte("short")},
		{Authenticate: tokenAuth, Registry: reg, SelfAddr: "ftp://x", RelayKey: testKey},
	}
	for i, o := range cases {
		if _, err := NewHub(o); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	h, err := NewHub(Options{Authenticate: tokenAuth, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	if h.o.StaleAfter != 45*time.Second || h.heartbeat != 15*time.Second {
		t.Fatalf("defaults: stale=%v heartbeat=%v", h.o.StaleAfter, h.heartbeat)
	}
	_ = h.Close()
}

// Test 1 (hub side): bad or missing token → 401, nothing registered.
func TestServeAgentRejectsBadToken(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	for _, tok := range []string{"", "nope"} {
		_, resp, err := dialFakeAgent(t, th.srv.URL, tok)
		if err == nil {
			t.Fatalf("token %q: expected handshake failure", tok)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401, got %v", tok, resp)
		}
		if resp.Header.Get(wire.HeaderSessionSecret) != "" || resp.Header.Get(wire.HeaderSessionID) != "" {
			t.Fatal("session headers returned on rejected connect")
		}
	}
	if _, err := reg.Lookup(context.Background(), 1); !errors.Is(err, ErrOffline) {
		t.Fatalf("registry: want ErrOffline, got %v", err)
	}
	if len(th.events.snapshot()) != 0 {
		t.Fatalf("unexpected events %v", th.events.snapshot())
	}
	resp, err := http.Get(th.srv.URL + "/api/v1" + ConnectPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET: want 400, got %d", resp.StatusCode)
	}
}

func TestLocalSessionRoundTrip(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	fa := mustDialFakeAgent(t, th.srv.URL, "tok-7")
	if fa.sessionID == "" || len(fa.secret) < 40 {
		t.Fatalf("session headers missing: id=%q secretLen=%d", fa.sessionID, len(fa.secret))
	}
	eventually(t, "online", online(th.hub, 7))
	cfg, sid, err := th.hub.RESTConfig(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if sid != fa.sessionID || cfg.BearerToken != fa.secret {
		t.Fatal("RESTConfig does not match the session")
	}
	if !strings.HasPrefix(cfg.Host, "http://127.0.0.1:") || cfg.Timeout != 30*time.Second || cfg.QPS != 50 || cfg.Burst != 100 || cfg.UserAgent != "xingshu-matrix" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	code, body := get(t, cfg, "/version")
	if code != 200 || body != "session="+sid+" path=/version" {
		t.Fatalf("got %d %q", code, body)
	}
	cfg2, _, _ := th.hub.RESTConfig(context.Background(), 7)
	if cfg2.Host != cfg.Host {
		t.Fatal("listener not reused")
	}
	if ev := th.events.snapshot(); len(ev) != 1 || ev[0] != "connect 7 "+sid {
		t.Fatalf("events %v", ev)
	}
}

// Test 6: relay across replicas sharing a registry, plus relay auth.
func TestRelayAcrossHubs(t *testing.T) {
	reg := NewMemoryRegistry()
	a := newTestHub(t, reg, nil)
	b := newTestHub(t, reg, nil)
	fa := mustDialFakeAgent(t, a.srv.URL, "tok-3")
	eventually(t, "online on B", online(b.hub, 3))

	o, err := reg.Lookup(context.Background(), 3)
	if err != nil || o.Addr != a.srv.URL {
		t.Fatalf("owner %+v err %v", o, err)
	}
	cfg, sid, err := b.hub.RESTConfig(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if sid != fa.sessionID || b.hub.localSession(3) != nil {
		t.Fatal("B should relay to A's session")
	}
	for i := 0; i < 3; i++ {
		code, body := get(t, cfg, "/api")
		if code != 200 || body != "session="+sid+" path=/api" {
			t.Fatalf("relay got %d %q", code, body)
		}
	}

	relayURL := "ws" + strings.TrimPrefix(a.srv.URL, "http") + "/api/v1" + RelayPath
	dial := func(cluster int64, auth string) int {
		hdr := http.Header{}
		if auth != "" {
			hdr.Set(wire.HeaderRelayAuth, auth)
		}
		ws, resp, err := websocket.DefaultDialer.Dial(relayURL+"?cluster_id="+strconv.FormatInt(cluster, 10), hdr)
		if err == nil {
			ws.Close()
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("relay dial: %v", err)
		}
		return resp.StatusCode
	}
	now := time.Now()
	otherKey := []byte("ffffffffffffffffffffffffffffffff")
	checks := []struct {
		name    string
		cluster int64
		auth    string
		want    int
	}{
		{"missing", 3, "", 401},
		{"garbage", 3, "garbage", 401},
		{"wrong key", 3, relayAuth(otherKey, 3, now), 401},
		{"wrong cluster", 3, relayAuth(testKey, 4, now), 401},
		{"stale", 3, relayAuth(testKey, 3, now.Add(-2*time.Minute)), 401},
		{"future", 3, relayAuth(testKey, 3, now.Add(2*time.Minute)), 401},
		{"no session", 9, relayAuth(testKey, 9, now), 503},
		{"valid", 3, relayAuth(testKey, 3, now), 101},
	}
	for _, c := range checks {
		if got := dial(c.cluster, c.auth); got != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, got)
		}
	}
}

func TestVerifyRelayAuth(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := relayAuth(testKey, 42, now)
	if !verifyRelayAuth(testKey, 42, good, now.Add(59*time.Second)) {
		t.Fatal("valid header rejected")
	}
	if verifyRelayAuth(testKey, 42, good, now.Add(61*time.Second)) {
		t.Fatal("expired header accepted")
	}
	if verifyRelayAuth(testKey[:16], 42, good, now) {
		t.Fatal("short key accepted")
	}
	ts, sig, _ := strings.Cut(good, ":")
	if verifyRelayAuth(testKey, 42, ts+":"+sig[:len(sig)-2]+"AA", now) {
		t.Fatal("tampered mac accepted")
	}
	if verifyRelayAuth(testKey, 42, strconv.FormatInt(now.Unix()+1, 10)+":"+sig, now) {
		t.Fatal("tampered timestamp accepted")
	}
}

// Test 7a: reconnect replaces the session; callbacks are ordered.
func TestReconnectReplacesSession(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	a1 := mustDialFakeAgent(t, th.srv.URL, "tok-5")
	eventually(t, "first connect", func() bool { return len(th.events.snapshot()) == 1 })
	a2 := mustDialFakeAgent(t, th.srv.URL, "tok-5")
	if a1.sessionID == a2.sessionID || a1.secret == a2.secret {
		t.Fatal("reconnect reused session id or secret")
	}
	if !closedWithin(a1.mux.CloseChan(), 5*time.Second) {
		t.Fatal("old session not closed")
	}
	eventually(t, "second connect", func() bool { return len(th.events.snapshot()) == 3 })
	want := []string{"connect 5 " + a1.sessionID, "disconnect 5 " + a1.sessionID, "connect 5 " + a2.sessionID}
	if got := th.events.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", got, want)
	}
	cfg, sid, err := th.hub.RESTConfig(context.Background(), 5)
	if err != nil || sid != a2.sessionID {
		t.Fatalf("sid %q err %v", sid, err)
	}
	if code, body := get(t, cfg, "/x"); code != 200 || !strings.Contains(body, a2.sessionID) {
		t.Fatalf("got %d %q", code, body)
	}
	if o, _ := reg.Lookup(context.Background(), 5); o.SessionID != a2.sessionID {
		t.Fatalf("registry owner %q", o.SessionID)
	}
}

// Test 7b: a heartbeat returning ErrSuperseded closes the local session.
func TestHeartbeatSupersededClosesOwner(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, func(o *Options) { o.StaleAfter = 300 * time.Millisecond })
	fa := mustDialFakeAgent(t, th.srv.URL, "tok-6")
	eventually(t, "online", online(th.hub, 6))
	time.Sleep(250 * time.Millisecond) // several heartbeats keep the claim fresh
	if ok, _ := th.hub.Online(context.Background(), 6); !ok {
		t.Fatal("heartbeats did not keep the session online")
	}
	_ = reg.Claim(context.Background(), 6, Owner{SessionID: "newer", Addr: "http://10.9.9.9:8080", Secret: "s", SeenAt: time.Now()})
	if !closedWithin(fa.mux.CloseChan(), 5*time.Second) {
		t.Fatal("superseded session not closed")
	}
	eventually(t, "disconnect event", func() bool {
		ev := th.events.snapshot()
		return len(ev) == 2 && ev[1] == "disconnect 6 "+fa.sessionID
	})
	if th.hub.localSession(6) != nil {
		t.Fatal("local session still present")
	}
	if o, _ := reg.Lookup(context.Background(), 6); o.SessionID != "newer" {
		t.Fatalf("release clobbered the newer claim: %q", o.SessionID)
	}
}

// Test 8: no session → ErrOffline; loopback conns with no owner close promptly.
func TestOfflineBehaviour(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	if _, _, err := th.hub.RESTConfig(context.Background(), 1); !errors.Is(err, ErrOffline) {
		t.Fatalf("want ErrOffline, got %v", err)
	}
	if ok, err := th.hub.Online(context.Background(), 1); ok || err != nil {
		t.Fatalf("Online = %v, %v", ok, err)
	}

	// Stale peer claim and a claim pointing at ourselves are both offline.
	_ = reg.Claim(context.Background(), 2, Owner{SessionID: "s", Addr: "http://10.9.9.9:8080", Secret: "x", SeenAt: time.Now().Add(-time.Hour)})
	_ = reg.Claim(context.Background(), 3, Owner{SessionID: "s", Addr: th.srv.URL, Secret: "x", SeenAt: time.Now()})
	for _, id := range []int64{2, 3} {
		if _, _, err := th.hub.RESTConfig(context.Background(), id); !errors.Is(err, ErrOffline) {
			t.Fatalf("cluster %d: want ErrOffline, got %v", id, err)
		}
	}

	fa := mustDialFakeAgent(t, th.srv.URL, "tok-4")
	eventually(t, "online", online(th.hub, 4))
	cfg, _, err := th.hub.RESTConfig(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = fa.mux.Close()
	eventually(t, "offline", func() bool { return !online(th.hub, 4)() })

	c, err := net.Dial("tcp", strings.TrimPrefix(cfg.Host, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("loopback conn not closed: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("loopback conn closed after %v", d)
	}
}

// An agent GoAway (graceful shutdown) stops new routing to that session
// while in-flight streams keep working.
func TestAgentGoAwayDrains(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	fa := mustDialFakeAgent(t, th.srv.URL, "tok-12")
	eventually(t, "online", online(th.hub, 12))
	cfg, _, err := th.hub.RESTConfig(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := net.Dial("tcp", strings.TrimPrefix(cfg.Host, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer inflight.Close()
	eventually(t, "stream opened", func() bool { return fa.mux.NumStreams() == 1 })
	_ = fa.mux.GoAway()
	eventually(t, "draining", func() bool {
		c, err := net.Dial("tcp", strings.TrimPrefix(cfg.Host, "http://"))
		if err == nil {
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = c.Read(make([]byte, 1))
			c.Close()
		}
		return !online(th.hub, 12)()
	})
	_ = inflight.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(inflight, "GET /inflight HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+cfg.BearerToken+"\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(inflight)
	if !strings.Contains(string(b), "path=/inflight") {
		t.Fatalf("in-flight stream broken by GoAway: %q", b)
	}
}

// Many concurrent requests share one session.
func TestConcurrentRequests(t *testing.T) {
	reg := NewMemoryRegistry()
	a := newTestHub(t, reg, nil)
	b := newTestHub(t, reg, nil)
	mustDialFakeAgent(t, a.srv.URL, "tok-13")
	eventually(t, "online", online(b.hub, 13))
	for _, h := range []*Hub{a.hub, b.hub} {
		cfg, sid, err := h.RESTConfig(context.Background(), 13)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Timeout: 10 * time.Second}
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/r/%d", cfg.Host, i), nil)
				req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
				resp, err := client.Do(req)
				if err != nil {
					errs <- err
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if want := fmt.Sprintf("session=%s path=/r/%d", sid, i); string(body) != want {
					errs <- fmt.Errorf("got %q want %q", body, want)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}
}

func TestDisconnectAndClose(t *testing.T) {
	reg := NewMemoryRegistry()
	th := newTestHub(t, reg, nil)
	fa := mustDialFakeAgent(t, th.srv.URL, "tok-8")
	eventually(t, "online", online(th.hub, 8))
	if _, _, err := th.hub.RESTConfig(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	th.hub.Disconnect(8)
	if !closedWithin(fa.mux.CloseChan(), 5*time.Second) {
		t.Fatal("Disconnect did not close the session")
	}
	eventually(t, "released", func() bool {
		_, err := reg.Lookup(context.Background(), 8)
		return errors.Is(err, ErrOffline)
	})
	th.hub.mu.Lock()
	_, hasListener := th.hub.listeners[8]
	th.hub.mu.Unlock()
	if hasListener {
		t.Fatal("listener kept after Disconnect")
	}

	fb := mustDialFakeAgent(t, th.srv.URL, "tok-9")
	eventually(t, "online", online(th.hub, 9))
	if err := th.hub.Close(); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(fb.mux.CloseChan(), 5*time.Second) {
		t.Fatal("Close did not close the session")
	}
	if _, resp, err := dialFakeAgent(t, th.srv.URL, "tok-9"); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connect after Close: %v %v", resp, err)
	}
}

func TestMemoryRegistry(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRegistry()
	if _, err := r.Lookup(ctx, 1); !errors.Is(err, ErrOffline) {
		t.Fatal(err)
	}
	if err := r.Heartbeat(ctx, 1, "a"); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	_ = r.Claim(ctx, 1, Owner{SessionID: "a", Secret: "s"})
	_ = r.Claim(ctx, 1, Owner{SessionID: "b", Secret: "t"})
	if err := r.Heartbeat(ctx, 1, "a"); !errors.Is(err, ErrSuperseded) {
		t.Fatal("old session heartbeat accepted")
	}
	if err := r.Heartbeat(ctx, 1, "b"); err != nil {
		t.Fatal(err)
	}
	_ = r.Release(ctx, 1, "a")
	if o, err := r.Lookup(ctx, 1); err != nil || o.SessionID != "b" || o.SeenAt.IsZero() {
		t.Fatalf("%+v %v", o, err)
	}
	_ = r.Release(ctx, 1, "b")
	if _, err := r.Lookup(ctx, 1); !errors.Is(err, ErrOffline) {
		t.Fatal("release did not remove the claim")
	}
}

func TestSelfAddrFromEnv(t *testing.T) {
	t.Setenv("MATRIX_POD_IP", "")
	if got := SelfAddrFromEnv(":8080"); got != "" {
		t.Fatalf("unset: %q", got)
	}
	t.Setenv("MATRIX_POD_IP", "10.1.2.3")
	for in, want := range map[string]string{
		":8080":        "http://10.1.2.3:8080",
		"0.0.0.0:9090": "http://10.1.2.3:9090",
		"":             "http://10.1.2.3:8080",
		"7000":         "http://10.1.2.3:7000",
	} {
		if got := SelfAddrFromEnv(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	t.Setenv("MATRIX_POD_IP", "fd00::5")
	if got := SelfAddrFromEnv(":8080"); got != "http://[fd00::5]:8080" {
		t.Fatalf("ipv6: %q", got)
	}
}
