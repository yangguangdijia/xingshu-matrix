package platformops

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// VersionStore loads the optimistic-lock version of the resources that
// support If-Match (users, settings, env policies).
type VersionStore interface {
	UserVersion(ctx context.Context, id int64) (httpx.Version, error)
	SettingVersion(ctx context.Context, key string) (httpx.Version, error)
	EnvPolicyVersion(ctx context.Context, env string) (httpx.Version, error)
}

type pgVersionStore struct{ db Pool }

func (s *pgVersionStore) one(ctx context.Context, sql string, arg any) (httpx.Version, error) {
	var v httpx.Version
	row := s.db.QueryRow(ctx, sql, arg)
	var t time.Time
	if err := row.Scan(&t); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.Version{}, nil
		}
		return v, err
	}
	return httpx.VersionFromTime(t), nil
}

func (s *pgVersionStore) UserVersion(ctx context.Context, id int64) (httpx.Version, error) {
	return s.one(ctx, `SELECT updated_at FROM users WHERE id=$1`, id)
}

func (s *pgVersionStore) SettingVersion(ctx context.Context, key string) (httpx.Version, error) {
	return s.one(ctx, `SELECT updated_at FROM settings WHERE key=$1`, key)
}

func (s *pgVersionStore) EnvPolicyVersion(ctx context.Context, env string) (httpx.Version, error) {
	return s.one(ctx, `SELECT updated_at FROM env_policies WHERE env=$1`, env)
}

// pgAdvisoryLock serialises conditional writes on one key with a
// session-level advisory lock held on a dedicated pooled connection.
func pgAdvisoryLock(db Pool) httpx.LockFunc {
	return func(ctx context.Context, key string) (func(), error) {
		conn, err := db.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, "ifmatch:"+key); err != nil {
			// The lock state is unknown: never hand this connection back.
			_ = conn.Hijack().Close(context.Background())
			return nil, err
		}
		return func() {
			if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, "ifmatch:"+key); err != nil {
				_ = conn.Hijack().Close(context.Background())
				return
			}
			conn.Release()
		}, nil
	}
}

// conditionalRoutes maps gin full paths to their version loaders.
func (o *Ops) conditionalRoutes() map[string]httpx.VersionFunc {
	vs := o.versions
	if vs == nil {
		return nil
	}
	return map[string]httpx.VersionFunc{
		"/api/v1/users/:id": func(c *gin.Context) (httpx.Version, error) {
			id, err := strconv.ParseInt(c.Param("id"), 10, 64)
			if err != nil || id <= 0 {
				return httpx.Version{}, nil
			}
			return vs.UserVersion(c.Request.Context(), id)
		},
		"/api/v1/settings/:key": func(c *gin.Context) (httpx.Version, error) {
			return vs.SettingVersion(c.Request.Context(), c.Param("key"))
		},
		"/api/v1/env-policies/:env": func(c *gin.Context) (httpx.Version, error) {
			return vs.EnvPolicyVersion(c.Request.Context(), c.Param("env"))
		},
	}
}

// Conditional applies httpx.IfMatch to the routes that support optimistic
// locking; every other route passes straight through.
func (o *Ops) Conditional() gin.HandlerFunc {
	routes := o.conditionalRoutes()
	if len(routes) == 0 {
		return func(c *gin.Context) { c.Next() }
	}
	mws := make(map[string]gin.HandlerFunc, len(routes))
	for path, load := range routes {
		mws[path] = httpx.IfMatch(httpx.IfMatchOptions{Load: load, Lock: o.lockResource})
	}
	return func(c *gin.Context) {
		if mw, ok := mws[c.FullPath()]; ok {
			mw(c)
			return
		}
		c.Next()
	}
}
