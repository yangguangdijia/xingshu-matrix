package platformops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// Ticket is one stored one-time connection ticket. Only the SHA-256 digest of
// the ticket is stored; TokenEnc is the caller's access token sealed with the
// platform cipher so redemption reuses the original session's authentication.
type Ticket struct {
	Hash      string
	UserID    int64
	SessionID string
	TokenEnc  string
	ExpiresAt time.Time
}

// TicketStore persists tickets.
type TicketStore interface {
	Create(ctx context.Context, t Ticket) error
	// Redeem atomically deletes the ticket and returns it; nil (no error) when
	// it does not exist or has expired. A ticket can be redeemed at most once.
	Redeem(ctx context.Context, hash string) (*Ticket, error)
}

const (
	ticketBytes  = 32
	ticketLen    = 43 // base64url(32 bytes) without padding
	ctxTicketKey = "platformops.ws_ticket"
)

// newTicket returns a random ticket and its storage digest.
func newTicket() (plain, hash string, err error) {
	b := make([]byte, ticketBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, hashTicket(plain), nil
}

func hashTicket(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func validTicketFormat(t string) bool {
	if len(t) != ticketLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(t)
	return err == nil
}

// bearerFrom returns the access token this request authenticated with.
func bearerFrom(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		if scheme, v, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(v)
		}
		return ""
	}
	return c.Query("access_token")
}

// RegisterRoutes mounts the authenticated platformops endpoints:
//
//	POST /auth/ws-ticket  → {ticket, expires_in, expires_at}
func (o *Ops) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/auth/ws-ticket", o.issueTicket)
}

func (o *Ops) issueTicket(c *gin.Context) {
	// Issued on every terminal open; not worth an audit row (the connection
	// itself is audited by the module that serves it).
	audit.Skip(c)
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	if p.ViaToken() || p.SessionID == "" {
		httpx.Fail(c, httpx.Forbidden("连接票据仅供页面登录会话使用，访问令牌请直接使用 Authorization 头"))
		return
	}
	if o.tickets == nil || o.seal == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "连接票据服务不可用"))
		return
	}
	tok := bearerFrom(c)
	if tok == "" {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return
	}
	enc, err := o.seal.Encrypt(tok)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	plain, hash, err := newTicket()
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	ttl := o.cfg.WSTicketTTL
	exp := time.Now().Add(ttl)
	if err := o.tickets.Create(c.Request.Context(), Ticket{Hash: hash, UserID: p.UserID, SessionID: p.SessionID, TokenEnc: enc, ExpiresAt: exp}); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	c.Header("Cache-Control", "no-store")
	httpx.OK(c, gin.H{"ticket": plain, "expires_in": int(ttl / time.Second), "expires_at": exp.UTC().Format(time.RFC3339)})
}

// WSTicketAuth redeems "?ticket=" on GET requests (WebSocket / EventSource
// clients cannot set headers). It must run before Auth.Middleware:
//
//   - the ticket is deleted atomically (one-time), expired ones are refused;
//   - the sealed token is decrypted and re-authenticated, and the resulting
//     principal must match the ticket's user and session (a logged-out or
//     revoked session cannot be resumed through an outstanding ticket);
//   - the ticket is removed from the URL and Authorization: Bearer is set, so
//     the regular auth middleware and handlers see a normal request.
//
// Requests without a ticket pass through unchanged. If an Authorization
// header is present the ticket is ignored (stripped, not redeemed).
func (o *Ops) WSTicketAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Next()
			return
		}
		q := c.Request.URL.Query()
		if !q.Has("ticket") {
			c.Next()
			return
		}
		t := q.Get("ticket")
		q.Del("ticket")
		c.Request.URL.RawQuery = q.Encode()
		if c.GetHeader("Authorization") != "" {
			c.Next()
			return
		}
		if !validTicketFormat(t) || o.tickets == nil || o.seal == nil || o.auth == nil {
			httpx.Fail(c, httpx.Unauthorized("连接票据无效或已过期"))
			return
		}
		tk, err := o.tickets.Redeem(c.Request.Context(), hashTicket(t))
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		if tk == nil {
			httpx.Fail(c, httpx.Unauthorized("连接票据无效或已过期"))
			return
		}
		tok, err := o.seal.Decrypt(tk.TokenEnc)
		if err != nil || tok == "" {
			httpx.Fail(c, httpx.Unauthorized("连接票据无效或已过期"))
			return
		}
		p, err := o.auth.Authenticate(c.Request.Context(), tok)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		if p == nil || p.UserID != tk.UserID || p.SessionID != tk.SessionID {
			httpx.Fail(c, httpx.Unauthorized("连接票据与当前会话不匹配"))
			return
		}
		c.Request.Header.Set("Authorization", "Bearer "+tok)
		c.Set(ctxTicketKey, true)
		c.Next()
	}
}

// ViaTicket reports whether the request was authenticated by a ws ticket.
func ViaTicket(c *gin.Context) bool { return c.GetBool(ctxTicketKey) }

// pgTicketStore is the PostgreSQL TicketStore (table ws_tickets, 0010).
type pgTicketStore struct{ db store.DB }

func (s *pgTicketStore) Create(ctx context.Context, t Ticket) error {
	_, err := s.db.Exec(ctx, `INSERT INTO ws_tickets(ticket_hash, user_id, session_id, token_enc, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		t.Hash, t.UserID, t.SessionID, t.TokenEnc, t.ExpiresAt)
	return err
}

func (s *pgTicketStore) Redeem(ctx context.Context, hash string) (*Ticket, error) {
	var t Ticket
	var valid bool
	err := s.db.QueryRow(ctx, `DELETE FROM ws_tickets WHERE ticket_hash=$1
		RETURNING user_id, session_id, token_enc, expires_at, expires_at > now()`, hash).
		Scan(&t.UserID, &t.SessionID, &t.TokenEnc, &t.ExpiresAt, &valid)
	if errors.Is(err, errNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, nil
	}
	t.Hash = hash
	return &t, nil
}
