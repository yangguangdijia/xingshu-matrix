// Package recovery 提供系统邮件配置（SMTP）与自助找回密码。
//
// 找回流程：登录页「忘记密码」→ POST /auth/password/forgot {account}（用户名或邮箱）
// → 账号存在、启用、有邮箱且不是 SSO 账号时，发送含一次性链接的邮件（30 分钟有效）
// → 用户打开 {site_url}/#/reset-password?token=... → POST /auth/password/reset 设置新密码，
// 解除锁定并撤销该用户全部会话。无论账号是否存在，forgot 都返回同样的结果，避免探测账号。
package recovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/mail"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// TokenTTL 是重置链接有效期。
const TokenTTL = 30 * time.Minute

// Service 提供 SMTP 设置与找回密码路由。
type Service struct {
	DB       store.DB
	Settings *settings.Service
	Auth     *auth.Service
	Limiter  *auth.RateLimiter
	Log      *slog.Logger
	// Send 可在测试中替换；默认 mail.Send。
	Send func(ctx context.Context, cfg mail.Config, to []string, subject, body string) error
}

// Register 挂载路由。
func (s *Service) Register(public, authed *gin.RouterGroup) {
	if s.Send == nil {
		s.Send = mail.Send
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	public.GET("/auth/password/forgot", s.available)
	public.POST("/auth/password/forgot", s.forgot)
	public.POST("/auth/password/reset", s.reset)
	authed.GET("/settings/smtp", rbac.RequirePerm("setting:read"), s.getSMTP)
	authed.PUT("/settings/smtp", rbac.RequirePerm("setting:manage"), s.putSMTP)
	authed.POST("/settings/smtp/test", rbac.RequirePerm("setting:manage"), s.testSMTP)
}

func (s *Service) load(ctx context.Context) mail.Config {
	var c mail.Config
	if err := s.Settings.Get(ctx, mail.SettingKey, &c); err != nil {
		return mail.Config{}
	}
	return c
}

func usable(c mail.Config) bool { return c.Enabled && c.Host != "" && c.SiteURL != "" }

func (s *Service) available(c *gin.Context) {
	httpx.OK(c, gin.H{"available": usable(s.load(c))})
}

func (s *Service) getSMTP(c *gin.Context) {
	cfg := s.load(c)
	has := cfg.Password != ""
	cfg.Password = ""
	httpx.OK(c, gin.H{"config": cfg, "has_password": has})
}

func (s *Service) putSMTP(c *gin.Context) {
	var in mail.Config
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Password == "" {
		in.Password = s.load(c).Password
	}
	if err := in.Validate(); err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	if in.Enabled && in.SiteURL == "" {
		httpx.Fail(c, httpx.Validation("site_url is required: links in emails point to it"))
		return
	}
	raw, _ := json.Marshal(in)
	var by int64
	if p := rbac.PrincipalFrom(c); p != nil {
		by = p.UserID
	}
	if _, err := s.Settings.Put(c, mail.SettingKey, raw, true, "系统邮件（SMTP）", by); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "setting", mail.SettingKey)
	in.Password = ""
	httpx.OK(c, gin.H{"config": in})
}

