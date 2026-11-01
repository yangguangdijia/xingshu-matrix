package cluster

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel"
)

type fakeAgentHub struct {
	session      string
	err          error
	online       bool
	disconnected []int64
}

func (f *fakeAgentHub) RESTConfig(context.Context, int64) (*rest.Config, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return &rest.Config{Host: "http://127.0.0.1:1", BearerToken: "session-secret"}, f.session, nil
}
func (f *fakeAgentHub) Online(context.Context, int64) (bool, error) { return f.online, nil }
func (f *fakeAgentHub) Disconnect(id int64)                         { f.disconnected = append(f.disconnected, id) }

func TestAgentClientsFailClosedWhenOffline(t *testing.T) {
	m := New()
	cl := &Cluster{ID: 3, ConnectMode: app.ClusterConnectAgent}
	if _, err := m.kube(context.Background(), cl); !errors.Is(err, app.ErrClusterOffline) {
		t.Fatalf("nil hub: want ErrClusterOffline, got %v", err)
	}
	m.hub = &fakeAgentHub{err: tunnel.ErrOffline}
	if _, err := m.kube(context.Background(), cl); !errors.Is(err, app.ErrClusterOffline) {
		t.Fatalf("offline hub: want ErrClusterOffline, got %v", err)
	}
}

func TestAgentClientsCacheBySession(t *testing.T) {
	hub := &fakeAgentHub{session: "s1"}
	m := New()
	m.hub = hub
	cl := &Cluster{ID: 3, ConnectMode: app.ClusterConnectAgent}
	first, err := m.kube(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := m.kube(context.Background(), cl)
	if first != again {
		t.Fatal("same session should reuse clients")
	}
	if first.rest.Host != "http://127.0.0.1:1" || first.rest.BearerToken != "session-secret" {
		t.Fatalf("unexpected rest config %+v", first.rest)
	}
	hub.session = "s2"
	rebuilt, _ := m.kube(context.Background(), cl)
	if rebuilt == first {
		t.Fatal("a new tunnel session must rebuild clients")
	}
}

func TestMarkAgentOnlineOnlyTouchesAgentClusters(t *testing.T) {
	m := New()
	agent := &Cluster{ID: 1, ConnectMode: app.ClusterConnectAgent}
	direct := &Cluster{ID: 2, ConnectMode: app.ClusterConnectKubeconfig}
	m.markAgentOnline(context.Background(), agent, direct)
	if agent.AgentOnline == nil || *agent.AgentOnline || direct.AgentOnline != nil {
		t.Fatalf("nil hub must report offline for agent only: %v %v", agent.AgentOnline, direct.AgentOnline)
	}
	m.hub = &fakeAgentHub{online: true}
	m.markAgentOnline(context.Background(), agent)
	if !*agent.AgentOnline {
		t.Fatal("online hub should mark agent online")
	}
}

func TestAgentSessionSecretAADIsPerCluster(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.EncryptBytes([]byte("secret"), agentSecretAAD(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DecryptBytes(enc, agentSecretAAD(2)); err == nil {
		t.Fatal("a session secret must not decrypt under another cluster's AAD")
	}
	if got, err := c.DecryptBytes(enc, agentSecretAAD(1)); err != nil || string(got) != "secret" {
		t.Fatalf("round trip failed: %q %v", got, err)
	}
}

// Requires MATRIX_TEST_DATABASE_URL pointing at a disposable database; uses an isolated schema.
func TestAgentAuthenticationAndRegistrySQL(t *testing.T) {
	url := integrationTestDatabaseURL()
	if url == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run PostgreSQL agent integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	pool, cleanup := integrationIsolatedPool(t, ctx, url)
	defer cleanup()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := crypto.New(key)
	m := New()
	m.d = &app.Deps{DB: pool, Crypto: cipher}
	m.log = slog.Default()

	if _, err := pool.Exec(ctx, `INSERT INTO cluster_clusters (name,env,kubeconfig,connect_mode) VALUES ('bad','prod','x','agent')`); err == nil {
		t.Fatal("agent clusters must not store a kubeconfig")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cluster_clusters (name,env,kubeconfig,connect_mode) VALUES ('bad2','prod','','kubeconfig')`); err == nil {
		t.Fatal("kubeconfig clusters must store a kubeconfig")
	}
	token, hash, _ := newAgentToken()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO cluster_clusters (name,env,kubeconfig,connect_mode,agent_token_hash)
		VALUES ('edge','prod','','agent',$1) RETURNING id`, hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got, err := m.authenticateAgent(ctx, tunnel.AgentInfo{Token: token, Fingerprint: "uid-a", Version: "v1"}); err != nil || got != id {
		t.Fatalf("first connect: %d %v", got, err)
	}
	if _, err := m.authenticateAgent(ctx, tunnel.AgentInfo{Token: token, Fingerprint: "uid-b"}); err == nil {
		t.Fatal("pinned fingerprint must reject a different cluster")
	}
	other, _, _ := newAgentToken()
	if _, err := m.authenticateAgent(ctx, tunnel.AgentInfo{Token: other, Fingerprint: "uid-a"}); err == nil {
		t.Fatal("unknown token must be rejected")
	}

	reg := pgRegistry{db: pool, cipher: cipher}
	if _, err := reg.Lookup(ctx, id); !errors.Is(err, tunnel.ErrOffline) {
		t.Fatalf("no session should be offline: %v", err)
	}
	if err := reg.Claim(ctx, id, tunnel.Owner{SessionID: "s1", Addr: "http://10.0.0.1:8080", Secret: "sec1"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Claim(ctx, id, tunnel.Owner{SessionID: "s2", Addr: "http://10.0.0.2:8080", Secret: "sec2"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Heartbeat(ctx, id, "s1"); !errors.Is(err, tunnel.ErrSuperseded) {
		t.Fatalf("old session heartbeat: %v", err)
	}
	owner, err := reg.Lookup(ctx, id)
	if err != nil || owner.SessionID != "s2" || owner.Secret != "sec2" || owner.Addr != "http://10.0.0.2:8080" {
		t.Fatalf("lookup: %+v %v", owner, err)
	}
	var stored string
	_ = pool.QueryRow(ctx, `SELECT secret_enc FROM cluster_agent_sessions WHERE cluster_id=$1`, id).Scan(&stored)
	if stored == "sec2" || !crypto.IsEncrypted(stored) {
		t.Fatal("session secret must be stored encrypted")
	}
	if err := reg.Release(ctx, id, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Lookup(ctx, id); err != nil {
		t.Fatal("releasing a stale session must not remove the current one")
	}
	k1, err := relayKey(ctx, pool, cipher)
	if err != nil || len(k1) != 32 {
		t.Fatalf("relay key: %d %v", len(k1), err)
	}
	k2, _ := relayKey(ctx, pool, cipher)
	if string(k1) != string(k2) {
		t.Fatal("relay key must be shared, not regenerated")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cluster_clusters WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Lookup(ctx, id); !errors.Is(err, tunnel.ErrOffline) {
		t.Fatal("deleting a cluster must remove its session")
	}
}
