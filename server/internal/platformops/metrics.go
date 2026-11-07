package platformops

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// metricsServer is the optional separate /metrics listener (MATRIX_METRICS_ADDR).
type metricsServer struct {
	srv *http.Server
	ln  net.Listener
}

// Addr returns the bound address (useful when the configured port is 0).
func (m *metricsServer) Addr() string { return m.ln.Addr().String() }

// MetricsAuth requires "Authorization: Bearer <MATRIX_METRICS_TOKEN>" when a
// token is configured. The comparison is constant-time over SHA-256 digests,
// so neither content nor length of the token leaks through timing.
func MetricsAuth(token string) gin.HandlerFunc {
	if token == "" {
		return func(c *gin.Context) { c.Next() }
	}
	want := sha256.Sum256([]byte(token))
	return func(c *gin.Context) {
		got := ""
		if h := c.GetHeader("Authorization"); h != "" {
			if scheme, v, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
				got = strings.TrimSpace(v)
			}
		}
		sum := sha256.Sum256([]byte(got))
		if got == "" || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// mountMetrics decides where /metrics lives (see RegisterProbes).
func (o *Ops) mountMetrics(e *gin.Engine, metrics gin.HandlerFunc) {
	if metrics == nil {
		return
	}
	authMW := MetricsAuth(o.cfg.MetricsToken)
	switch {
	case o.cfg.MetricsAddr != "":
		ms, err := startMetricsServer(o.cfg.MetricsAddr, authMW, metrics)
		if err != nil {
			// Do not silently fall back to the main port: that would expose
			// metrics where the operator asked them not to be.
			o.log.Error("metrics listener failed to start; /metrics is not served", "addr", o.cfg.MetricsAddr, "err", err)
		} else {
			o.metricsServer = ms
			o.log.Info("metrics served on separate listener", "addr", ms.Addr(), "token_required", o.cfg.MetricsToken != "")
		}
		notFound := func(c *gin.Context) { c.String(http.StatusNotFound, "404 page not found") }
		e.GET("/metrics", notFound)
	default:
		if o.cfg.MetricsToken == "" {
			o.log.Warn("/metrics is exposed on the main port without authentication; set MATRIX_METRICS_ADDR (separate port) and/or MATRIX_METRICS_TOKEN")
		}
		e.GET("/metrics", authMW, metrics)
	}
}

func startMetricsServer(addr string, authMW, metrics gin.HandlerFunc) (*metricsServer, error) {
	if addr == "" {
		return nil, errors.New("empty address")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/metrics", authMW, metrics)
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ms := &metricsServer{srv: srv, ln: ln}
	go func() { _ = srv.Serve(ln) }()
	return ms, nil
}
