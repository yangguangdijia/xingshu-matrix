package jobs

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric results for matrix_jobs_processed_total.
const (
	ResultSucceeded = "succeeded"
	ResultFailed    = "failed" // attempt failed, will retry
	ResultDead      = "dead"   // dead-lettered (exhausted or permanent)
	ResultReaped    = "reaped" // lease expired, recovered by the reaper
)

// Metrics are the queue's Prometheus collectors. A nil *Metrics is a no-op.
type Metrics struct {
	processed *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	byStatus  *prometheus.GaugeVec
}

// NewMetrics creates the collectors and registers them on reg (nil = not
// registered). Registering twice on the same registry reuses the existing
// collectors instead of failing.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		processed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "matrix", Subsystem: "jobs", Name: "processed_total",
			Help: "Background job attempts by type and result (succeeded, failed, dead, reaped).",
		}, []string{"type", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "matrix", Subsystem: "jobs", Name: "duration_seconds",
			Help:    "Background job attempt duration by type.",
			Buckets: []float64{.05, .1, .5, 1, 5, 15, 60, 300, 900, 3600},
		}, []string{"type"}),
		byStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "matrix", Subsystem: "jobs", Name: "by_status",
			Help: "Background jobs currently in each status (dead = dead-letter backlog).",
		}, []string{"status"}),
	}
	if reg == nil {
		return m
	}
	m.processed = registerOrReuse(reg, m.processed)
	m.duration = registerOrReuse(reg, m.duration)
	m.byStatus = registerOrReuse(reg, m.byStatus)
	return m
}

func registerOrReuse[T prometheus.Collector](reg prometheus.Registerer, c T) T {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(T); ok {
				return existing
			}
		}
		// Conflicting descriptor: keep an unregistered collector rather than panic.
	}
	return c
}

func (m *Metrics) observe(jobType, result string, seconds float64) {
	if m == nil {
		return
	}
	m.processed.WithLabelValues(jobType, result).Inc()
	if seconds >= 0 && result != ResultReaped {
		m.duration.WithLabelValues(jobType).Observe(seconds)
	}
}

func (m *Metrics) setCounts(counts map[string]int64) {
	if m == nil {
		return
	}
	for st, n := range counts {
		m.byStatus.WithLabelValues(st).Set(float64(n))
	}
}
