// Package platformops holds the platform-level HTTP mechanisms added in v1.29:
// liveness/readiness probes, /metrics protection, browser security headers,
// one-time WebSocket/SSE tickets, the global API rate limiter, Idempotency-Key
// handling and If-Match optimistic locking on core resources.
//
// Everything is configured from environment variables (see Config) so the
// shared config package does not need to change. server.New wires it.
package platformops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// Config is read from the environment by ConfigFromEnv.
type Config struct {
	// MetricsAddr (MATRIX_METRICS_ADDR, e.g. ":9090") serves /metrics on a
	// separate listener; the main port then no longer exposes /metrics.
	MetricsAddr string
	// MetricsToken (MATRIX_METRICS_TOKEN) requires "Authorization: Bearer <token>"
	// on /metrics (on whichever listener serves it).
	MetricsToken string

	// Security headers (MATRIX_CSP, MATRIX_FRAME_ANCESTORS, MATRIX_HSTS,
	// MATRIX_HSTS_MAX_AGE, MATRIX_HSTS_INCLUDE_SUBDOMAINS).
	Security httpx.SecurityOptions

	// APIRatePerMinute (MATRIX_API_RATE_PER_MINUTE, default 1200) per
	// authenticated user; AnonRatePerMinute (MATRIX_API_ANON_RATE_PER_MINUTE,
	// default 600) per token/IP on unauthenticated routes. 0 disables.
	APIRatePerMinute  int
	AnonRatePerMinute int
	// RateExempt (MATRIX_API_RATE_EXEMPT) lists path prefixes never limited.
	RateExempt []string

	// ReadyRequireRedis (MATRIX_READYZ_REQUIRE_REDIS) makes /readyz fail when
	// Redis is configured but unreachable. Default: reported, not required.
	ReadyRequireRedis bool

	// IdempotencyTTL (MATRIX_IDEMPOTENCY_TTL, default 24h).
	IdempotencyTTL time.Duration
	// WSTicketTTL is fixed at 30s (not configurable on purpose).
	WSTicketTTL time.Duration
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Security:          httpx.DefaultSecurityOptions(),
		APIRatePerMinute:  1200,
		AnonRatePerMinute: 600,
		IdempotencyTTL:    24 * time.Hour,
		WSTicketTTL:       30 * time.Second,
	}
}

// ConfigFromEnv applies MATRIX_* variables over DefaultConfig.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	c := DefaultConfig()
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(k string) (string, bool) {
		v, ok := lookup(k)
		return strings.TrimSpace(v), ok
	}
	var errs []string
	intVar := func(k string, dst *int) {
		if v, ok := get(k); ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				errs = append(errs, k+": must be a non-negative integer")
				return
			}
			*dst = n
		}
	}
	boolVar := func(k string, dst *bool) {
		if v, ok := get(k); ok && v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, k+": must be true/false")
				return
			}
			*dst = b
		}
	}
	if v, ok := get("MATRIX_METRICS_ADDR"); ok {
		c.MetricsAddr = v
	}
	if v, ok := get("MATRIX_METRICS_TOKEN"); ok {
		c.MetricsToken = v
	}
	if v, ok := get("MATRIX_CSP"); ok && v != "" {
		c.Security.CSP = v
	}
	if v, ok := get("MATRIX_FRAME_ANCESTORS"); ok && v != "" {
		c.Security.FrameAncestors = v
	}
	boolVar("MATRIX_HSTS", &c.Security.HSTS)
	intVar("MATRIX_HSTS_MAX_AGE", &c.Security.HSTSMaxAge)
	boolVar("MATRIX_HSTS_INCLUDE_SUBDOMAINS", &c.Security.HSTSIncludeSubdomains)
	intVar("MATRIX_API_RATE_PER_MINUTE", &c.APIRatePerMinute)
	intVar("MATRIX_API_ANON_RATE_PER_MINUTE", &c.AnonRatePerMinute)
	if v, ok := get("MATRIX_API_RATE_EXEMPT"); ok {
		c.RateExempt = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.RateExempt = append(c.RateExempt, p)
			}
		}
	}
	boolVar("MATRIX_READYZ_REQUIRE_REDIS", &c.ReadyRequireRedis)
	if v, ok := get("MATRIX_IDEMPOTENCY_TTL"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			errs = append(errs, "MATRIX_IDEMPOTENCY_TTL: must be a duration >= 1m")
		} else {
			c.IdempotencyTTL = d
		}
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("platformops config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// Pool is the subset of *pgxpool.Pool used here.
type Pool interface {
	store.DB
	Ping(ctx context.Context) error
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// Authenticator verifies an access token (auth.Service implements it).
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*rbac.Principal, error)
}

