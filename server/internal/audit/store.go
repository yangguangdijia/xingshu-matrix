package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// PgStore writes and queries audit_logs.
type PgStore struct{ DB store.DB }

// WriteBatch inserts entries with COPY-like batching.
func (s *PgStore) WriteBatch(ctx context.Context, entries []Entry) error {
	b := &pgx.Batch{}
	for _, e := range entries {
		var detail any
		if len(e.Detail) > 0 {
			detail = string(e.Detail)
		}
		b.Queue(`INSERT INTO audit_logs(user_id, username, ip, method, path, resource, resource_id, action,
			status_code, result, duration_ms, request_id, user_agent, detail, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15)`,
			e.UserID, e.Username, e.IP, e.Method, e.Path, e.Resource, e.ResourceID, e.Action,
			e.StatusCode, e.Result, e.DurationMs, e.RequestID, e.UserAgent, detail, e.CreatedAt)
	}
	return s.DB.SendBatch(ctx, b).Close()
}

// Filter narrows an audit query. Zero values are ignored.
type Filter struct {
	UserID   int64
	Username string // prefix match, case-insensitive
	Resource string
	Action   string
	Method   string
	Result   string
	Path     string // prefix match
	IP       string
	Keyword  string // matches path / resource_id / username
	From, To time.Time
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.UserID > 0 {
		add("user_id = ?", f.UserID)
	}
	if f.Username != "" {
		add("lower(username) LIKE lower(?::text) || '%'", escapeLike(f.Username))
	}
	if f.Resource != "" {
		add("resource = ?", f.Resource)
	}
	if f.Action != "" {
		add("action = ?", f.Action)
	}
	if f.Method != "" {
		add("method = ?", strings.ToUpper(f.Method))
	}
	if f.Result != "" {
		add("result = ?", f.Result)
	}
	if f.Path != "" {
		add("path LIKE ?::text || '%'", escapeLike(f.Path))
	}
	if f.IP != "" {
		add("ip = ?", f.IP)
	}
	if f.Keyword != "" {
		add("(path ILIKE '%' || ?::text || '%' OR resource_id ILIKE '%' || ?::text || '%' OR username ILIKE '%' || ?::text || '%')", escapeLike(f.Keyword))
	}
	if !f.From.IsZero() {
		add("created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("created_at < ?", f.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

const entryCols = `id, user_id, username, ip, method, path, resource, resource_id, action, status_code, result,
	duration_ms, request_id, user_agent, detail, created_at`

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	var detail []byte
	err := row.Scan(&e.ID, &e.UserID, &e.Username, &e.IP, &e.Method, &e.Path, &e.Resource, &e.ResourceID, &e.Action,
		&e.StatusCode, &e.Result, &e.DurationMs, &e.RequestID, &e.UserAgent, &detail, &e.CreatedAt)
	e.Detail = detail
	return e, err
}

// Query returns a page of entries, newest first.
func (s *PgStore) Query(ctx context.Context, f Filter, p httpx.Page) ([]Entry, int64, error) {
	where, args := f.where()
	var total int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM audit_logs%s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		entryCols, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Get returns one entry.
func (s *PgStore) Get(ctx context.Context, id int64) (*Entry, error) {
	e, err := scanEntry(s.DB.QueryRow(ctx, `SELECT `+entryCols+` FROM audit_logs WHERE id=$1`, id))
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &e, err
}

// FilterFromQuery parses filter query params.
func FilterFromQuery(c *gin.Context) (Filter, error) {
	f := Filter{
		Username: c.Query("username"), Resource: c.Query("resource"), Action: c.Query("action"),
		Method: c.Query("method"), Result: c.Query("result"), Path: c.Query("path"), IP: c.Query("ip"),
		Keyword: c.Query("q"),
	}
	if v := c.Query("user_id"); v != "" {
		if _, err := fmt.Sscan(v, &f.UserID); err != nil {
			return f, httpx.Validation("invalid user_id")
		}
	}
	if f.Result != "" && f.Result != ResultSuccess && f.Result != ResultFailure {
		return f, httpx.Validation("result must be success or failure")
	}
	var err error
	if f.From, err = httpx.QueryTime(c, "from"); err != nil {
		return f, err
	}
	if f.To, err = httpx.QueryTime(c, "to"); err != nil {
		return f, err
	}
	return f, nil
}

// RegisterRoutes mounts the audit query API.
func (s *PgStore) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/audit-logs", rbac.RequirePerm("audit:read"), func(c *gin.Context) {
		f, err := FilterFromQuery(c)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		p := httpx.ParsePage(c)
		items, total, err := s.Query(c.Request.Context(), f, p)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, p))
	})
	r.GET("/audit-logs/:id", rbac.RequirePerm("audit:read"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		e, err := s.Get(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, e)
	})
}
