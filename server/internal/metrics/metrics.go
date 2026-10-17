// Package metrics exposes Prometheus metrics for matrix-server.
package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

// Metrics holds the registry and HTTP collectors.
type Metrics struct {
	Registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
}

// New creates a registry with Go/process collectors, build info and HTTP metrics.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "matrix", Name: "http_requests_total", Help: "HTTP requests by method, route and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "matrix", Name: "http_request_duration_seconds", Help: "HTTP request latency by method and route.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "matrix", Name: "http_requests_in_flight", Help: "In-flight HTTP requests.",
		}),
	}
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "matrix", Name: "build_info", Help: "Build information.",
		ConstLabels: prometheus.Labels{"version": version.Version, "commit": version.Commit},
	})
	build.Set(1)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.inflight, build,
	)
	return m
}

// Middleware records request metrics, labelled by the matched route template
// (not the raw path) to keep cardinality bounded.
func (m *Metrics) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		m.inflight.Inc()
		c.Next()
		m.inflight.Dec()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		m.requests.WithLabelValues(c.Request.Method, route, strconv.Itoa(c.Writer.Status())).Inc()
		m.duration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}

// Handler serves /metrics.
func (m *Metrics) Handler() gin.HandlerFunc {
	h := promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
	return gin.WrapH(h)
}
