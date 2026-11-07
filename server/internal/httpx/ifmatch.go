package httpx

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Optimistic concurrency convention (see docs/api/README.md):
//
//   - A resource's version is derived from its updated_at column (microsecond
//     precision, as stored by PostgreSQL) or, where a table has one, its
//     resource_version counter.
//   - Single-resource GETs return it as a strong ETag: "<unix-micro>".
//   - Writes MAY send If-Match with that ETag, or with the RFC3339 updated_at
//     value already present in the JSON body. When present it is enforced:
//     a mismatch returns HTTP 412 / code 41200. "*" means "must exist".
//   - Without If-Match the write behaves exactly as before (last write wins),
//     so existing clients keep working.
//   - Successful conditional writes return the new ETag.

// CodePreconditionFailed is the business code for HTTP 412.
const CodePreconditionFailed = 41200

// PreconditionFailed builds the 412 error.
func PreconditionFailed(msg string) *Error {
	return NewError(http.StatusPreconditionFailed, CodePreconditionFailed, msg)
}

// Version identifies the current state of one resource.
type Version struct {
	// Exists is false when the resource does not exist (yet).
	Exists bool
	// Tag is the opaque version string (no quotes).
	Tag string
	// UpdatedAt, when known, lets clients send the updated_at they already
	// hold instead of the ETag.
	UpdatedAt time.Time
}

// VersionFromTime builds a Version from an updated_at timestamp.
func VersionFromTime(t time.Time) Version {
	return Version{Exists: true, Tag: strconv.FormatInt(t.UTC().UnixMicro(), 10), UpdatedAt: t}
}

// VersionFromCounter builds a Version from an integer resource_version.
func VersionFromCounter(n int64) Version {
	return Version{Exists: true, Tag: "r" + strconv.FormatInt(n, 10)}
}

// ETag returns the header value for v ("" when the resource does not exist).
func (v Version) ETag() string {
	if !v.Exists || v.Tag == "" {
		return ""
	}
	return `"` + v.Tag + `"`
}

// SetETag writes the ETag header for v.
func SetETag(c *gin.Context, v Version) {
	if e := v.ETag(); e != "" {
		c.Header("ETag", e)
	}
}

// IfMatchSatisfied evaluates an If-Match header value against cur.
// An empty header is always satisfied.
func IfMatchSatisfied(header string, cur Version) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return true
	}
	if header == "*" {
		return cur.Exists
	}
	if !cur.Exists {
		return false
	}
	for _, raw := range strings.Split(header, ",") {
		t := strings.TrimSpace(raw)
		t = strings.TrimPrefix(t, "W/")
		t = strings.Trim(t, `"`)
		if t == "" {
			continue
		}
		if t == cur.Tag {
			return true
		}
		if !cur.UpdatedAt.IsZero() {
			if ts, err := time.Parse(time.RFC3339Nano, t); err == nil &&
				ts.UTC().Truncate(time.Microsecond).Equal(cur.UpdatedAt.UTC().Truncate(time.Microsecond)) {
				return true
			}
		}
	}
	return false
}

// CheckIfMatch returns a 412 error when the request carries an If-Match that
// does not match cur.
func CheckIfMatch(c *gin.Context, cur Version) error {
	h := c.GetHeader("If-Match")
	if IfMatchSatisfied(h, cur) {
		return nil
	}
	e := PreconditionFailed("资源已被他人修改，请刷新后重试")
	if cur.Exists {
		return e.WithDetails(gin.H{"current_etag": cur.ETag()})
	}
	return e.WithDetails(gin.H{"current_etag": nil})
}

// VersionFunc loads the current version of the resource addressed by c.
type VersionFunc func(c *gin.Context) (Version, error)

// LockFunc serialises conditional writes on key (e.g. a PostgreSQL advisory
// lock), closing the gap between the If-Match check and the write. It returns
// an unlock func. nil means "no lock".
type LockFunc func(ctx context.Context, key string) (unlock func(), err error)

// IfMatchOptions configures IfMatch.
type IfMatchOptions struct {
	// Load returns the current version (required).
	Load VersionFunc
	// Key identifies the resource for Lock (default: method-independent path).
	Key func(c *gin.Context) string
	// Lock is optional; see LockFunc.
	Lock LockFunc
}

// IfMatch is the reusable optimistic-lock middleware for one resource route:
//
//   - GET/HEAD: sets ETag (the handler still writes the body).
//   - PUT/PATCH/DELETE/POST with If-Match: locks (optional), compares, returns
//     412 on mismatch, otherwise runs the handler and, on 2xx, sets the new
//     ETag before the body is written.
//   - Writes without If-Match pass through unchanged.
func IfMatch(o IfMatchOptions) gin.HandlerFunc {
	key := o.Key
	if key == nil {
		key = func(c *gin.Context) string { return c.Request.URL.Path }
	}
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead:
			if v, err := o.Load(c); err == nil {
				SetETag(c, v)
			}
			c.Next()
			return
		}
		if strings.TrimSpace(c.GetHeader("If-Match")) == "" {
			c.Next()
			return
		}
		if o.Lock != nil {
			unlock, err := o.Lock(c.Request.Context(), key(c))
			if err != nil {
				Fail(c, NewError(http.StatusServiceUnavailable, CodeUnavailable, "资源正忙，请稍后重试").Wrap(err))
				return
			}
			defer unlock()
		}
		cur, err := o.Load(c)
		if err != nil {
			Fail(c, err)
			return
		}
		if err := CheckIfMatch(c, cur); err != nil {
			Fail(c, err)
			return
		}
		w := &etagWriter{ResponseWriter: c.Writer, c: c, load: o.Load}
		c.Writer = w
		c.Next()
		c.Writer = w.ResponseWriter
	}
}

// etagWriter injects the post-write ETag right before the first body byte.
type etagWriter struct {
	gin.ResponseWriter
	c    *gin.Context
	load VersionFunc
	done bool
}

func (w *etagWriter) inject() {
	if w.done {
		return
	}
	w.done = true
	if st := w.ResponseWriter.Status(); st < 200 || st > 299 {
		return
	}
	if v, err := w.load(w.c); err == nil {
		if e := v.ETag(); e != "" {
			w.ResponseWriter.Header().Set("ETag", e)
		} else {
			w.ResponseWriter.Header().Del("ETag")
		}
	}
}

func (w *etagWriter) WriteHeaderNow() { w.inject(); w.ResponseWriter.WriteHeaderNow() }
func (w *etagWriter) Write(b []byte) (int, error) {
	w.inject()
	return w.ResponseWriter.Write(b)
}
func (w *etagWriter) WriteString(s string) (int, error) {
	w.inject()
	return w.ResponseWriter.WriteString(s)
}
