package users

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// mfaAdmin is the optional auth capability for administrator MFA reset.
type mfaAdmin interface {
	ResetMFA(ctx context.Context, userID int64) error
}

// mfaStatusReader is the optional auth capability for MFA flags in admin lists.
type mfaStatusReader interface {
	MFAEnabledFor(ctx context.Context, userID int64) (bool, error)
}

func errMFAUnsupported() *httpx.Error {
	return httpx.NewError(http.StatusNotImplemented, 50100, "当前认证服务不支持两步验证管理")
}

// ResetMFA removes a user's MFA enrolment (administrator action).
func (s *Service) ResetMFA(ctx context.Context, id int64) error {
	m, ok := s.Sessions.(mfaAdmin)
	if !ok {
		return errMFAUnsupported()
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := m.ResetMFA(ctx, id); err != nil {
		return err
	}
	s.Sessions.InvalidateUser(id)
	return nil
}

// MFAFlags reports, per user id, whether MFA is enabled.
func (s *Service) MFAFlags(ctx context.Context, ids []int64) (map[int64]bool, error) {
	m, ok := s.Sessions.(mfaStatusReader)
	if !ok {
		return nil, errMFAUnsupported()
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		on, err := m.MFAEnabledFor(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = on
	}
	return out, nil
}

// registerMFAAdmin mounts the MFA admin endpoints under /users.
func (s *Service) registerMFAAdmin(r *gin.RouterGroup, read, manage gin.HandlerFunc) {
	// GET /users/mfa-flags?ids=1,2,3 → {"1":true,"2":false,...} for the user list.
	r.GET("/users/mfa-flags", read, func(c *gin.Context) {
		var ids []int64
		for _, p := range strings.Split(c.Query("ids"), ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil || id <= 0 {
				httpx.Fail(c, httpx.Validation("ids 格式错误"))
				return
			}
			ids = append(ids, id)
		}
		if len(ids) > 200 {
			httpx.Fail(c, httpx.Validation("ids 最多 200 个"))
			return
		}
		flags, err := s.MFAFlags(c.Request.Context(), ids)
		respond(c, flags, err)
	})
	r.DELETE("/users/:id/mfa", manage, withID(func(c *gin.Context, id int64) {
		audit.SetResource(c, "users", fmt.Sprint(id))
		audit.SetDetail(c, gin.H{"action": "reset_mfa"})
		if err := s.ResetMFA(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id, "mfa_enabled": false})
	}))
}
