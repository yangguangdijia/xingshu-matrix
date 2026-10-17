package rbac

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID      int64    `json:"user_id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	SessionID   string   `json:"-"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	// EnvScope lists environments the user may operate on; nil means all.
	EnvScope           []string `json:"env_scope"`
	MustChangePassword bool     `json:"must_change_password"`
}

// Can reports whether the principal holds perm.
func (p *Principal) Can(perm string) bool { return p != nil && Match(p.Permissions, perm) }

// CanEnv reports whether the principal may operate on env.
func (p *Principal) CanEnv(env string) bool {
	if p == nil {
		return false
	}
	if p.EnvScope == nil {
		return true
	}
	for _, e := range p.EnvScope {
		if e == env || e == "*" {
			return true
		}
	}
	return false
}

const ginKeyPrincipal = "rbac.principal"

type ctxKey struct{}

// SetPrincipal stores the principal on the gin context and the request context.
func SetPrincipal(c *gin.Context, p *Principal) {
	c.Set(ginKeyPrincipal, p)
	c.Set("auth.user_id", p.UserID)
	c.Request = c.Request.WithContext(WithPrincipal(c.Request.Context(), p))
}

// WithPrincipal returns ctx carrying p (used by background work like AI tools).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal carried by ctx, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// PrincipalFrom returns the principal of the request, or nil when unauthenticated.
func PrincipalFrom(c *gin.Context) *Principal {
	if v, ok := c.Get(ginKeyPrincipal); ok {
		if p, ok := v.(*Principal); ok {
			return p
		}
	}
	return nil
}

// RequirePerm requires the caller to hold every listed permission.
func RequirePerm(perms ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := PrincipalFrom(c)
		if p == nil {
			httpx.Fail(c, httpx.Unauthorized("authentication required"))
			return
		}
		for _, perm := range perms {
			if !p.Can(perm) {
				httpx.Fail(c, httpx.Forbidden("permission denied: "+perm).WithDetails(gin.H{"required": perm}))
				return
			}
		}
		c.Next()
	}
}

// RequireAnyPerm requires at least one of the listed permissions.
func RequireAnyPerm(perms ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := PrincipalFrom(c)
		if p == nil {
			httpx.Fail(c, httpx.Unauthorized("authentication required"))
			return
		}
		for _, perm := range perms {
			if p.Can(perm) {
				c.Next()
				return
			}
		}
		httpx.Fail(c, httpx.Forbidden("permission denied").WithDetails(gin.H{"required_any": perms}))
	}
}

// RequireEnvPerm requires perm and that the caller's env scope covers the
// environment returned by envOf (e.g. read from a path param or the target
// cluster). An empty env is treated as "no environment" and only perm is checked.
func RequireEnvPerm(perm string, envOf func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := PrincipalFrom(c)
		if p == nil {
			httpx.Fail(c, httpx.Unauthorized("authentication required"))
			return
		}
		if !p.Can(perm) {
			httpx.Fail(c, httpx.Forbidden("permission denied: "+perm).WithDetails(gin.H{"required": perm}))
			return
		}
		if env := envOf(c); env != "" && !p.CanEnv(env) {
			httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env))
			return
		}
		c.Next()
	}
}
