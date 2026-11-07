package platformops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

const (
	// HeaderIdempotencyKey is the request header.
	HeaderIdempotencyKey = "Idempotency-Key"
	// HeaderIdempotentReplayed marks a replayed response.
	HeaderIdempotentReplayed = "Idempotent-Replayed"
	// CodeIdempotencyMismatch: same key, different request (HTTP 422).
	CodeIdempotencyMismatch = 42200

	idemMaxBody  = 1 << 20         // request bytes hashed / response bytes stored
	idemLease    = 5 * time.Minute // a "processing" row older than this can be taken over
	idemMaxKey   = 255
	idemStoreTTL = 5 * time.Second // store calls after the handler ran
)

// IdemRecord is one idempotency_keys row.
type IdemRecord struct {
	Scope       string
	Route       string
	Key         string
	RequestHash string
	State       string // "processing" | "done"
	Status      int
	ContentType string
	Body        []byte
	ExpiresAt   time.Time
}

// IdempotencyStore persists idempotency records.
type IdempotencyStore interface {
	// Begin claims (scope, route, key) for a new request. It returns nil when
	// the claim succeeded (a fresh, expired or lease-expired row), otherwise
	// the existing record.
	Begin(ctx context.Context, rec IdemRecord, lease time.Duration) (*IdemRecord, error)
	// Complete stores the response of a claimed key.
	Complete(ctx context.Context, rec IdemRecord) error
	// Release drops a claim whose response is not replayable.
	Release(ctx context.Context, scope, route, key string) error
}

func validIdemKey(k string) bool {
	if k == "" || len(k) > idemMaxKey {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func isStreamingRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

// Idempotency implements Idempotency-Key for authenticated writes
// (POST/PUT/PATCH/DELETE). Mount after Auth.Middleware.
//
//   - the key is scoped by caller (user) and method + path; the request hash
//     covers method, path, query and the first 1 MiB of the body;
//   - same key while the first request is still running → 409 (the claim is a
//     5-minute lease, after which a crashed attempt can be retried);
//   - same key with a different request → 422 (code 42200);
//   - a completed request is replayed byte-for-byte with
//     Idempotent-Replayed: true (and not audited again);
//   - 5xx responses, responses > 1 MiB, SSE and WebSocket are not stored, so
//     the client may retry them with the same key.
//
// Without the header, or without a store, requests pass through unchanged.
func (o *Ops) Idempotency() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		key := strings.TrimSpace(c.GetHeader(HeaderIdempotencyKey))
		if key == "" || o.idem == nil || isStreamingRequest(c.Request) {
			c.Next()
			return
		}
		if !validIdemKey(key) {
			httpx.Fail(c, httpx.BadRequest("Idempotency-Key 必须为 1～255 个可见 ASCII 字符"))
			return
		}
		p := rbac.PrincipalFrom(c)
		if p == nil || p.UserID <= 0 {
			c.Next()
			return
		}
		scope := "u:" + strconv.FormatInt(p.UserID, 10)
		route := c.Request.Method + " " + c.Request.URL.Path

		reqHash, err := hashRequest(c.Request)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("读取请求体失败"))
			return
		}
		ctx := c.Request.Context()
		existing, err := o.idem.Begin(ctx, IdemRecord{Scope: scope, Route: route, Key: key, RequestHash: reqHash,
			State: "processing", ExpiresAt: time.Now().Add(o.cfg.IdempotencyTTL)}, idemLease)
		if err != nil {
			if isUndefinedTable(err) { // migration 0010 not applied yet
				c.Next()
				return
			}
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		if existing != nil {
			switch {
			case existing.RequestHash != reqHash:
				audit.Skip(c)
				httpx.Fail(c, httpx.NewError(http.StatusUnprocessableEntity, CodeIdempotencyMismatch, "该幂等键已用于内容不同的请求"))
			case existing.State != "done":
				audit.Skip(c)
				c.Header("Retry-After", "1")
				httpx.Fail(c, httpx.Conflict("相同请求正在处理中，请稍后重试"))
			default:
				audit.Skip(c)
				replay(c, existing)
			}
			return
		}

		rec := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = rec
		finished := false
		defer func() {
			if finished {
				return
			}
			// Panic or abort before we could decide: free the key.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idemStoreTTL)
			defer cancel()
			_ = o.idem.Release(sctx, scope, route, key)
		}()
		c.Next()
		c.Writer = rec.ResponseWriter

		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idemStoreTTL)
		defer cancel()
		status := rec.Status()
		ctype := rec.Header().Get("Content-Type")
		storable := status < 500 && status != http.StatusSwitchingProtocols && !rec.overflow &&
			!strings.Contains(strings.ToLower(ctype), "text/event-stream")
		if storable {
			err = o.idem.Complete(sctx, IdemRecord{Scope: scope, Route: route, Key: key, RequestHash: reqHash,
				State: "done", Status: status, ContentType: ctype, Body: rec.buf.Bytes()})
		} else {
			err = o.idem.Release(sctx, scope, route, key)
		}
		if err != nil {
			o.log.Warn("idempotency record not saved", "route", route, "err", err)
		}
		finished = true
	}
}