// Sealer encrypts the token bound to a ws ticket (crypto.Cipher implements it).
type Sealer interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(envelope string) (string, error)
}

// Options are the runtime dependencies.
type Options struct {
	Config     Config
	DB         Pool // nil disables DB-backed checks/stores (tests)
	Redis      *redis.Client
	Logger     *slog.Logger
	Auth       Authenticator
	Sealer     Sealer
	Migrations []store.MigrationSource

	// Test hooks (nil → PostgreSQL implementations).
	Tickets     TicketStore
	Idempotency IdempotencyStore
}

// Ops is the assembled set of mechanisms.
type Ops struct {
	cfg  Config
	db   Pool
	rdb  *redis.Client
	log  *slog.Logger
	auth Authenticator
	seal Sealer

	tickets TicketStore
	idem    IdempotencyStore

	userLimiter *auth.RateLimiter
	anonLimiter *auth.RateLimiter

	migrations    []store.MigrationSource
	migMu         sync.Mutex
	migExpected   map[string][]int
	migReady      bool
	metricsServer *metricsServer

	cancel context.CancelFunc
}

// New builds Ops from explicit options.
func New(o Options) *Ops {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Config.WSTicketTTL <= 0 {
		o.Config.WSTicketTTL = 30 * time.Second
	}
	if o.Config.IdempotencyTTL <= 0 {
		o.Config.IdempotencyTTL = 24 * time.Hour
	}
	ops := &Ops{
		cfg: o.Config, db: o.DB, rdb: o.Redis, log: o.Logger.With("component", "platformops"),
		auth: o.Auth, seal: o.Sealer, tickets: o.Tickets, idem: o.Idempotency, migrations: o.Migrations,
	}
	if ops.tickets == nil && o.DB != nil {
		ops.tickets = &pgTicketStore{db: o.DB}
	}
	if ops.idem == nil && o.DB != nil {
		ops.idem = &pgIdempotencyStore{db: o.DB}
	}
	ops.userLimiter = newAPILimiter(o.Redis, "api:user", o.Config.APIRatePerMinute)
	ops.anonLimiter = newAPILimiter(o.Redis, "api:anon", o.Config.AnonRatePerMinute)
	return ops
}

// FromDeps builds Ops for the running server: env config, the shared DB /
// Redis / cipher, auth for ticket redemption and every module's migrations
// for /readyz. A bad env value is logged and the defaults are kept.
func FromDeps(d *app.Deps, authn Authenticator, mods []app.Module) *Ops {
	cfg, err := ConfigFromEnv(os.LookupEnv)
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	if err != nil {
		log.Warn("platformops: invalid configuration, using defaults for the invalid keys", "err", err)
	}
	srcs := []store.MigrationSource{{Module: "core", FS: store.CoreMigrations()}}
	for _, m := range mods {
		if f := m.Migrations(); f != nil {
			srcs = append(srcs, store.MigrationSource{Module: m.Name(), FS: f})
		}
	}
	o := Options{Config: cfg, Redis: d.Redis, Logger: log, Auth: authn, Migrations: srcs}
	if d.DB != nil {
		o.DB = d.DB
	}
	if d.Crypto != nil {
		o.Sealer = d.Crypto
	}
	ops := New(o)
	ops.startBackground()
	return ops
}

// Config returns the effective configuration.
func (o *Ops) Config() Config { return o.cfg }

// startBackground purges expired tickets and idempotency records.
func (o *Ops) startBackground() {
	if o.db == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				o.purge(ctx)
			}
		}
	}()
}

func (o *Ops) purge(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, q := range []string{
		`DELETE FROM ws_tickets WHERE expires_at < now() - interval '1 minute'`,
		`DELETE FROM idempotency_keys WHERE expires_at < now()`,
	} {
		if _, err := o.db.Exec(cctx, q); err != nil && !isUndefinedTable(err) {
			o.log.Warn("purge expired records", "err", err)
		}
	}
}

// Close stops background work and the separate metrics listener.
func (o *Ops) Close(ctx context.Context) error {
	if o.cancel != nil {
		o.cancel()
	}
	if o.metricsServer != nil {
		return o.metricsServer.srv.Shutdown(ctx)
	}
	return nil
}

func isUndefinedTable(err error) bool {
	var pg *pgconn.PgError
	if ok := asPgErr(err, &pg); ok {
		return pg.Code == "42P01"
	}
	return false
}

func asPgErr(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var errNoRows = pgx.ErrNoRows
