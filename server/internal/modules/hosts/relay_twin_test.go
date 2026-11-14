package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules/hosts/proto"
)

// memOwnerRegistry is an in-memory ownerRegistry shared by the twin replicas.
// Freshness is evaluated against a controllable clock, like the PG registry
// evaluates it against the database clock.
type memOwnerRegistry struct {
	mu     sync.Mutex
	now    time.Time
	stale  time.Duration
	owners map[int64]memClaim
	renews int
}

type memClaim struct {
	owner agentOwner
	seen  time.Time
}

func newMemOwnerRegistry() *memOwnerRegistry {
	return &memOwnerRegistry{now: time.Unix(1_700_000_000, 0), stale: defaultStale, owners: map[int64]memClaim{}}
}

func (r *memOwnerRegistry) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *memOwnerRegistry) claim(_ context.Context, hostID int64, o agentOwner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[hostID] = memClaim{owner: agentOwner{SessionID: o.SessionID, Addr: o.Addr}, seen: r.now}
	return nil
}

func (r *memOwnerRegistry) renew(_ context.Context, hostID int64, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	c, ok := r.owners[hostID]
	if !ok || c.owner.SessionID != sessionID {
		return errSuperseded
	}
	c.seen = r.now
	r.owners[hostID] = c
	return nil
}

func (r *memOwnerRegistry) release(_ context.Context, hostID int64, sessionID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.owners[hostID]
	if !ok || c.owner.SessionID != sessionID {
		return false, nil
	}
	delete(r.owners, hostID)
	return true, nil
}

func (r *memOwnerRegistry) lookup(_ context.Context, hostID int64) (agentOwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.owners[hostID]
	if !ok {
		return agentOwner{}, errOffline
	}
	o := c.owner
	o.Fresh = r.now.Sub(c.seen) < r.stale
	return o, nil
}

func (r *memOwnerRegistry) has(hostID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.owners[hostID]
	return ok
}

type twinReplica struct {
	hub *Hub
	srv *httptest.Server
}

var twinKey = []byte(strings.Repeat("r", minRelayKeyLen))

func newTwinReplica(t *testing.T, st agentStore, reg ownerRegistry, lease time.Duration) *twinReplica {
	t.Helper()
	gin.SetMode(gin.TestMode)
	hub := newHub(st, slog.Default())
	r := gin.New()
	r.GET(proto.WSPath, hub.ServeWS)
	r.POST("/api/v1"+relayPath, hub.ServeRelay)
	srv := httptest.NewServer(r)
	hub.configureRelay(reg, srv.URL, twinKey)
	if lease > 0 {
		hub.relay.lease = lease
	}
	t.Cleanup(func() { hub.Close(); srv.Close() })
	return &twinReplica{hub: hub, srv: srv}
}

func newTwinStore() *fakeStore {
	return &fakeStore{secret: "s3cret", online: map[int64]bool{}, finished: map[int64]string{}, output: map[int64]string{}}
}

// connectTwinAgent connects host 7's agent to the replica and waits for its claim.
func connectTwinAgent(t *testing.T, rep *twinReplica) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(rep.srv.URL, "http") + proto.WSPath
	hdr := map[string][]string{"Authorization": {"Bearer s3cret"}, proto.HeaderHostID: {"7"}}
	ws, _, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	send(t, ws, proto.TypeHello, proto.Hello{AgentVersion: "test"})
	var env proto.Envelope
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeWelcome {
		t.Fatalf("welcome: %v %v", env.Type, err)
	}
	waitFor(t, "agent registered on replica", func() bool { return rep.hub.Connected(7) })
	return ws
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTwinRelayDispatchThroughPeer(t *testing.T) {
	st, reg := newTwinStore(), newMemOwnerRegistry()
	a := newTwinReplica(t, st, reg, 0)
	b := newTwinReplica(t, st, reg, 0)
	ws := connectTwinAgent(t, a)

	if b.hub.Connected(7) || !b.hub.Online(context.Background(), 7) {
		t.Fatal("replica B should see the agent online through A's claim without a local socket")
	}
	if !a.hub.Online(context.Background(), 7) {
		t.Fatal("replica A should report its local agent online")
	}

	if err := b.hub.Dispatch(7, 200, proto.TypeExec, proto.Exec{TaskID: 200, Command: "echo hi", TimeoutSec: 5}, 5*time.Second); err != nil {
		t.Fatalf("dispatch via B: %v", err)
	}
	_, ch, cancel, ok := b.hub.Subscribe(200)
	if !ok {
		t.Fatal("B should track the relayed task")
	}
	defer cancel()

	var env proto.Envelope
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.ReadJSON(&env); err != nil || env.Type != proto.TypeExec {
		t.Fatalf("agent on A should receive exec, got %v %v", env.Type, err)
	}
	var ex proto.Exec
	if err := json.Unmarshal(env.Data, &ex); err != nil || ex.TaskID != 200 || ex.Command != "echo hi" {
		t.Fatalf("relayed exec frame %+v %v", ex, err)
	}
	send(t, ws, proto.TypeExecOutput, proto.ExecOutput{TaskID: 200, Stream: "stdout", Data: "hi from A\n"})
	send(t, ws, proto.TypeExecResult, proto.ExecResult{TaskID: 200, ExitCode: 0})

	var got []string
	for ev := range ch {
		got = append(got, ev.Data)
	}
	if len(got) != 1 || got[0] != "hi from A\n" {
		t.Fatalf("B streamed events %q", got)
	}
	ctx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	if err := b.hub.Wait(ctx, 200); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// The owner (A) persists the result; B finishes without persisting.
	if st.finished[200] != StatusSuccess || st.output[200] != "hi from A\n" {
		t.Fatalf("persisted %q / %q", st.finished[200], st.output[200])
	}
}

