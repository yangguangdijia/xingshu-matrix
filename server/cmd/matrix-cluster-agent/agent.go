package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/tunnel/wire"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

var errTokenRejected = errors.New("token rejected")

type agent struct {
	cfg    config
	log    *slog.Logger
	dialer *websocket.Dialer
	proxy  http.Handler // authenticated stream traffic → kube-apiserver

	fingerprint func(ctx context.Context) (string, error)
	fp          string

	ready atomic.Bool

	// Tunables (tests shorten them).
	minBackoff  time.Duration
	maxBackoff  time.Duration
	resetAfter  time.Duration
	drainPeriod time.Duration
}

func newAgent(cfg config, tlsCfg *tls.Config, proxy http.Handler, log *slog.Logger) *agent {
	return &agent{
		cfg: cfg,
		log: log,
		dialer: &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 15 * time.Second,
			TLSClientConfig:  tlsCfg,
			ReadBufferSize:   32 << 10,
			WriteBufferSize:  32 << 10,
		},
		proxy:       proxy,
		minBackoff:  time.Second,
		maxBackoff:  60 * time.Second,
		resetAfter:  time.Minute,
		drainPeriod: 5 * time.Second,
	}
}

// run keeps a tunnel session up until ctx is cancelled.
func (a *agent) run(ctx context.Context) {
	if !a.loadFingerprint(ctx) {
		return
	}
	backoff := a.minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > a.resetAfter {
			backoff = a.minBackoff
		}
		wait := backoff
		if errors.Is(err, errTokenRejected) {
			a.log.Error("token rejected by the platform; retrying", "retry_in", a.maxBackoff.String())
			wait = a.maxBackoff
		} else {
			a.log.Warn("tunnel session ended", "error", err, "retry_in", wait.String())
		}
		if !sleepCtx(ctx, jitter(wait)) {
			return
		}
		if backoff *= 2; backoff > a.maxBackoff {
			backoff = a.maxBackoff
		}
	}
}

func (a *agent) loadFingerprint(ctx context.Context) bool {
	if a.fingerprint == nil {
		return true
	}
	backoff := a.minBackoff
	for {
		fp, err := a.fingerprint(ctx)
		if err == nil && fp != "" {
			a.fp = fp
			return true
		}
		if err == nil {
			err = errors.New("empty namespace UID")
		}
		a.log.Error("cannot read kube-system namespace UID (needs get namespaces)", "error", err, "retry_in", backoff.String())
		if !sleepCtx(ctx, jitter(backoff)) {
			return false
		}
		if backoff *= 2; backoff > a.maxBackoff {
			backoff = a.maxBackoff
		}
	}
}

// session dials the platform and serves streams until the session ends.
func (a *agent) session(ctx context.Context) error {
	token, err := readToken(a.cfg)
	if err != nil {
		return err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	hdr.Set(wire.HeaderAgentVersion, version.Version)
	if a.fp != "" {
		hdr.Set(wire.HeaderClusterFingerprint, a.fp)
	}
	ws, resp, err := a.dialer.DialContext(ctx, a.cfg.ConnectURL, hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return errTokenRejected
			}
			return fmt.Errorf("connect: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("connect: %w", err)
	}
	secret := resp.Header.Get(wire.HeaderSessionSecret)
	sessionID := resp.Header.Get(wire.HeaderSessionID)
	conn := wire.NewConn(ws)
	if secret == "" {
		_ = conn.Close()
		return errors.New("connect: platform did not return a session secret")
	}
	mux, err := yamux.Server(conn, wire.YamuxConfig(a.log))
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("yamux: %w", err)
	}
	defer mux.Close()

	srv := &http.Server{
		Handler:           sessionHandler(secret, a.proxy),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       10 * time.Minute, // longer than client-side idle timeouts
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	a.ready.Store(true)
	defer a.ready.Store(false)
	a.log.Info("tunnel established", "session_id", sessionID)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(mux) }()

	select {
	case <-ctx.Done():
		a.ready.Store(false)
		a.drain(mux)
		_ = srv.Close()
		<-serveErr
		return ctx.Err()
	case <-mux.CloseChan():
		_ = srv.Close()
		<-serveErr
		return errors.New("session closed")
	case err := <-serveErr:
		_ = srv.Close()
		return fmt.Errorf("serve: %w", err)
	}
}

// drain refuses new streams and waits briefly for in-flight ones.
func (a *agent) drain(mux *yamux.Session) {
	_ = mux.GoAway()
	deadline := time.Now().Add(a.drainPeriod)
	for mux.NumStreams() > 0 && time.Now().Before(deadline) && !mux.IsClosed() {
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *agent) healthHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	m.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.ready.Load() {
			http.Error(w, "tunnel not established", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return m
}

// jitter returns a duration in [d/2, d).
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
