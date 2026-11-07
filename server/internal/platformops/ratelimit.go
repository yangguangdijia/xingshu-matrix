package platformops

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// newAPILimiter returns an in-process token bucket of perMinute per key that
// consults Redis first when configured (shared across replicas; falls back to
// the local bucket while Redis is unreachable). perMinute <= 0 disables (nil).
func newAPILimiter(rdb *redis.Client, prefix string, perMinute int) *auth.RateLimiter {
	if perMinute <= 0 {
		return nil
	}
	l := auth.NewRateLimiter(perMinute)
	if rdb != nil {
		l.UseRemote(auth.NewRedisRateLimiter(rdb, prefix, perMinute))
	}
	return l
}

// rateKey picks the limiter key: u:<user id> for an authenticated principal,
// t:<token digest> for a bearer presented on an unauthenticated route, else
// ip:<client ip>. The token itself never becomes part of a key.
func rateKey(c *gin.Context) (key string, authed bool) {
	if p := rbac.PrincipalFrom(c); p != nil && p.UserID > 0 {
		return "u:" + strconv.FormatInt(p.UserID, 10), true
	}
	if tok := bearerFrom(c); tok != "" {
		s := sha256.Sum256([]byte(tok))
		return "t:" + hex.EncodeToString(s[:12]), false
	}
	return "ip:" + c.ClientIP(), false
}

func (o *Ops) rateExempt(path string) bool {
	for _, p := range o.cfg.RateExempt {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// RateLimit is the global API limiter. Mount it on the public group and, after
// Auth.Middleware, on the authenticated group: authenticated calls are counted
// per user (MATRIX_API_RATE_PER_MINUTE, default 1200/min), anonymous ones per
// token digest or client IP (MATRIX_API_ANON_RATE_PER_MINUTE, default 600/min).
// A rejected request gets 429 + Retry-After + X-RateLimit-Limit and is not
// written to the audit log.
func (o *Ops) RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions || o.rateExempt(c.Request.URL.Path) {
			c.Next()
			return
		}
		key, authed := rateKey(c)
		l, limit := o.anonLimiter, o.cfg.AnonRatePerMinute
		if authed {
			l, limit = o.userLimiter, o.cfg.APIRatePerMinute
		}
		if l == nil {
			c.Next()
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		if ok, wait := l.Allow(key); !ok {
			audit.Skip(c)
			secs := int(math.Ceil(wait.Seconds()))
			if secs < 1 {
				secs = 1
			}
			c.Header("Retry-After", strconv.Itoa(secs))
			httpx.Fail(c, httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "请求过于频繁，请稍后再试"))
			return
		}
		c.Next()
	}
}
