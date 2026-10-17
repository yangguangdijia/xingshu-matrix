// Command matrix-server is the 星枢 Matrix platform API server.
//
//	matrix-server [serve] -config /etc/matrix/config.yaml
//	matrix-server migrate -config ...
//	matrix-server version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/config"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/metrics"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/modules"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/server"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/users"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("MATRIX_CONFIG"), "path to config.yaml (env MATRIX_CONFIG)")
	_ = fs.Parse(args)

	switch cmd {
	case "version":
		b, _ := json.MarshalIndent(version.Get(), "", "  ")
		fmt.Println(string(b))
		return
	case "serve", "migrate":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve|migrate|version)\n", cmd)
		os.Exit(2)
	}
	if err := run(cmd, *cfgPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(c config.LogConfig) *slog.Logger {
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func run(cmd, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)
	slog.SetDefault(log)
	for _, w := range cfg.Warnings {
		log.Warn("config", "warning", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mods := modules.All()
	if err := app.Validate(mods); err != nil {
		return err
	}

	pool, err := store.Open(ctx, cfg.Database.DSN, cfg.Database.MaxConns)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if cmd == "migrate" || cfg.Database.AutoMigrate {
		if err := migrate(ctx, pool, log, mods); err != nil {
			return err
		}
		if cmd == "migrate" {
			log.Info("migrations applied")
			return nil
		}
	}
	return serve(ctx, cfg, log, pool, mods)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, mods []app.Module) error {
	srcs := []store.MigrationSource{{Module: "core", FS: store.CoreMigrations()}}
	for _, m := range mods {
		if f := m.Migrations(); f != nil {
			srcs = append(srcs, store.MigrationSource{Module: m.Name(), FS: f})
		}
	}
	return store.Migrate(ctx, pool, log, srcs...)
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, mods []app.Module) error {
	key, err := cfg.EncryptionKeyBytes()
	if err != nil {
		return err
	}
	cipher, err := crypto.New(key)
	if err != nil {
		return err
	}

	var rdb *redis.Client
	if cfg.Redis.Addr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Warn("redis unreachable at startup", "err", err)
		}
		defer rdb.Close()
	}

	// RBAC and permission catalog.
	catalog := rbac.NewCatalog()
	if err := catalog.Register(rbac.CorePermissions()...); err != nil {
		return err
	}
	for _, m := range mods {
		if pp, ok := m.(app.PermissionProvider); ok {
			if err := catalog.Register(pp.Permissions()...); err != nil {
				return fmt.Errorf("module %s permissions: %w", m.Name(), err)
			}
		}
	}
	rbacSvc := rbac.NewService(pool, catalog)

	// Auth.
	authRepo := &auth.PgRepo{DB: pool}
	tokens := auth.NewTokenManager(cfg.Auth.JWTSecret, cfg.Auth.Issuer, cfg.Auth.AccessTTL)
	authSvc := auth.NewService(authRepo, rbacSvc, tokens, auth.Options{
		RefreshTTL:        cfg.Auth.RefreshTTL,
		MaxLoginFailures:  cfg.Auth.MaxLoginFailures,
		LockoutDuration:   cfg.Auth.LockoutDuration,
		PrincipalCacheTTL: 15 * time.Second,
	}, log.With("component", "auth"))
	rbacSvc.OnChange = authSvc.InvalidateAll
	if created, err := authSvc.EnsureAdmin(ctx, cfg.Auth.InitialAdminPassword); err != nil {
		return fmt.Errorf("ensure admin: %w", err)
	} else if created {
		log.Warn("built-in admin created; password must be changed at first login", "username", "admin")
	}

	auditStore := &audit.PgStore{DB: pool}
	recorder := audit.NewRecorder(auditStore, log.With("component", "audit"))
	defer recorder.Close()

	approvalSvc := approval.New(&approval.PgRepo{DB: pool}, rbacSvc, log.With("component", "approval"))
	settingsSvc := &settings.Service{DB: pool, Cipher: cipher}
	usersSvc := &users.Service{DB: pool, RBAC: rbacSvc, Sessions: authSvc}

	tools := ai.NewToolRegistry()
	if err := registerCoreTools(tools, auditStore); err != nil {
		return err
	}
	aiSvc := &ai.Service{
		Store:         &ai.Store{DB: pool, Cipher: cipher},
		Tools:         tools,
		HTTP:          &http.Client{},
		Log:           log.With("component", "ai"),
		Timeout:       cfg.AI.RequestTimeout,
		MaxToolRounds: cfg.AI.MaxToolRounds,
		ConfirmTTL:    10 * time.Minute,
	}

	m := metrics.New()
	deps := &app.Deps{
		DB: pool, Cfg: cfg, Crypto: cipher, Audit: recorder, Approval: approvalSvc,
		AITools: tools, Logger: log, RBAC: rbacSvc, Settings: settingsSvc,
		Metrics: m.Registry, Redis: rdb,
	}
	engine := server.New(deps, server.Core{
		Auth: authSvc, Limiter: auth.NewRateLimiter(cfg.Auth.LoginRatePerMinute), RBAC: rbacSvc,
		Users: usersSvc, Settings: settingsSvc, AuditDB: auditStore, Audit: recorder,
		Approval: approvalSvc, AI: aiSvc, Metrics: m,
	}, withLoggers(mods, log))

	// Background workers.
	for _, mod := range mods {
		if st, ok := mod.(app.Starter); ok {
			md := *deps
			md.Logger = log.With("module", mod.Name())
			if err := st.Start(ctx, &md); err != nil {
				return fmt.Errorf("start module %s: %w", mod.Name(), err)
			}
		}
	}
	go purgeSessions(ctx, authRepo, log)

	srv := &http.Server{
		Addr: cfg.Server.Listen, Handler: engine,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("matrix-server listening", "addr", cfg.Server.Listen, "version", version.Get().Version, "modules", len(mods))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(sctx)
	for _, mod := range mods {
		if sp, ok := mod.(app.Stopper); ok {
			if err := sp.Stop(sctx); err != nil {
				log.Warn("module stop", "module", mod.Name(), "err", err)
			}
		}
	}
	approvalSvc.Wait()
	return nil
}

// withLoggers is a hook point for per-module logger scoping (Register receives shared deps).
func withLoggers(mods []app.Module, _ *slog.Logger) []app.Module { return mods }

func purgeSessions(ctx context.Context, repo *auth.PgRepo, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := repo.PurgeExpiredSessions(ctx); err != nil {
				log.Warn("purge sessions", "err", err)
			} else if n > 0 {
				log.Info("purged expired sessions", "count", n)
			}
		}
	}
}

