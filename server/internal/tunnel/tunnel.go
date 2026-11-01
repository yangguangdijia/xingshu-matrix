// Package tunnel lets the platform reach member-cluster kube-apiservers through
// reverse tunnels opened by matrix-cluster-agent.
//
// The agent dials ConnectPath over WebSocket. The hub authenticates it, replies
// with a per-session id and secret, and runs yamux over the websocket with the
// hub as the yamux client: every platform request to the cluster is a new
// stream carrying plain HTTP/1.1, which the agent authenticates with the
// session secret and reverse-proxies to its in-cluster kube-apiserver.
//
// Locally, each cluster gets a loopback TCP listener; RESTConfig points
// client-go at it. Accepted connections are piped to a stream on this replica's
// session, or, when another replica owns the session (per Registry), relayed
// to that replica's RelayPath over an HMAC-authenticated WebSocket.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Routes, relative to the /api/v1 group.
const (
	ConnectPath = "/cluster-agent/connect"
	RelayPath   = "/cluster-agent/relay"
)

var (
	ErrOffline    = errors.New("cluster agent is not connected")
	ErrSuperseded = errors.New("cluster agent session was superseded")
)

const (
	defaultStaleAfter   = 45 * time.Second
	maxHeartbeat        = 15 * time.Second
	relaySkew           = 60 * time.Second
	minRelayKeyLen      = 32
	registryOpTimeout   = 10 * time.Second
	sessionWaitTimeout  = 10 * time.Second
	relayDialTimeout    = 10 * time.Second
	maxAgentHeaderValue = 256
)

// AgentInfo describes a connecting agent. Token is only populated for the
// Authenticate callback; it is cleared before OnConnect.
type AgentInfo struct {
	Token, Fingerprint, Version string
	RemoteAddr                  string
}

// Authenticate maps an agent's credentials to a cluster ID, or returns an error
// (→ HTTP 401, generic message).
type Authenticate func(ctx context.Context, info AgentInfo) (clusterID int64, err error)

// Owner identifies the replica holding a cluster's session.
type Owner struct {
	SessionID, Addr, Secret string
	SeenAt                  time.Time
}

// Registry stores which replica owns each cluster's session. Implementations
// must store Secret encrypted.
type Registry interface {
	Claim(ctx context.Context, clusterID int64, o Owner) error              // upsert; newest claim wins
	Heartbeat(ctx context.Context, clusterID int64, sessionID string) error // ErrSuperseded if not current
	Release(ctx context.Context, clusterID int64, sessionID string) error   // no-op if not current
	Lookup(ctx context.Context, clusterID int64) (Owner, error)             // ErrOffline if none
}

// Options configures a Hub.
type Options struct {
	Authenticate Authenticate
	Registry     Registry
	SelfAddr     string // this replica's relay base URL, e.g. "http://10.0.0.5:8080"; empty = no relay (single replica)
	RelayKey     []byte // >= 32 bytes; required when SelfAddr != ""
	OnConnect    func(clusterID int64, info AgentInfo, sessionID string)
	OnDisconnect func(clusterID int64, sessionID string)
	Logger       *slog.Logger
	StaleAfter   time.Duration // default 45s
}

// Hub owns this replica's agent sessions and per-cluster loopback listeners.
type Hub struct {
	o         Options
	log       *slog.Logger
	heartbeat time.Duration
	upgrader  websocket.Upgrader
	dialer    *websocket.Dialer

	mu        sync.Mutex
	closed    bool
	sessions  map[int64]*session
	listeners map[int64]*clusterListener
	conns     map[int64]map[io.Closer]struct{} // loopback/relay conns per cluster
	wg        sync.WaitGroup
}

