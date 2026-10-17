package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

type fakeStore struct {
	mu       sync.Mutex
	token    string
	secret   string
	online   map[int64]bool
	finished map[int64]string
	output   map[int64]string
}

func (f *fakeStore) validToken(_ context.Context, p string) (*EnrollToken, error) {
	if p != f.token {
		return nil, errors.New("bad token")
	}
	return &EnrollToken{}, nil
}
func (f *fakeStore) enroll(_ context.Context, p string, h proto.Hello, _ string) (*Host, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p != f.token {
		return nil, "", errors.New("bad token")
	}
	f.token = "" // one-time
	f.secret = "s3cret"
	return &Host{ID: 7, Name: h.Facts.Hostname}, f.secret, nil
}
func (f *fakeStore) authAgent(_ context.Context, id int64, s string) error {
	if id != 7 || s != f.secret {
		return errors.New("bad")
	}
	return nil
}
func (f *fakeStore) markOnline(_ context.Context, id int64, _ proto.Hello) error {
	f.mu.Lock()
	f.online[id] = true
	f.mu.Unlock()
	return nil
}
func (f *fakeStore) heartbeat(context.Context, int64, *proto.Facts) error { return nil }
func (f *fakeStore) markOffline(_ context.Context, id int64) error {
	f.mu.Lock()
	f.online[id] = false
	f.mu.Unlock()
	return nil
}
func (f *fakeStore) finishTask(_ context.Context, id int64, st string, _ *int, out, _ string) error {
	f.mu.Lock()
	f.finished[id], f.output[id] = st, out
	f.mu.Unlock()
	return nil
}

func send(t *testing.T, ws *websocket.Conn, typ string, v any) {
	b, _ := proto.Encode(typ, v)
	if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatal(err)
	}
}

func TestEnrolReconnectExec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fs := &fakeStore{token: "tok", online: map[int64]bool{}, finished: map[int64]string{}, output: map[int64]string{}}
	hub := newHub(fs, slog.Default())
	r := gin.New()
	r.GET(proto.WSPath, hub.ServeWS)
	srv := httptest.NewServer(r)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + proto.WSPath

	// bad token rejected before upgrade
	if _, resp, err := websocket.DefaultDialer.Dial(url+"?token=nope", nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
	// enrol
	ws, _, err := websocket.DefaultDialer.Dial(url+"?token=tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, ws, proto.TypeHello, proto.Hello{Facts: proto.Facts{Hostname: "web-1"}})
	var env proto.Envelope
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeWelcome {
		t.Fatalf("welcome: %v %v", env.Type, err)
	}
	var w proto.Welcome
	_ = json.Unmarshal(env.Data, &w)
	if w.HostID != 7 || w.Secret != "s3cret" {
		t.Fatalf("bad welcome %+v", w)
	}
	ws.Close()
	// token is one-time
	if _, _, err := websocket.DefaultDialer.Dial(url+"?token=tok", nil); err == nil {
		t.Fatal("token reuse should fail")
	}
	// reconnect with secret
	hdr := http.Header{"Authorization": {"Bearer s3cret"}, proto.HeaderHostID: {"7"}}
	ws, _, err = websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	send(t, ws, proto.TypeHello, proto.Hello{})
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeWelcome {
		t.Fatal("no welcome on reconnect")
	}
	for i := 0; i < 50 && !hub.Connected(7); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if err := hub.Dispatch(7, 100, proto.TypeExec, proto.Exec{TaskID: 100, Command: "echo hi", TimeoutSec: 5}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := hub.Dispatch(8, 101, proto.TypeExec, proto.Exec{TaskID: 101}, time.Second); err != errOffline {
		t.Fatalf("want offline, got %v", err)
	}
	_, ch, cancel, ok := hub.Subscribe(100)
	if !ok {
		t.Fatal("task should be running")
	}
	defer cancel()
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeExec {
		t.Fatalf("agent should get exec, got %v %v", env.Type, err)
	}
	send(t, ws, proto.TypeExecOutput, proto.ExecOutput{TaskID: 100, Stream: "stdout", Data: "hi\n"})
	send(t, ws, proto.TypeExecResult, proto.ExecResult{TaskID: 100, ExitCode: 0})
	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Data != "hi\n" {
		t.Fatalf("events %+v", got)
	}
	_ = hub.Wait(context.Background(), 100)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.finished[100] != StatusSuccess || fs.output[100] != "hi\n" || !fs.online[7] {
		t.Fatalf("store state %+v %+v", fs.finished, fs.output)
	}
}

func TestRoutesAndHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := &Module{hub: newHub(&fakeStore{}, slog.Default())}
	r := gin.New()
	m.mount(r.Group("/api/v1"), r.Group("/api/v1")) // panics on route conflicts
	if got := uniqIDs([]int64{3, 3, 0, 1}); len(got) != 2 {
		t.Fatalf("uniq %v", got)
	}
	if normTimeout(0) != defaultTimeout || normTimeout(99999) != maxTimeout {
		t.Fatal("timeout normalisation")
	}
	if primaryIP([]string{"127.0.0.1", "fe80::1", "10.0.0.5"}, "x") != "10.0.0.5" {
		t.Fatal("primaryIP")
	}
}
