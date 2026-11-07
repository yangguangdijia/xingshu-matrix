package auth

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// RegisterIdentityRoutes mounts the MFA login step (public) and the
// self-service access-token / MFA endpoints (authenticated). It is a no-op
// unless EnableIdentity was called.
func (s *Service) RegisterIdentityRoutes(public, authed *gin.RouterGroup, limiter *RateLimiter) {
	if s.identity == nil {
		return
	}
	public.POST("/auth/mfa/verify", func(c *gin.Context) {
		audit.SetResource(c, "auth", "mfa-verify")
		if limiter != nil {
			if ok, wait := limiter.Allow("login:" + c.ClientIP()); !ok {
				c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
				httpx.Fail(c, httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "请求过于频繁，请稍后再试"))
				return
			}
		}
		var req struct {
			Ticket string `json:"mfa_ticket" binding:"required,max=128"`
			Code   string `json:"code" binding:"required,max=32"`
		}
		if !httpx.Bind(c, &req) {
			return
		}
		res, err := s.CompleteMFA(c.Request.Context(), req.Ticket, req.Code, c.ClientIP(), c.Request.UserAgent())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		u := res.User
		audit.SetUser(c, u.ID, u.Username)
		audit.SetDetail(c, gin.H{"enrolled": len(res.RecoveryCodes) > 0})
		data := gin.H{
			"token": res.Pair,
			"user":  gin.H{"id": u.ID, "username": u.Username, "display_name": u.DisplayName, "must_change_password": u.MustChangePassword},
		}
		if len(res.RecoveryCodes) > 0 {
			data["recovery_codes"] = res.RecoveryCodes
		}
		httpx.OK(c, data)
	})

	// --- personal access tokens ---
	authed.GET("/auth/tokens", func(c *gin.Context) {
		if !requireInteractive(c) {
			return
		}
		ts, err := s.ListAPITokens(c.Request.Context(), rbac.PrincipalFrom(c).UserID)
		respondJSON(c, ts, err)
	})
	authed.POST("/auth/tokens", func(c *gin.Context) {
		audit.SetResource(c, "api_token", "")
		if !requireInteractive(c) {
			return
		}
		var in TokenInput
		if !httpx.Bind(c, &in) {
			return
		}
		p := rbac.PrincipalFrom(c)
		t, err := s.CreateAPIToken(c.Request.Context(), p.UserID, p.UserID, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "api_token", strconv.FormatInt(t.ID, 10))
		audit.SetDetail(c, gin.H{"name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes, "expires_at": t.ExpiresAt})
		httpx.Created(c, t)
	})
	authed.DELETE("/auth/tokens/:id", func(c *gin.Context) {
		if !requireInteractive(c) {
			return
		}
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "api_token", strconv.FormatInt(id, 10))
		audit.SetAction(c, "revoke")
		p := rbac.PrincipalFrom(c)
		if err := s.RevokeAPIToken(c.Request.Context(), p.UserID, id, p.UserID); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id})
	})

	// --- MFA self-service ---
	type codeReq struct {
		Code string `json:"code" binding:"required,max=32"`
	}
	authed.GET("/auth/mfa", func(c *gin.Context) {
		if !requireInteractive(c) {
			return
		}
		st, err := s.MFAStatusOf(c.Request.Context(), rbac.PrincipalFrom(c).UserID)
		respondJSON(c, st, err)
	})
	authed.POST("/auth/mfa/setup", func(c *gin.Context) {
		audit.SetResource(c, "mfa", "setup")
		if !requireInteractive(c) {
			return
		}
		st, err := s.BeginMFASetup(c.Request.Context(), rbac.PrincipalFrom(c))
		respondJSON(c, st, err)
	})
	authed.POST("/auth/mfa/activate", func(c *gin.Context) {
		audit.SetResource(c, "mfa", "activate")
		if !requireInteractive(c) {
			return
		}
		var req codeReq
		if !httpx.Bind(c, &req) {
			return
		}
		codes, err := s.ActivateMFASetup(c.Request.Context(), rbac.PrincipalFrom(c), req.Code)
		respondJSON(c, gin.H{"enabled": true, "recovery_codes": codes}, err)
	})
	authed.POST("/auth/mfa/disable", func(c *gin.Context) {
		audit.SetResource(c, "mfa", "disable")
		if !requireInteractive(c) {
			return
		}
		var req codeReq
		if !httpx.Bind(c, &req) {
			return
		}
		err := s.DisableMFA(c.Request.Context(), rbac.PrincipalFrom(c), req.Code)
		respondJSON(c, gin.H{"enabled": false}, err)
	})
	authed.POST("/auth/mfa/recovery-codes", func(c *gin.Context) {
		audit.SetResource(c, "mfa", "recovery-codes")
		if !requireInteractive(c) {
			return
		}
		var req codeReq
		if !httpx.Bind(c, &req) {
			return
		}
		codes, err := s.RegenerateRecoveryCodes(c.Request.Context(), rbac.PrincipalFrom(c), req.Code)
		respondJSON(c, gin.H{"recovery_codes": codes}, err)
	})

	// --- MFA enforcement policy (security settings) ---
	authed.GET("/auth/mfa/policy", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		p, err := s.LoadMFAPolicy(c.Request.Context())
		respondJSON(c, p, err)
	})
	authed.PUT("/auth/mfa/policy", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		audit.SetResource(c, "settings", MFAPolicyKey)
		var in MFAPolicy
		if !httpx.Bind(c, &in) {
			return
		}
		p, err := s.SaveMFAPolicy(c.Request.Context(), in, rbac.PrincipalFrom(c).UserID)
		if err == nil {
			audit.SetDetail(c, p)
		}
		respondJSON(c, p, err)
	})
}

func respondJSON(c *gin.Context, v any, err error) {
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}