// testSMTP 用提交的配置（密码留空则用已保存的）发一封测试邮件。
func (s *Service) testSMTP(c *gin.Context) {
	var in struct {
		mail.Config
		To string `json:"to" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	cfg := in.Config
	if cfg.Host == "" {
		cfg = s.load(c)
	} else if cfg.Password == "" {
		cfg.Password = s.load(c).Password
	}
	cfg.Enabled = true
	if err := cfg.Validate(); err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c, 30*time.Second)
	defer cancel()
	if err := s.Send(ctx, cfg, []string{in.To}, "星枢 Matrix 测试邮件", "这是一封测试邮件。收到说明系统邮件配置正确。\n\n发送时间："+time.Now().Format("2006-01-02 15:04:05")); err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	httpx.OK(c, gin.H{"ok": true})
}

func hashOf(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

const genericReply = "如果该账号存在并绑定了邮箱，重置链接已发送，请在 30 分钟内查收。"

func (s *Service) forgot(c *gin.Context) {
	audit.SetResource(c, "auth", "password_forgot")
	if s.Limiter != nil {
		if ok, wait := s.Limiter.Allow("forgot:" + c.ClientIP()); !ok {
			c.Header("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
			httpx.Fail(c, httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "请求过于频繁，请稍后再试"))
			return
		}
	}
	var in struct {
		Account string `json:"account" binding:"required,max=128"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	cfg := s.load(c)
	if !usable(cfg) {
		httpx.Fail(c, httpx.Conflict("系统邮件未配置，请联系平台管理员重置密码"))
		return
	}
	acct := strings.TrimSpace(in.Account)
	// 后台发送，响应时间与账号是否存在无关
	go s.issue(context.WithoutCancel(c.Request.Context()), cfg, acct, c.ClientIP())
	httpx.OK(c, gin.H{"message": genericReply})
}

func (s *Service) issue(ctx context.Context, cfg mail.Config, acct, ip string) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var id int64
	var username, email, hash, status string
	err := s.DB.QueryRow(ctx, `SELECT id, username, email, password_hash, status FROM users
		WHERE lower(username)=lower($1) OR (email<>'' AND lower(email)=lower($1)) ORDER BY id LIMIT 1`, acct).Scan(&id, &username, &email, &hash, &status)
	if err != nil || email == "" || status != "active" || strings.HasPrefix(hash, "!sso:") || strings.HasPrefix(hash, "!ldap:") {
		return
	}
	// 同一账号 2 分钟内只发一封，防止邮件轰炸
	var recent int
	if s.DB.QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE user_id=$1 AND created_at > now()-interval '2 minutes'`, id).Scan(&recent) == nil && recent > 0 {
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	if _, err := s.DB.Exec(ctx, `INSERT INTO password_resets (token_hash,user_id,expires_at,created_ip) VALUES ($1,$2,now()+$3::interval,$4)`,
		hashOf(token), id, fmt.Sprintf("%d seconds", int(TokenTTL.Seconds())), ip); err != nil {
		s.Log.Warn("password reset token insert failed", "err", err)
		return
	}
	_, _ = s.DB.Exec(ctx, `DELETE FROM password_resets WHERE expires_at < now() - interval '1 day'`)
	link := cfg.SiteURL + "/#/reset-password?token=" + token
	body := "你好，" + username + "：\n\n有人为你的账号申请了重置密码。如果是你本人操作，请在 30 分钟内打开下面的链接设置新密码：\n\n" + link +
		"\n\n如果不是你本人操作，请忽略这封邮件，你的密码不会改变。\n\n申请来源 IP：" + ip
	if err := s.Send(ctx, cfg, []string{email}, "重置你的星枢 Matrix 密码", body); err != nil {
		s.Log.Warn("password reset mail failed", "user", username, "err", err)
	}
}

func (s *Service) reset(c *gin.Context) {
	audit.SetResource(c, "auth", "password_reset")
	if s.Limiter != nil {
		if ok, _ := s.Limiter.Allow("reset:" + c.ClientIP()); !ok {
			httpx.Fail(c, httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "请求过于频繁，请稍后再试"))
			return
		}
	}
	var in struct {
		Token    string `json:"token" binding:"required"`
		Password string `json:"password" binding:"required,max=128"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	var uid int64
	var username string
	err := s.DB.QueryRow(c, `SELECT r.user_id, u.username FROM password_resets r JOIN users u ON u.id=r.user_id
		WHERE r.token_hash=$1 AND r.used_at IS NULL AND r.expires_at > now()`, hashOf(in.Token)).Scan(&uid, &username)
	if err != nil {
		httpx.Fail(c, httpx.Validation("重置链接无效或已过期，请重新申请"))
		return
	}
	if err := auth.ValidatePassword(in.Password, username); err != nil {
		httpx.Fail(c, err)
		return
	}
	tag, err := s.DB.Exec(c, `UPDATE password_resets SET used_at=now() WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now()`, hashOf(in.Token))
	if err != nil || tag.RowsAffected() != 1 {
		httpx.Fail(c, httpx.Validation("重置链接无效或已过期，请重新申请"))
		return
	}
	// 同一用户其它未使用的令牌一并作废
	_, _ = s.DB.Exec(c, `UPDATE password_resets SET used_at=now() WHERE user_id=$1 AND used_at IS NULL`, uid)
	if err := s.Auth.ResetPassword(c.Request.Context(), uid, in.Password); err != nil {
		if errors.Is(err, auth.ErrWeakPassword) {
			httpx.Fail(c, err)
			return
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetUser(c, uid, username)
	httpx.OK(c, gin.H{"message": "密码已重置，请使用新密码登录"})
}
