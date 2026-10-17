// Package audit records every write operation (and any explicit event a module
// reports) into audit_logs and exposes a query API.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Result values.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// Entry is one audit record.
type Entry struct {
	ID         int64           `json:"id"`
	UserID     *int64          `json:"user_id"`
	Username   string          `json:"username"`
	IP         string          `json:"ip"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Resource   string          `json:"resource"`
	ResourceID string          `json:"resource_id"`
	Action     string          `json:"action"`
	StatusCode int             `json:"status_code"`
	Result     string          `json:"result"`
	DurationMs int             `json:"duration_ms"`
	RequestID  string          `json:"request_id"`
	UserAgent  string          `json:"user_agent"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Writer persists batches of entries.
type Writer interface {
	WriteBatch(ctx context.Context, entries []Entry) error
}

// Recorder buffers entries and writes them in batches in the background. When
// the buffer is full it writes synchronously, so entries are never dropped.
type Recorder struct {
	w       Writer
	log     *slog.Logger
	ch      chan Entry
	done    chan struct{}
	closeMu sync.Once
	wg      sync.WaitGroup
}

// NewRecorder starts the background writer.
func NewRecorder(w Writer, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	r := &Recorder{w: w, log: log, ch: make(chan Entry, 4096), done: make(chan struct{})}
	r.wg.Add(1)
	go r.loop()
	return r
}

// Record enqueues an entry (CreatedAt defaults to now).
func (r *Recorder) Record(ctx context.Context, e Entry) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	if e.Result == "" {
		e.Result = ResultSuccess
	}
	select {
	case <-r.done:
		r.writeNow([]Entry{e})
	case r.ch <- e:
	default:
		r.writeNow([]Entry{e})
	}
}

func (r *Recorder) writeNow(batch []Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.w.WriteBatch(ctx, batch); err != nil {
		// last resort: never lose an audit record silently
		for _, e := range batch {
			b, _ := json.Marshal(e)
			r.log.Error("audit write failed", "error", err, "entry", string(b))
		}
	}
}

func (r *Recorder) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]Entry, 0, 100)
	flush := func() {
		if len(batch) > 0 {
			r.writeNow(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.done:
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close flushes pending entries and stops the writer.
func (r *Recorder) Close() {
	r.closeMu.Do(func() { close(r.done) })
	r.wg.Wait()
}

// gin context keys used by handlers to enrich the automatic entry.
const (
	keyResource   = "audit.resource"
	keyResourceID = "audit.resource_id"
	keyAction     = "audit.action"
	keyDetail     = "audit.detail"
	keyUserID     = "audit.user_id"
	keyUsername   = "audit.username"
	keySkip       = "audit.skip"
)

// SetResource overrides the derived resource and id of the current request.
func SetResource(c *gin.Context, resource, id string) {
	c.Set(keyResource, resource)
	c.Set(keyResourceID, id)
}

// SetAction overrides the derived action ("create", "approve", ...).
func SetAction(c *gin.Context, action string) { c.Set(keyAction, action) }

// SetDetail attaches a JSON-serialisable detail (never put secrets here).
func SetDetail(c *gin.Context, v any) { c.Set(keyDetail, v) }

// SetUsername records the claimed username for unauthenticated requests (login).
func SetUsername(c *gin.Context, name string) { c.Set(keyUsername, name) }

// SetUser records the user for requests that authenticate inside the handler.
func SetUser(c *gin.Context, id int64, name string) {
	c.Set(keyUserID, id)
	c.Set(keyUsername, name)
}

// Skip disables the automatic entry for this request.
func Skip(c *gin.Context) { c.Set(keySkip, true) }

// Middleware records every non-read request (POST/PUT/PATCH/DELETE) after it completes.
func (r *Recorder) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		start := time.Now()
		c.Next()
		if c.GetBool(keySkip) {
			return
		}
		r.Record(c.Request.Context(), FromGin(c, start))
	}
}

// FromGin builds an entry for the current (completed) request.
func FromGin(c *gin.Context, start time.Time) Entry {
	status := c.Writer.Status()
	e := Entry{
		IP:         c.ClientIP(),
		Method:     c.Request.Method,
		Path:       c.Request.URL.Path,
		StatusCode: status,
		Result:     ResultSuccess,
		DurationMs: int(time.Since(start).Milliseconds()),
		RequestID:  httpx.RequestIDFrom(c),
		UserAgent:  trunc(c.Request.UserAgent(), 512),
		CreatedAt:  start,
	}
	if status >= 400 {
		e.Result = ResultFailure
	}
	if p := rbac.PrincipalFrom(c); p != nil {
		id := p.UserID
		e.UserID, e.Username = &id, p.Username
	}
	if v, ok := c.Get(keyUserID); ok && e.UserID == nil {
		if id, ok := v.(int64); ok {
			e.UserID = &id
		}
	}
	if e.Username == "" {
		e.Username = trunc(c.GetString(keyUsername), 128)
	}
	e.Resource, e.ResourceID, e.Action = DeriveResource(c.Request.Method, c.Request.URL.Path)
	if v := c.GetString(keyResource); v != "" {
		e.Resource = v
		e.ResourceID = c.GetString(keyResourceID)
	}
	if v := c.GetString(keyAction); v != "" {
		e.Action = v
	}
	detail := map[string]any{}
	if v, ok := c.Get(keyDetail); ok {
		detail["data"] = v
	}
	if len(c.Errors) > 0 {
		detail["error"] = trunc(c.Errors.String(), 2000)
	}
	if len(detail) > 0 {
		e.Detail, _ = json.Marshal(detail)
	}
	return e
}

// DeriveResource maps /api/v1/<resource>[/<id>[/<sub>...]] to (resource, id, action).
func DeriveResource(method, path string) (resource, id, action string) {
	p := strings.TrimPrefix(path, "/api/v1/")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) > 0 {
		resource = parts[0]
	}
	if len(parts) > 1 {
		id = parts[1]
	}
	switch method {
	case http.MethodPost:
		action = "create"
	case http.MethodPut, http.MethodPatch:
		action = "update"
	case http.MethodDelete:
		action = "delete"
	default:
		action = strings.ToLower(method)
	}
	if len(parts) > 2 {
		action = parts[len(parts)-1]
	}
	return trunc(resource, 64), trunc(id, 128), trunc(action, 64)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
