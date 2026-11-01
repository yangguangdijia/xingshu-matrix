package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
)

// relayAuth builds the X-Matrix-Relay-Auth value "<unix>:<base64url(mac)>",
// mac = HMAC-SHA256(key, "<cluster_id>|<unix>").
func relayAuth(key []byte, clusterID int64, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	return ts + ":" + base64.RawURLEncoding.EncodeToString(relayMAC(key, clusterID, ts))
}

func relayMAC(key []byte, clusterID int64, ts string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strconv.FormatInt(clusterID, 10) + "|" + ts))
	return m.Sum(nil)
}

func verifyRelayAuth(key []byte, clusterID int64, value string, now time.Time) bool {
	if len(key) < minRelayKeyLen {
		return false
	}
	ts, sig, ok := strings.Cut(strings.TrimSpace(value), ":")
	if !ok || ts == "" || sig == "" {
		return false
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	d := now.Sub(time.Unix(unix, 0))
	if d > relaySkew || d < -relaySkew {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, relayMAC(key, clusterID, ts))
}

// dialRelay opens a relay websocket to the owner replica.
func (h *Hub) dialRelay(ctx context.Context, ownerAddr string, clusterID int64) (net.Conn, error) {
	u, err := url.Parse(strings.TrimRight(ownerAddr, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse owner address: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("unsupported owner address scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1" + RelayPath
	u.RawQuery = url.Values{"cluster_id": {strconv.FormatInt(clusterID, 10)}}.Encode()
	hdr := http.Header{}
	hdr.Set(wire.HeaderRelayAuth, relayAuth(h.o.RelayKey, clusterID, time.Now()))
	ws, resp, err := h.dialer.DialContext(ctx, u.String(), hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("relay handshake: HTTP %d", resp.StatusCode)
		}
		return nil, err
	}
	return wire.NewConn(ws), nil
}

// ServeRelay handles a peer replica's relay request: it opens a stream on the
// local session for cluster_id and pipes it to the websocket.
func (h *Hub) ServeRelay(w http.ResponseWriter, r *http.Request) {
	if h.o.SelfAddr == "" || len(h.o.RelayKey) < minRelayKeyLen {
		http.NotFound(w, r)
		return
	}
	clusterID, err := strconv.ParseInt(r.URL.Query().Get("cluster_id"), 10, 64)
	if err != nil || clusterID <= 0 {
		http.Error(w, "invalid cluster_id", http.StatusBadRequest)
		return
	}
	if !verifyRelayAuth(h.o.RelayKey, clusterID, r.Header.Get(wire.HeaderRelayAuth), time.Now()) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	s := h.localSession(clusterID)
	if s == nil {
		http.Error(w, ErrOffline.Error(), http.StatusServiceUnavailable)
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

	stream, err := s.open()
	if err != nil {
		http.Error(w, ErrOffline.Error(), http.StatusServiceUnavailable)
		return
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = stream.Close()
		return
	}
	conn := wire.NewConn(ws)
	if !h.track(clusterID, conn) {
		_ = conn.Close()
		_ = stream.Close()
		return
	}
	defer h.untrack(clusterID, conn)
	wire.Pipe(conn, stream)
}
