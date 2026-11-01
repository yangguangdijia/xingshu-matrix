package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
)

type session struct {
	clusterID int64
	id        string
	secret    string
	info      AgentInfo
	mux       *yamux.Session
	done      chan struct{} // closed after Release and OnDisconnect ran
	draining  atomic.Bool   // agent sent GoAway: no new streams
}

// open starts a stream to the agent. A GoAway from the agent (graceful
// shutdown) marks the session draining so new requests stop selecting it
// while in-flight streams finish.
func (s *session) open() (net.Conn, error) {
	c, err := s.mux.Open()
	if errors.Is(err, yamux.ErrRemoteGoAway) {
		s.draining.Store(true)
	}
	return c, err
}

// ServeAgent handles the agent's WebSocket connect. It authenticates through
// Options.Authenticate and blocks for the life of the session.
func (h *Hub) ServeAgent(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	token := wire.BearerToken(r.Header)
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	info := AgentInfo{
		Token:       token,
		Fingerprint: headerValue(r, wire.HeaderClusterFingerprint),
		Version:     headerValue(r, wire.HeaderAgentVersion),
		RemoteAddr:  r.RemoteAddr,
	}
	actx, cancel := context.WithTimeout(r.Context(), registryOpTimeout)
	clusterID, err := h.o.Authenticate(actx, info)
	cancel()
	if err != nil || clusterID <= 0 {
		// The error may describe the token; do not log it.
		h.log.Warn("cluster agent authentication failed", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	info.Token = ""

	sessionID, err := randomToken(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	secret, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()

	hdr := http.Header{}
	hdr.Set(wire.HeaderSessionID, sessionID)
	hdr.Set(wire.HeaderSessionSecret, secret)
	ws, err := h.upgrader.Upgrade(w, r, hdr)
	if err != nil {
		return // Upgrade already replied
	}
	conn := wire.NewConn(ws)
	mux, err := yamux.Client(conn, wire.YamuxConfig(h.log))
	if err != nil {
		_ = conn.Close()
		return
	}
	s := &session{clusterID: clusterID, id: sessionID, secret: secret, info: info, mux: mux, done: make(chan struct{})}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = mux.Close()
		return
	}
	old := h.sessions[clusterID]
	h.sessions[clusterID] = s
	h.mu.Unlock()

	if old != nil {
		// Let the old session release its claim and report its disconnect
		// before the new one claims, so callbacks are ordered.
		_ = old.mux.Close()
		select {
		case <-old.done:
		case <-time.After(sessionWaitTimeout):
		}
	}

	cctx, cancel := context.WithTimeout(context.Background(), registryOpTimeout)
	err = h.o.Registry.Claim(cctx, clusterID, Owner{SessionID: sessionID, Addr: h.o.SelfAddr, Secret: secret, SeenAt: time.Now()})
	cancel()
	if err != nil {
		h.log.Error("cluster agent claim failed", "cluster_id", clusterID, "error", err)
		h.mu.Lock()
		if h.sessions[clusterID] == s {
			delete(h.sessions, clusterID)
		}
		h.mu.Unlock()
		_ = mux.Close()
		close(s.done)
		return
	}
	if h.o.OnConnect != nil {
		h.safe("OnConnect", func() { h.o.OnConnect(clusterID, info, sessionID) })
	}
	h.log.Info("cluster agent connected", "cluster_id", clusterID, "session_id", sessionID,
		"agent_version", info.Version, "remote", info.RemoteAddr)

	h.runSession(s)
	h.finishSession(s)
}

// runSession heartbeats the claim until the session closes or is superseded.
func (h *Hub) runSession(s *session) {
	t := time.NewTicker(h.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-s.mux.CloseChan():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), registryOpTimeout)
			err := h.o.Registry.Heartbeat(ctx, s.clusterID, s.id)
			cancel()
			switch {
			case err == nil:
			case errors.Is(err, ErrSuperseded):
				h.log.Info("cluster agent session superseded", "cluster_id", s.clusterID, "session_id", s.id)
				_ = s.mux.Close()
				return
			default:
				// Transient registry failure: keep serving locally; peers
				// will see the claim go stale until heartbeats recover.
				h.log.Warn("cluster agent heartbeat failed", "cluster_id", s.clusterID, "session_id", s.id, "error", err)
			}
		}
	}
}

func (h *Hub) finishSession(s *session) {
	_ = s.mux.Close()
	h.mu.Lock()
	if h.sessions[s.clusterID] == s {
		delete(h.sessions, s.clusterID)
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), registryOpTimeout)
	if err := h.o.Registry.Release(ctx, s.clusterID, s.id); err != nil {
		h.log.Warn("cluster agent release failed", "cluster_id", s.clusterID, "session_id", s.id, "error", err)
	}
	cancel()
	if h.o.OnDisconnect != nil {
		h.safe("OnDisconnect", func() { h.o.OnDisconnect(s.clusterID, s.id) })
	}
	h.log.Info("cluster agent disconnected", "cluster_id", s.clusterID, "session_id", s.id)
	close(s.done)
}

func (h *Hub) localSession(clusterID int64) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[clusterID]
	if s == nil || s.mux.IsClosed() || s.draining.Load() {
		return nil
	}
	return s
}

func headerValue(r *http.Request, name string) string {
	v := strings.TrimSpace(r.Header.Get(name))
	if len(v) > maxAgentHeaderValue {
		v = v[:maxAgentHeaderValue]
	}
	return v
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
