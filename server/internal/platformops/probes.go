package platformops

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// probeTimeout bounds every dependency check of /readyz.
const probeTimeout = 2 * time.Second

// Check is one dependency result in the /readyz body.
type Check struct {
	// Status is "ok", "fail" or "disabled".
	Status string `json:"status"`
	// Required checks make the probe fail; optional ones are only reported.
	Required bool   `json:"required"`
	Error    string `json:"error,omitempty"`
	// Pending lists missing migrations ("module:version"), at most 20.
	Pending []string `json:"pending,omitempty"`
}

// Readiness is the /readyz body.
type Readiness struct {
	Status string           `json:"status"` // "ok" | "fail"
	Checks map[string]Check `json:"checks"`
}

// RegisterProbes mounts /livez, /healthz, /readyz and /metrics on e. It
// replaces the old fixed /healthz and the unauthenticated /metrics:
//
//   - MATRIX_METRICS_ADDR set: /metrics is served on that separate listener
//     only (the main port answers 404);
//   - MATRIX_METRICS_TOKEN set: /metrics requires the Bearer token;
//   - neither: /metrics stays open on the main port and a warning is logged.
func (o *Ops) RegisterProbes(e *gin.Engine, metrics gin.HandlerFunc) {
	ok := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	e.GET("/livez", ok)
	e.HEAD("/livez", ok)
	e.GET("/healthz", ok)
	e.HEAD("/healthz", ok)
	e.GET("/readyz", o.readyzHandler)
	e.HEAD("/readyz", o.readyzHandler)
	o.mountMetrics(e, metrics)
}

func (o *Ops) readyzHandler(c *gin.Context) {
	r := o.Ready(c.Request.Context())
	code := http.StatusOK
	if r.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(code, r)
}

// Ready evaluates the readiness checks.
func (o *Ops) Ready(ctx context.Context) Readiness {
	checks := map[string]Check{}

	db := Check{Status: "ok", Required: true}
	if o.pingDB == nil {
		db.Status, db.Error = "fail", "database not configured"
	} else {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		if err := o.pingDB(cctx); err != nil {
			db.Status, db.Error = "fail", shortErr(err)
		}
		cancel()
	}
	checks["database"] = db

	mig := Check{Status: "ok", Required: true}
	if db.Status != "ok" {
		mig.Status, mig.Error = "fail", "database unavailable"
	} else if pending, err := o.pendingMigrations(ctx); err != nil {
		mig.Status, mig.Error = "fail", shortErr(err)
	} else if len(pending) > 0 {
		mig.Status, mig.Error = "fail", fmt.Sprintf("%d migration(s) not applied", len(pending))
		if len(pending) > 20 {
			pending = pending[:20]
		}
		mig.Pending = pending
	}
	checks["migrations"] = mig

	rc := Check{Status: "disabled", Required: o.cfg.ReadyRequireRedis}
	if o.pingRedis != nil {
		rc.Status = "ok"
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		if err := o.pingRedis(cctx); err != nil {
			rc.Status, rc.Error = "fail", shortErr(err)
		}
		cancel()
	} else if o.cfg.ReadyRequireRedis {
		rc.Status, rc.Error = "fail", "redis not configured"
	}
	checks["redis"] = rc

	out := Readiness{Status: "ok", Checks: checks}
	for _, ch := range checks {
		if ch.Required && ch.Status == "fail" {
			out.Status = "fail"
		}
	}
	return out
}

// pendingMigrations compares every module's embedded migrations with
// schema_migrations. Once everything is applied the result is cached (a
// running binary's embedded set never changes), so later probes cost one ping.
func (o *Ops) pendingMigrations(ctx context.Context) ([]string, error) {
	o.migMu.Lock()
	if o.migReady {
		o.migMu.Unlock()
		return nil, nil
	}
	if o.migExpected == nil {
		exp := map[string][]int{}
		for _, src := range o.migrations {
			migs, err := store.ParseMigrations(src.FS)
			if err != nil {
				o.migMu.Unlock()
				return nil, fmt.Errorf("module %s: %w", src.Module, err)
			}
			for _, m := range migs {
				exp[src.Module] = append(exp[src.Module], m.Version)
			}
		}
		o.migExpected = exp
	}
	expected := o.migExpected
	o.migMu.Unlock()

	if len(expected) == 0 {
		o.markMigReady()
		return nil, nil
	}
	if o.appliedMigs == nil {
		return nil, fmt.Errorf("database not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	applied, err := o.appliedMigs(cctx)
	if err != nil {
		return nil, err
	}
	var pending []string
	for mod, vs := range expected {
		for _, v := range vs {
			if !applied[mod][v] {
				pending = append(pending, fmt.Sprintf("%s:%04d", mod, v))
			}
		}
	}
	sort.Strings(pending)
	if len(pending) == 0 {
		o.markMigReady()
	}
	return pending, nil
}

func (o *Ops) markMigReady() {
	o.migMu.Lock()
	o.migReady = true
	o.migMu.Unlock()
}

func queryAppliedMigrations(ctx context.Context, db store.DB) (map[string]map[int]bool, error) {
	rows, err := db.Query(ctx, `SELECT module, version FROM schema_migrations`)
	if err != nil {
		if isUndefinedTable(err) {
			return map[string]map[int]bool{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[int]bool{}
	for rows.Next() {
		var mod string
		var v int
		if err := rows.Scan(&mod, &v); err != nil {
			return nil, err
		}
		if out[mod] == nil {
			out[mod] = map[int]bool{}
		}
		out[mod][v] = true
	}
	return out, rows.Err()
}

// shortErr keeps probe output small and single-line (no DSNs: pgx errors do
// not include the password, and we cut at the first newline).
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