// registerCoreTools registers built-in AI tools backed by core data.
func registerCoreTools(r *ai.ToolRegistry, as *audit.PgStore) error {
	return r.Register(ai.Tool{
		Name:        "search_audit_logs",
		Module:      "core",
		Description: "按用户、资源、动作、结果和时间范围检索平台审计日志（只读）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"username":{"type":"string","description":"操作人用户名"},
			"resource":{"type":"string","description":"资源类型，如 user、cluster、pipeline"},
			"action":{"type":"string","description":"动作，如 create、update、delete"},
			"since":{"type":"string","description":"起始时间 RFC3339"},
			"until":{"type":"string","description":"结束时间 RFC3339"},
			"limit":{"type":"integer","minimum":1,"maximum":100}}}`),
		Permission: "audit:read",
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Username, Resource, Action, Since, Until string
				Limit                                    int
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
			}
			if a.Limit <= 0 || a.Limit > 100 {
				a.Limit = 20
			}
			f := audit.Filter{Username: a.Username, Resource: a.Resource, Action: a.Action}
			if t, err := time.Parse(time.RFC3339, a.Since); err == nil {
				f.From = t
			}
			if t, err := time.Parse(time.RFC3339, a.Until); err == nil {
				f.To = t
			}
			items, total, err := as.Query(ctx, f, httpx.Page{Page: 1, Size: a.Limit})
			if err != nil {
				return nil, err
			}
			return map[string]any{"total": total, "items": items}, nil
		},
	})
}