func replay(c *gin.Context, r *IdemRecord) {
	if r.ContentType != "" {
		c.Header("Content-Type", r.ContentType)
	}
	c.Header(HeaderIdempotentReplayed, "true")
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	c.Status(status)
	if len(r.Body) > 0 {
		_, _ = c.Writer.Write(r.Body)
	} else {
		c.Writer.WriteHeaderNow()
	}
	c.Abort()
}

// hashRequest digests method, path, query and the first 1 MiB of the body,
// restoring the body for the handler.
func hashRequest(r *http.Request) (string, error) {
	h := sha256.New()
	io.WriteString(h, r.Method+"\n"+r.URL.Path+"\n"+r.URL.RawQuery+"\n")
	if r.Body != nil && r.Body != http.NoBody {
		head, err := io.ReadAll(io.LimitReader(r.Body, idemMaxBody))
		if err != nil {
			return "", err
		}
		h.Write(head)
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// captureWriter tees the response body (up to 1 MiB) for storage.
type captureWriter struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	overflow bool
}

func (w *captureWriter) keep(b []byte) {
	if w.overflow {
		return
	}
	if w.buf.Len()+len(b) > idemMaxBody {
		w.overflow = true
		w.buf.Reset()
		return
	}
	w.buf.Write(b)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.keep(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.keep([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// Flush marks streamed responses as not replayable.
func (w *captureWriter) Flush() {
	w.overflow = true
	w.buf.Reset()
	w.ResponseWriter.Flush()
}

// pgIdempotencyStore is the PostgreSQL IdempotencyStore (table idempotency_keys, 0010).
type pgIdempotencyStore struct{ db store.DB }

func (s *pgIdempotencyStore) Begin(ctx context.Context, rec IdemRecord, lease time.Duration) (*IdemRecord, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var claimed bool
		err := s.db.QueryRow(ctx, `INSERT INTO idempotency_keys(scope, route, idem_key, request_hash, state, expires_at)
			VALUES ($1,$2,$3,$4,'processing',$5)
			ON CONFLICT (scope, route, idem_key) DO UPDATE SET
			  request_hash=EXCLUDED.request_hash, state='processing', response_status=0, content_type='',
			  response_body=NULL, created_at=now(), expires_at=EXCLUDED.expires_at
			WHERE idempotency_keys.expires_at < now()
			   OR (idempotency_keys.state='processing' AND idempotency_keys.created_at < now() - make_interval(secs => $6))
			RETURNING true`, rec.Scope, rec.Route, rec.Key, rec.RequestHash, rec.ExpiresAt, lease.Seconds()).Scan(&claimed)
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, errNoRows) {
			return nil, err
		}
		var ex IdemRecord
		err = s.db.QueryRow(ctx, `SELECT request_hash, state, response_status, content_type, COALESCE(response_body, ''::bytea), expires_at
			FROM idempotency_keys WHERE scope=$1 AND route=$2 AND idem_key=$3`, rec.Scope, rec.Route, rec.Key).
			Scan(&ex.RequestHash, &ex.State, &ex.Status, &ex.ContentType, &ex.Body, &ex.ExpiresAt)
		if errors.Is(err, errNoRows) {
			continue // released between the two statements; claim again
		}
		if err != nil {
			return nil, err
		}
		ex.Scope, ex.Route, ex.Key = rec.Scope, rec.Route, rec.Key
		return &ex, nil
	}
	return nil, errors.New("idempotency: could not claim key")
}

func (s *pgIdempotencyStore) Complete(ctx context.Context, rec IdemRecord) error {
	_, err := s.db.Exec(ctx, `UPDATE idempotency_keys SET state='done', response_status=$4, content_type=$5, response_body=$6
		WHERE scope=$1 AND route=$2 AND idem_key=$3 AND state='processing'`,
		rec.Scope, rec.Route, rec.Key, rec.Status, rec.ContentType, rec.Body)
	return err
}

func (s *pgIdempotencyStore) Release(ctx context.Context, scope, route, key string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE scope=$1 AND route=$2 AND idem_key=$3 AND state='processing'`,
		scope, route, key)
	return err
}
