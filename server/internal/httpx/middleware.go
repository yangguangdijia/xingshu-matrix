package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	HeaderRequestID = "X-Request-ID"
	ctxRequestID    = "httpx.request_id"
)

// RequestID assigns (or propagates a sane incoming) X-Request-ID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if !validRequestID(id) {
			id = newID()
		}
		c.Set(ctxRequestID, id)
		c.Header(HeaderRequestID, id)
		c.Next()
	}
}

func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestIDFrom returns the request id of the current request ("" if none).
func RequestIDFrom(c *gin.Context) string { return c.GetString(ctxRequestID) }

// Recover converts panics into a 500 envelope and logs the stack.
func Recover(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && err == http.ErrAbortHandler {
					panic(r) // let net/http handle client aborts
				}
				log.Error("panic recovered", "request_id", RequestIDFrom(c), "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				if !c.Writer.Written() {
					Fail(c, Internal(fmt.Errorf("panic: %v", r)))
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}

// CORS implements a small CORS policy. origins may contain "*".
func CORS(origins []string) gin.HandlerFunc {
	allowAll := false
	set := map[string]bool{}
	for _, o := range origins {
		if o == "*" {
			allowAll = true
		}
		set[strings.TrimRight(o, "/")] = true
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" || len(origins) == 0 {
			c.Next()
			return
		}
		if !allowAll && !set[origin] {
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			c.Next()
			return
		}
		h := c.Writer.Header()
		h.Add("Vary", "Origin")
		if allowAll {
			h.Set("Access-Control-Allow-Origin", "*")
		} else {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		h.Set("Access-Control-Expose-Headers", "X-Request-ID, Content-Disposition")
		if c.Request.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			reqH := c.GetHeader("Access-Control-Request-Headers")
			if reqH == "" {
				reqH = "Authorization, Content-Type, X-Request-ID"
			}
			h.Set("Access-Control-Allow-Headers", reqH)
			h.Set("Access-Control-Max-Age", "600")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// AccessLog logs one structured line per request.
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		attrs := []any{
			"request_id", RequestIDFrom(c),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		}
		if uid, ok := c.Get("auth.user_id"); ok {
			attrs = append(attrs, "user_id", uid)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.String())
		}
		switch {
		case status >= 500:
			log.Error("http request", attrs...)
		case status >= 400:
			log.Warn("http request", attrs...)
		default:
			log.Info("http request", attrs...)
		}
	}
}

// ParamID parses a positive int64 path parameter; writes a 400 and returns false on error.
func ParamID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		Fail(c, BadRequest("invalid "+name))
		return 0, false
	}
	return id, true
}
