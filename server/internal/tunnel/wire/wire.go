// Package wire holds the protocol plumbing shared by the platform-side tunnel
// hub (internal/tunnel) and the in-cluster agent (cmd/matrix-cluster-agent):
// header names, a websocket-backed net.Conn, yamux settings and a
// bidirectional pipe. It carries no policy; authentication lives in callers.
package wire

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Header names used on the connect handshake and relay requests.
const (
	HeaderAgentVersion       = "X-Matrix-Agent-Version"
	HeaderClusterFingerprint = "X-Matrix-Cluster-Fingerprint"
	HeaderSessionID          = "X-Matrix-Session-Id"
	HeaderSessionSecret      = "X-Matrix-Session-Secret"
	HeaderRelayAuth          = "X-Matrix-Relay-Auth"
)

// MaxMessageSize bounds a single websocket message. yamux writes frame headers
// and bodies separately and bodies never exceed the stream window, so this is
// a defensive cap only.
const MaxMessageSize = 8 << 20

// MaxStreamWindow is the per-stream receive window (yamux default is 256 KiB,
// which caps throughput on high-latency links).
const MaxStreamWindow = 1 << 20

// Conn adapts a gorilla websocket to net.Conn using binary messages. One
// goroutine may Read while another Writes.
type Conn struct {
	ws *websocket.Conn

	rmu sync.Mutex
	r   io.Reader

	wmu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// NewConn wraps ws. The caller must not use ws directly afterwards.
func NewConn(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(MaxMessageSize)
	return &Conn{ws: ws}
}

func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, mapErr(err)
			}
			if mt != websocket.BinaryMessage {
				continue // ignore text frames; NextReader discards the remainder
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if errors.Is(err, io.EOF) {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			return n, mapErr(err)
		}
		return n, nil
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, mapErr(err)
	}
	return len(p), nil
}

// Close sends a best-effort close frame and closes the underlying connection.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		_ = c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		c.closeErr = c.ws.Close()
	})
	return c.closeErr
}

func (c *Conn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

func mapErr(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		return io.EOF
	}
	if errors.Is(err, net.ErrClosed) {
		return io.EOF
	}
	return err
}

// YamuxConfig returns the yamux settings used by both ends.
func YamuxConfig(log *slog.Logger) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = 512
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.MaxStreamWindowSize = MaxStreamWindow
	cfg.StreamOpenTimeout = 30 * time.Second
	cfg.LogOutput = nil
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg.Logger = yamuxLogger{log: log}
	return cfg
}

type yamuxLogger struct{ log *slog.Logger }

func (l yamuxLogger) Print(v ...interface{}) { l.log.Debug(strings.TrimSpace(fmt.Sprint(v...))) }
func (l yamuxLogger) Printf(format string, v ...interface{}) {
	l.log.Debug(strings.TrimSpace(fmt.Sprintf(format, v...)))
}
func (l yamuxLogger) Println(v ...interface{}) { l.log.Debug(strings.TrimSpace(fmt.Sprintln(v...))) }

// Pipe copies a<->b until both directions finish, half-closing where the
// connection supports it, then closes both. It blocks.
func Pipe(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// closeWrite half-closes dst when possible. yamux streams treat Close as a
// half-close (FIN; reads continue until the peer's FIN). Anything else is
// closed fully, which also ends the opposite copy.
func closeWrite(c io.ReadWriteCloser) {
	switch v := c.(type) {
	case interface{ CloseWrite() error }:
		_ = v.CloseWrite()
	default:
		_ = v.Close()
	}
}

// BearerToken extracts the token from an "Authorization: Bearer x" header.
func BearerToken(h http.Header) string {
	v := strings.TrimSpace(h.Get("Authorization"))
	if len(v) < 7 || !strings.EqualFold(v[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(v[7:])
}

// SecretEqual compares secrets in constant time. Empty never matches.
func SecretEqual(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
