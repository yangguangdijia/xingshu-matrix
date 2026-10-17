// Package app defines the contract between the platform core and feature
// modules (cluster, hosts/agent, bastion, observability, cicd, middleware, ...).
//
// A module is a value implementing Module, listed in internal/modules/registry.go.
// At startup the core:
//  1. applies the module's embedded migrations (tracked per module name),
//  2. registers its permissions (optional PermissionProvider),
//  3. calls Register with the authenticated /api/v1 router group,
//  4. calls Start (optional Starter) for background workers, and Stop on shutdown.
//
// Modules must not import each other's internals; they talk to the core only
// through Deps.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/config"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
)

// Deps are the shared core services handed to every module.
type Deps struct {
	// DB is the PostgreSQL pool. Use store.InTx for transactions.
	DB *pgxpool.Pool
	// Cfg is the loaded server configuration (read-only).
	Cfg *config.Config
	// Crypto encrypts secrets at rest (kubeconfigs, host passwords, tokens).
	Crypto *crypto.Cipher
	// Audit records explicit audit events (write HTTP requests are recorded
	// automatically; use audit.SetResource / audit.SetDetail to enrich them).
	Audit *audit.Recorder
	// Approval gates production changes: call Approval.Gate before a write on
	// an environment and register a ticket type whose handler performs it.
	Approval *approval.Service
	// AITools is the AI tool-calling registry; register read tools freely and
	// mark state-changing tools RequiresConfirmation.
	AITools *ai.ToolRegistry
	// Logger is a structured logger scoped with module=<Name()>.
	Logger *slog.Logger

	// RBAC exposes env policies (RBAC.RequiresApproval) and the permission
	// catalog. Use rbac.RequirePerm / rbac.RequireEnvPerm as route middleware.
	RBAC *rbac.Service
	// Settings is the key/value settings store (secrets encrypted).
	Settings *settings.Service
	// Public is the unauthenticated /api/v1 group, for webhooks (Alertmanager)
	// and agent enrolment. Every handler on it must authenticate the caller itself.
	Public *gin.RouterGroup
	// Metrics registers module Prometheus collectors (served on /metrics).
	Metrics prometheus.Registerer
	// Redis is the shared cache/lock client; nil when redis.addr is not configured.
	Redis *redis.Client
}

// Module is a pluggable feature module.
type Module interface {
	// Name is a unique, stable, lowercase identifier ("cluster", "cicd"). It
	// keys the module's migration history, so never rename a shipped module.
	Name() string
	// Migrations returns an fs.FS whose root holds NNNN_name.up.sql files
	// (typically via //go:embed migrations/*.sql + fs.Sub), or nil.
	Migrations() fs.FS
	// Register mounts the module's routes on r, the authenticated /api/v1
	// group (auth + forced-password-change gate + audit already applied).
	Register(r *gin.RouterGroup, d *Deps)
}

// PermissionProvider is implemented by modules that define permissions
// (shown in the role editor). Use rbac.P(module, resource, action, desc).
type PermissionProvider interface {
	Permissions() []rbac.Permission
}

// Starter is implemented by modules with background workers (informers,
// agent hubs, schedulers). ctx is cancelled on shutdown.
type Starter interface {
	Start(ctx context.Context, d *Deps) error
}

// Stopper is implemented by modules that need explicit cleanup on shutdown.
type Stopper interface {
	Stop(ctx context.Context) error
}

// Validate checks module names are unique, well-formed and not reserved.
func Validate(mods []Module) error {
	seen := map[string]bool{}
	for _, m := range mods {
		n := m.Name()
		if !moduleNameRe.MatchString(n) {
			return fmt.Errorf("app: invalid module name %q (want ^[a-z][a-z0-9_]{1,31}$)", n)
		}
		if n == "core" {
			return fmt.Errorf("app: module name %q is reserved", n)
		}
		if seen[n] {
			return fmt.Errorf("app: duplicate module name %q", n)
		}
		seen[n] = true
	}
	return nil
}

var moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