func TestTwinOfflineAfterOwnerGone(t *testing.T) {
	t.Run("agent disconnected from A", func(t *testing.T) {
		st, reg := newTwinStore(), newMemOwnerRegistry()
		a := newTwinReplica(t, st, reg, 0)
		b := newTwinReplica(t, st, reg, 0)
		ws := connectTwinAgent(t, a)
		_ = ws.Close()
		waitFor(t, "A to drop the agent", func() bool { return !a.hub.Connected(7) && !reg.has(7) })
		st.mu.Lock()
		online := st.online[7]
		st.mu.Unlock()
		if online {
			t.Fatal("released claim should mark the host offline")
		}
		if b.hub.Online(context.Background(), 7) {
			t.Fatal("B should report the host offline")
		}
		err := b.hub.Dispatch(7, 300, proto.TypeExec, proto.Exec{TaskID: 300, Command: "true", TimeoutSec: 5}, 5*time.Second)
		if !errors.Is(err, errOffline) {
			t.Fatalf("want errOffline, got %v", err)
		}
	})

	t.Run("replica A unreachable", func(t *testing.T) {
		st, reg := newTwinStore(), newMemOwnerRegistry()
		a := newTwinReplica(t, st, reg, 0)
		b := newTwinReplica(t, st, reg, 0)
		connectTwinAgent(t, a)
		// A's listener dies while its claim is still fresh (crash / partition).
		a.srv.Listener.Close()
		a.srv.CloseClientConnections()
		err := b.hub.Dispatch(7, 301, proto.TypeExec, proto.Exec{TaskID: 301, Command: "true", TimeoutSec: 5}, 5*time.Second)
		if !errors.Is(err, errOwnerUnreachable) {
			t.Fatalf("want errOwnerUnreachable, got %v", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "主机") || !strings.Contains(msg, "不可达") {
			t.Fatalf("error should be a Chinese operator message, got %q", msg)
		}
		if _, _, _, ok := b.hub.Subscribe(301); ok {
			t.Fatal("failed relay must not leave a tracked task on B")
		}
	})
}

func TestTwinLeaseExpiry(t *testing.T) {
	t.Run("stale claim is offline", func(t *testing.T) {
		st, reg := newTwinStore(), newMemOwnerRegistry()
		a := newTwinReplica(t, st, reg, time.Hour) // no renewals during the test
		b := newTwinReplica(t, st, reg, 0)
		connectTwinAgent(t, a)
		if !b.hub.Online(context.Background(), 7) {
			t.Fatal("fresh claim should be online")
		}
		reg.advance(defaultStale + time.Second)
		if b.hub.Online(context.Background(), 7) {
			t.Fatal("expired lease should be offline on B")
		}
		err := b.hub.Dispatch(7, 400, proto.TypeExec, proto.Exec{TaskID: 400, Command: "true", TimeoutSec: 5}, 5*time.Second)
		if !errors.Is(err, errOffline) {
			t.Fatalf("want errOffline for expired lease, got %v", err)
		}
		if !a.hub.Online(context.Background(), 7) {
			t.Fatal("A still holds the socket and should report it online")
		}
	})

	t.Run("renewal keeps claim fresh", func(t *testing.T) {
		st, reg := newTwinStore(), newMemOwnerRegistry()
		connectTwinAgent(t, newTwinReplica(t, st, reg, 10*time.Millisecond))
		b := newTwinReplica(t, st, reg, 0)
		reg.advance(defaultStale + time.Second)
		reg.mu.Lock()
		before := reg.renews
		reg.mu.Unlock()
		waitFor(t, "a lease renewal", func() bool { reg.mu.Lock(); defer reg.mu.Unlock(); return reg.renews > before })
		if !b.hub.Online(context.Background(), 7) {
			t.Fatal("renewed claim should be fresh again")
		}
	})

	t.Run("superseded claim closes the socket", func(t *testing.T) {
		st, reg := newTwinStore(), newMemOwnerRegistry()
		a := newTwinReplica(t, st, reg, 10*time.Millisecond)
		connectTwinAgent(t, a)
		// The agent reconnected through another replica: its claim replaces A's.
		_ = reg.claim(context.Background(), 7, agentOwner{SessionID: "other", Addr: "http://other"})
		waitFor(t, "A to close the superseded socket", func() bool { return !a.hub.Connected(7) })
		st.mu.Lock()
		online := st.online[7]
		st.mu.Unlock()
		if !online {
			t.Fatal("A must not mark the host offline when another replica holds the claim")
		}
		if o, err := reg.lookup(context.Background(), 7); err != nil || o.SessionID != "other" {
			t.Fatalf("A must not release the newer claim: %+v %v", o, err)
		}
	})
}
