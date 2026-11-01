package tunnel

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"k8s.io/client-go/rest"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
)

type clusterListener struct {
	ln   net.Listener
	port int
}

// RESTConfig returns a new rest.Config pointing at this replica's loopback
// listener for the cluster. sessionID changes on every reconnect so callers
// can key client caches on it. ErrOffline when no live owner.
//
// Timeout is 30s per the contract; callers that run watches, log follows or
// exec should clear it on their copy.
func (h *Hub) RESTConfig(ctx context.Context, clusterID int64) (*rest.Config, string, error) {
	sessionID, secret, err := h.resolve(ctx, clusterID)
	if err != nil {
		return nil, "", err
	}
	port, err := h.ensureListener(clusterID)
	if err != nil {
		return nil, "", err
	}
	return &rest.Config{
		Host:        "http://127.0.0.1:" + strconv.Itoa(port),
		BearerToken: secret,
		Timeout:     30 * time.Second,
		QPS:         50,
		Burst:       100,
		UserAgent:   "xingshu-matrix",
	}, sessionID, nil
}

// Online reports whether a live owner (local or on a peer) exists.
func (h *Hub) Online(ctx context.Context, clusterID int64) (bool, error) {
	_, _, err := h.resolve(ctx, clusterID)
	if errors.Is(err, ErrOffline) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// resolve returns the live session id and secret for the cluster.
func (h *Hub) resolve(ctx context.Context, clusterID int64) (string, string, error) {
	if s := h.localSession(clusterID); s != nil {
		return s.id, s.secret, nil
	}
	o, err := h.remoteOwner(ctx, clusterID)
	if err != nil {
		return "", "", err
	}
	return o.SessionID, o.Secret, nil
}

// remoteOwner returns a live owner on another replica that this replica can
// relay to, or ErrOffline. Registry errors other than ErrOffline pass through.
func (h *Hub) remoteOwner(ctx context.Context, clusterID int64) (Owner, error) {
	o, err := h.o.Registry.Lookup(ctx, clusterID)
	if err != nil {
		return Owner{}, err
	}
	if o.SessionID == "" || o.Secret == "" || time.Since(o.SeenAt) > h.o.StaleAfter {
		return Owner{}, ErrOffline
	}
	// Our own claim without a local session is stale; an empty Addr means
	// the owner cannot be relayed to; and we cannot sign without a key.
	if o.Addr == "" || (h.o.SelfAddr != "" && o.Addr == h.o.SelfAddr) || len(h.o.RelayKey) < minRelayKeyLen {
		return Owner{}, ErrOffline
	}
	return o, nil
}

func (h *Hub) ensureListener(clusterID int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrOffline
	}
	if l := h.listeners[clusterID]; l != nil {
		return l.port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l := &clusterListener{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	h.listeners[clusterID] = l
	h.wg.Add(1)
	go h.acceptLoop(clusterID, l)
	return l.port, nil
}

func (h *Hub) acceptLoop(clusterID int64, l *clusterListener) {
	defer h.wg.Done()
	delay := 5 * time.Millisecond
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(delay)
				continue
			}
			h.mu.Lock()
			current := h.listeners[clusterID] == l
			h.mu.Unlock()
			if !current {
				return
			}
			time.Sleep(delay)
			if delay < time.Second {
				delay *= 2
			}
			continue
		}
		delay = 5 * time.Millisecond
		if !h.track(clusterID, c) {
			_ = c.Close()
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer h.untrack(clusterID, c)
			h.handleLocal(clusterID, c)
		}()
	}
}

// handleLocal pipes one loopback connection to the cluster's agent, directly
// or via the owning replica. With no live owner the conn is closed at once.
func (h *Hub) handleLocal(clusterID int64, c net.Conn) {
	if s := h.localSession(clusterID); s != nil {
		stream, err := s.open()
		if err != nil {
			_ = c.Close()
			return
		}
		wire.Pipe(c, stream)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayDialTimeout)
	defer cancel()
	o, err := h.remoteOwner(ctx, clusterID)
	if err != nil {
		_ = c.Close()
		return
	}
	rc, err := h.dialRelay(ctx, o.Addr, clusterID)
	if err != nil {
		h.log.Warn("cluster agent relay dial failed", "cluster_id", clusterID, "owner", o.Addr, "error", err)
		_ = c.Close()
		return
	}
	if !h.track(clusterID, rc) {
		_ = rc.Close()
		_ = c.Close()
		return
	}
	defer h.untrack(clusterID, rc)
	wire.Pipe(c, rc)
}
