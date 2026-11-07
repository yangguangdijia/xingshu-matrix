package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Middleware authenticates "Authorization: Bearer <access token>" (or
// ?access_token= for EventSource / WebSocket clients that cannot set headers).
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := ""
		if h := c.GetHeader("Authorization"); h != "" {
			scheme, v, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				httpx.Fail(c, httpx.Unauthorized("Authorization header must be Bearer"))
				return
			}
			tok = strings.TrimSpace(v)
		} else {
			tok = c.Query("access_token")
		}
		if tok == "" {
			httpx.Fail(c, httpx.Unauthorized("authentication required"))
			return
		}
		var p *rbac.Principal
		var err error
		if IsAPIToken(tok) {
			// API 令牌只接受 Authorization 头，避免出现在 URL / 访问日志里
			if c.GetHeader("Authorization") == "" {
				httpx.Fail(c, httpx.Unauthorized("访问令牌只能通过 Authorization: Bearer 头传递"))
				return
			}
			p, err = s.AuthenticateAPIToken(c.Request.Context(), tok, c.ClientIP())
		} else {
			p, err = s.Authenticate(c.Request.Context(), tok)
		}
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		rbac.SetPrincipal(c, p)
		c.Next()
	}
}

// requireInteractive rejects API-token callers on identity-sensitive endpoints
// (password, sessions, tokens, MFA): a leaked token must not be able to mint
// more tokens or take over the account.
func requireInteractive(c *gin.Context) bool {
	p := rbac.PrincipalFrom(c)
	if p == nil {
		httpx.Fail(c, httpx.Unauthorized("authentication required"))
		return false
	}
	if p.ViaToken() || p.SessionID == "" {
		httpx.Fail(c, httpx.Forbidden("该操作需要登录后在页面中进行，访问令牌不能调用"))
		return false
	}
	return true
}

// passwordGateAllowed lists the routes reachable while a password change is pending.
var passwordGateAllowed = map[string]bool{
	"GET /api/v1/auth/me":         true,
	"PUT /api/v1/auth/password":   true,
	"POST /api/v1/auth/logout":    true,
	"GET /api/v1/auth/sessions":   true,
	"GET /api/v1/settings/public": true,
}

// PasswordChangeGate blocks everything except the routes above while the user
// must change their password (first admin login, admin-reset passwords).
func PasswordChangeGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := rbac.PrincipalFrom(c)
		if p != nil && p.MustChangePassword && !passwordGateAllowed[c.Request.Method+" "+c.FullPath()] {
			httpx.Fail(c, httpx.NewError(http.StatusForbidden, httpx.CodePasswordChangeRequired, "首次登录或密码已被重置，请先修改密码"))
			return
		}
		c.Next()
	}
}

type loginReq struct {
	Username string `json:"username" binding:"required,max=64"`
	Password string `json:"password" binding:"required,max=128"`
}

// RegisterPublic mounts the unauthenticated endpoints (login, refresh).
func (s *Service) RegisterPublic(r *gin.RouterGroup, limiter *RateLimiter) {
	r.POST("/auth/login", func(c *gin.Context) {
		audit.SetResource(c, "auth", "login")
		if limiter != nil {
			if ok, wait := limiter.Allow("login:" + c.ClientIP()); !ok {
				c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
				httpx.Fail(c, httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "登录请求过于频繁，请稍后再试"))
				return
			}
		}
		var req loginReq
		if !httpx.Bind(c, &req) {
			return
		}
		audit.SetUsername(c, req.Username)
		pair, u, err := s.Login(c.Request.Context(), strings.TrimSpace(req.Username), req.Password, c.ClientIP(), c.Request.UserAgent())
		var mfa *MFARequiredError
		if errors.As(err, &mfa) {
			// 密码正确，等待第二因素：不发会话，只返回短期挑战票据
			audit.SetUser(c, u.ID, u.Username)
			audit.SetDetail(c, gin.H{"mfa": "pending", "enroll": mfa.Enroll})
			httpx.OK(c, gin.H{
				"mfa_required": true, "mfa_ticket": mfa.Ticket, "mfa_enroll": mfa.Enroll, "expires_in": mfa.ExpiresIn,
				"secret": mfa.Secret, "otpauth_uri": mfa.OTPAuthURI,
			})
			return
		}
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetUser(c, u.ID, u.Username)
		httpx.OK(c, gin.H{
			"token": pair,
			"user":  gin.H{"id": u.ID, "username": u.Username, "display_name": u.DisplayName, "must_change_password": u.MustChangePassword},
		})
	})
	r.POST("/auth/refresh", func(c *gin.Context) {
		audit.SetResource(c, "auth", "refresh")
		var req struct {
			RefreshToken string `json:"refresh_token" binding:"required"`
		}
		if !httpx.Bind(c, &req) {
			return
		}
		pair, err := s.Refresh(c.Request.Context(), req.RefreshToken)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, pair)
	})
}

// RegisterRoutes mounts the authenticated self-service endpoints.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/auth/logout", func(c *gin.Context) {
		audit.SetResource(c, "auth", "logout")
		p := rbac.PrincipalFrom(c)
		if !requireInteractive(c) {
			return
		}
		if err := s.Logout(c.Request.Context(), p.SessionID); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, nil)
	})
	r.GET("/auth/me", func(c *gin.Context) { httpx.OK(c, rbac.PrincipalFrom(c)) })
	r.PUT("/auth/password", func(c *gin.Context) {
		audit.SetResource(c, "auth", "password")
		if !requireInteractive(c) {
			return
		}
		var req struct {
			OldPassword string `json:"old_password" binding:"required,max=128"`
			NewPassword string `json:"new_password" binding:"required,max=128"`
		}
		if !httpx.Bind(c, &req) {
			return
		}
		if err := s.ChangePassword(c.Request.Context(), rbac.PrincipalFrom(c), req.OldPassword, req.NewPassword); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"must_change_password": false})
	})
	r.GET("/auth/sessions", func(c *gin.Context) {
		if !requireInteractive(c) {
			return
		}
		ss, err := s.Sessions(c.Request.Context(), rbac.PrincipalFrom(c))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, ss)
	})
	r.DELETE("/auth/sessions/:sid", func(c *gin.Context) {
		audit.SetResource(c, "session", c.Param("sid"))
		if !requireInteractive(c) {
			return
		}
		if err := s.RevokeOwnSession(c.Request.Context(), rbac.PrincipalFrom(c), c.Param("sid")); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, nil)
	})
}