// NewHub validates o and returns a Hub.
func NewHub(o Options) (*Hub, error) {
	if o.Authenticate == nil {
		return nil, errors.New("tunnel: Authenticate is required")
	}
	if o.Registry == nil {
		return nil, errors.New("tunnel: Registry is required")
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = defaultStaleAfter
	}
	o.SelfAddr = strings.TrimRight(strings.TrimSpace(o.SelfAddr), "/")
	if o.SelfAddr != "" {
		u, err := url.Parse(o.SelfAddr)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("tunnel: SelfAddr must be an http(s) base URL")
		}
		if len(o.RelayKey) < minRelayKeyLen {
			return nil, fmt.Errorf("tunnel: RelayKey must be at least %d bytes when SelfAddr is set", minRelayKeyLen)
		}
	} else if len(o.RelayKey) > 0 && len(o.RelayKey) < minRelayKeyLen {
		return nil, fmt.Errorf("tunnel: RelayKey must be at least %d bytes", minRelayKeyLen)
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "cluster-tunnel")
	hb := o.StaleAfter / 3
	if hb > maxHeartbeat {
		hb = maxHeartbeat
	}
	return &Hub{
		o:         o,
		log:       log,
		heartbeat: hb,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  32 << 10,
			// Default CheckOrigin: rejects cross-origin browser requests;
			// agents and relays send no Origin header.
		},
		dialer: &websocket.Dialer{
			HandshakeTimeout: relayDialTimeout,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  32 << 10,
		},
		sessions:  map[int64]*session{},
		listeners: map[int64]*clusterListener{},
		conns:     map[int64]map[io.Closer]struct{}{},
	}, nil
}

// SelfAddrFromEnv returns "http://<MATRIX_POD_IP>:<port of listen addr>" or ""
// when MATRIX_POD_IP is unset.
func SelfAddrFromEnv(listen string) string {
	ip := strings.TrimSpace(os.Getenv("MATRIX_POD_IP"))
	if ip == "" {
		return ""
	}
	port := "8080"
	if l := strings.TrimSpace(listen); l != "" {
		if _, p, err := net.SplitHostPort(l); err == nil && p != "" {
			port = p
		} else if !strings.Contains(l, ":") {
			port = l
		}
	}
	return "http://" + net.JoinHostPort(ip, port)
}

// safe runs an integrator callback, containing panics.
func (h *Hub) safe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("tunnel callback panicked", "callback", name, "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// track registers c for cluster-scoped teardown; false if the hub is closed.
func (h *Hub) track(clusterID int64, c io.Closer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	set := h.conns[clusterID]
	if set == nil {
		set = map[io.Closer]struct{}{}
		h.conns[clusterID] = set
	}
	set[c] = struct{}{}
	return true
}

func (h *Hub) untrack(clusterID int64, c io.Closer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.conns[clusterID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.conns, clusterID)
		}
	}
}

// Disconnect closes this replica's session, listener and in-flight
// connections for the cluster. The caller deletes the registry row.
func (h *Hub) Disconnect(clusterID int64) {
	h.mu.Lock()
	s := h.sessions[clusterID]
	l := h.listeners[clusterID]
	delete(h.listeners, clusterID)
	conns := h.conns[clusterID]
	delete(h.conns, clusterID)
	h.mu.Unlock()
	if l != nil {
		_ = l.ln.Close()
	}
	for c := range conns {
		_ = c.Close()
	}
	if s != nil {
		_ = s.mux.Close()
	}
}

// Close shuts down all sessions, listeners and connections.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	sessions := h.sessions
	listeners := h.listeners
	conns := h.conns
	h.sessions = map[int64]*session{}
	h.listeners = map[int64]*clusterListener{}
	h.conns = map[int64]map[io.Closer]struct{}{}
	h.mu.Unlock()

	for _, l := range listeners {
		_ = l.ln.Close()
	}
	for _, set := range conns {
		for c := range set {
			_ = c.Close()
		}
	}
	for _, s := range sessions {
		_ = s.mux.Close()
	}
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(sessionWaitTimeout):
		return errors.New("tunnel: timed out waiting for sessions to close")
	}
}
