// Package sso 实现企业单点登录（OpenID Connect 授权码模式 + PKCE）。
//
// 流程：登录页 → GET /auth/sso/start → 身份源登录 → GET /auth/sso/callback
// → 平台用授权码换取 access token、读取 userinfo、映射 / 自动创建本地用户
// → 带一次性 sso_ticket 跳回 SPA → POST /auth/sso/token 换取平台会话。
// 浏览器地址栏里只出现一次性、60 秒有效的票据，从不出现平台 token。
//
// 配置存放在设置项 auth.oidc（整体加密），通过 GET/PUT /settings/sso 管理。
package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// SettingKey 是保存 OIDC 配置的设置项。
const SettingKey = "auth.oidc"

// Config 是 OIDC 身份源配置。
type Config struct {
	Enabled       bool   `json:"enabled"`
	DisplayName   string `json:"display_name"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret,omitempty"`
	Scopes        string `json:"scopes"`
	UsernameClaim string `json:"username_claim"`
	NameClaim     string `json:"name_claim"`
	EmailClaim    string `json:"email_claim"`
	AutoCreate    bool   `json:"auto_create"`
	DefaultRole   string `json:"default_role"`
	// RedirectBase 是用户浏览器访问平台的外部地址，例如 https://matrix.example.com；
	// 回调地址为 RedirectBase + /api/v1/auth/sso/callback，需要在身份源登记。
	RedirectBase string `json:"redirect_base"`
}

func (c *Config) normalize() {
	c.Issuer = strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
	c.RedirectBase = strings.TrimRight(strings.TrimSpace(c.RedirectBase), "/")
	if c.Scopes == "" {
		c.Scopes = "openid profile email"
	}
	if c.UsernameClaim == "" {
		c.UsernameClaim = "preferred_username"
	}
	if c.NameClaim == "" {
		c.NameClaim = "name"
	}
	if c.EmailClaim == "" {
		c.EmailClaim = "email"
	}
	if c.DefaultRole == "" {
		c.DefaultRole = "viewer"
	}
	if c.DisplayName == "" {
		c.DisplayName = "企业 SSO"
	}
}

// Validate 检查启用所需的字段。
func (c *Config) Validate() error {
	c.normalize()
	if !c.Enabled {
		return nil
	}
	for k, v := range map[string]string{"issuer": c.Issuer, "client_id": c.ClientID, "redirect_base": c.RedirectBase} {
		if v == "" {
			return httpx.Validation(k + " is required when SSO is enabled")
		}
	}
	for k, v := range map[string]string{"issuer": c.Issuer, "redirect_base": c.RedirectBase} {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return httpx.Validation(k + " must be an absolute http(s) URL")
		}
	}
	if !strings.Contains(" "+c.Scopes+" ", " openid ") {
		return httpx.Validation("scopes must include openid")
	}
	return nil
}

// CallbackURL 是需要在身份源登记的回调地址。
func (c *Config) CallbackURL() string { return c.RedirectBase + "/api/v1/auth/sso/callback" }

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Service 提供 SSO 路由。
type Service struct {
	DB       store.DB
	Settings *settings.Service
	Auth     *auth.Service
	Log      *slog.Logger
	HTTP     *http.Client

	mu    sync.Mutex
	cache map[string]cachedDisc
}

type cachedDisc struct {
	d   *discovery
	exp time.Time
}

// Register 挂载公开与需登录的 SSO 路由。
func (s *Service) Register(public, authed *gin.RouterGroup) {
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	public.GET("/auth/sso/config", s.publicConfig)
	public.GET("/auth/sso/start", s.start)
	public.GET("/auth/sso/callback", s.callback)
	public.POST("/auth/sso/token", s.token)
	authed.GET("/settings/sso", rbac.RequirePerm("setting:read"), s.getConfig)
	authed.PUT("/settings/sso", rbac.RequirePerm("setting:manage"), s.putConfig)
	authed.POST("/settings/sso/test", rbac.RequirePerm("setting:manage"), s.testConfig)
}

func (s *Service) load(ctx context.Context) (*Config, error) {
	var c Config
	if err := s.Settings.Get(ctx, SettingKey, &c); err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			c.normalize()
			return &c, nil
		}
		return nil, err
	}
	c.normalize()
	return &c, nil
}

func (s *Service) publicConfig(c *gin.Context) {
	cfg, err := s.load(c)
	if err != nil {
		httpx.OK(c, gin.H{"enabled": false})
		return
	}
	httpx.OK(c, gin.H{"enabled": cfg.Enabled && cfg.Issuer != "" && cfg.ClientID != "", "display_name": cfg.DisplayName})
}

func (s *Service) getConfig(c *gin.Context) {
	cfg, err := s.load(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	hasSecret := cfg.ClientSecret != ""
	cfg.ClientSecret = ""
	httpx.OK(c, gin.H{"config": cfg, "has_client_secret": hasSecret, "callback_url": cfg.CallbackURL()})
}

func (s *Service) putConfig(c *gin.Context) {
	var in Config
	if !httpx.Bind(c, &in) {
		return
	}
	old, err := s.load(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.ClientSecret == "" { // 留空表示保留原密钥
		in.ClientSecret = old.ClientSecret
	}
	if err := in.Validate(); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Enabled && in.AutoCreate {
		var n int
		if err := s.DB.QueryRow(c, `SELECT count(*) FROM roles WHERE code=$1`, in.DefaultRole).Scan(&n); err != nil || n == 0 {
			httpx.Fail(c, httpx.Validation("default_role does not exist: "+in.DefaultRole))
			return
		}
		if in.DefaultRole == "admin" {
			httpx.Fail(c, httpx.Validation("auto-created SSO users cannot default to the admin role"))
			return
		}
	}
	raw, _ := json.Marshal(in)
	var by int64
	if p := rbac.PrincipalFrom(c); p != nil {
		by = p.UserID
	}
	if _, err := s.Settings.Put(c, SettingKey, raw, true, "企业 SSO（OIDC）", by); err != nil {
		httpx.Fail(c, err)
		return
	}
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
	audit.SetResource(c, "setting", SettingKey)
	in.ClientSecret = ""
	httpx.OK(c, gin.H{"config": in, "callback_url": in.CallbackURL()})
}

// testConfig 用提交的（或已保存的）配置做一次发现文档探测，验证 issuer 可达且端点完整。
func (s *Service) testConfig(c *gin.Context) {
	var in Config
	_ = c.ShouldBindJSON(&in)
	if in.Issuer == "" {
		cfg, err := s.load(c)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		in = *cfg
	}
	in.normalize()
	d, err := s.fetchDiscovery(c, in.Issuer)
	if err != nil {
		httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		return
	}
	httpx.OK(c, gin.H{"ok": true, "issuer": d.Issuer, "authorization_endpoint": d.AuthorizationEndpoint,
		"token_endpoint": d.TokenEndpoint, "userinfo_endpoint": d.UserinfoEndpoint, "callback_url": in.CallbackURL()})
}

func (s *Service) discover(ctx context.Context, issuer string) (*discovery, error) {
	s.mu.Lock()
	if v, ok := s.cache[issuer]; ok && time.Now().Before(v.exp) {
		s.mu.Unlock()
		return v.d, nil
	}
	s.mu.Unlock()
	d, err := s.fetchDiscovery(ctx, issuer)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cachedDisc{}
	}
	s.cache[issuer] = cachedDisc{d: d, exp: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return d, nil
}

func (s *Service) fetchDiscovery(ctx context.Context, issuer string) (*discovery, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法访问身份源：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("发现文档返回 HTTP %d", resp.StatusCode)
	}
	var d discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, errors.New("发现文档不是合法 JSON")
	}
	if strings.TrimRight(d.Issuer, "/") != issuer {
		return nil, fmt.Errorf("发现文档的 issuer（%s）与配置不一致", d.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.UserinfoEndpoint == "" {
		return nil, errors.New("发现文档缺少 authorization / token / userinfo 端点")
	}
	return &d, nil
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashOf(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

// safeRedirect 只允许跳回平台自己的外部地址，防止开放重定向。
func safeRedirect(cfg *Config, r string) string {
	base := cfg.RedirectBase + "/"
	if r == "" || !(strings.HasPrefix(r, base) || r == cfg.RedirectBase) {
		return base
	}
	u, err := url.Parse(r)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Del("sso_ticket")
	q.Del("sso_error")
	u.RawQuery = q.Encode()
	return u.String()
}

func withParam(r, k, v string) string {
	u, err := url.Parse(r)
	if err != nil {
		return r
	}
	q := u.Query()
	q.Set(k, v)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Service) start(c *gin.Context) {
	cfg, err := s.load(c)
	if err != nil || !cfg.Enabled {
		c.String(http.StatusNotFound, "企业 SSO 未启用")
		return
	}
	d, err := s.discover(c, cfg.Issuer)
	if err != nil {
		s.Log.Warn("sso discovery failed", "err", err)
		c.Redirect(http.StatusFound, withParam(safeRedirect(cfg, c.Query("redirect")), "sso_error", "server_error"))
		return
	}
	state, verifier, nonce := random(24), random(48), random(16)
	redirect := safeRedirect(cfg, c.Query("redirect"))
	if _, err := s.DB.Exec(c, `INSERT INTO sso_states (state_hash,verifier,nonce,redirect,expires_at) VALUES ($1,$2,$3,$4,now()+interval '10 minutes')`,
		hashOf(state), verifier, nonce, redirect); err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	_, _ = s.DB.Exec(c, `DELETE FROM sso_states WHERE expires_at < now()`)
	ch := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {cfg.CallbackURL()},
		"scope":                 {cfg.Scopes},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(ch[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	c.Redirect(http.StatusFound, d.AuthorizationEndpoint+sep+q.Encode())
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

func (s *Service) callback(c *gin.Context) {
	cfg, err := s.load(c)
	if err != nil || !cfg.Enabled {
		c.String(http.StatusNotFound, "企业 SSO 未启用")
		return
	}
	fail := func(redirect, code string) {
		c.Redirect(http.StatusFound, withParam(redirect, "sso_error", code))
	}
	var verifier, nonce, redirect string
	err = s.DB.QueryRow(c, `DELETE FROM sso_states WHERE state_hash=$1 AND expires_at > now() RETURNING verifier, nonce, redirect`,
		hashOf(c.Query("state"))).Scan(&verifier, &nonce, &redirect)
	if err != nil {
		fail(cfg.RedirectBase+"/", "invalid_state")
		return
	}
	if e := c.Query("error"); e != "" {
		fail(redirect, "provider_"+regexp.MustCompile(`[^a-z_]`).ReplaceAllString(e, ""))
		return
	}
	code := c.Query("code")
	if code == "" {
		fail(redirect, "invalid_state")
		return
	}
	d, err := s.discover(c, cfg.Issuer)
	if err != nil {
		fail(redirect, "server_error")
		return
	}
	claims, err := s.exchange(c, cfg, d, code, verifier)
	if err != nil {
		s.Log.Warn("sso exchange failed", "err", err)
		fail(redirect, "server_error")
		return
	}
	username, _ := claims[cfg.UsernameClaim].(string)
	username = strings.TrimSpace(username)
	if !usernameRe.MatchString(username) {
		s.Log.Warn("sso username claim missing or invalid", "claim", cfg.UsernameClaim)
		fail(redirect, "invalid_identity")
		return
	}
	name, _ := claims[cfg.NameClaim].(string)
	email, _ := claims[cfg.EmailClaim].(string)
	uid, err := s.mapUser(c, cfg, username, name, email)
	if err != nil {
		s.Log.Warn("sso user mapping failed", "user", username, "err", err)
		fail(redirect, "invalid_identity")
		return
	}
	ticket := random(32)
	if _, err := s.DB.Exec(c, `INSERT INTO sso_tickets (ticket_hash,user_id,ip,user_agent,expires_at) VALUES ($1,$2,$3,$4,now()+interval '60 seconds')`,
		hashOf(ticket), uid, c.ClientIP(), c.Request.UserAgent()); err != nil {
		fail(redirect, "server_error")
		return
	}
	_, _ = s.DB.Exec(c, `DELETE FROM sso_tickets WHERE expires_at < now()`)
	c.Redirect(http.StatusFound, withParam(redirect, "sso_ticket", ticket))
}

// exchange 用授权码换 access token（client_secret_post + PKCE），再读 userinfo。
func (s *Service) exchange(ctx context.Context, cfg *Config, d *discovery, code, verifier string) (map[string]any, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {cfg.CallbackURL()},
		"client_id":     {cfg.ClientID},
		"code_verifier": {verifier},
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint HTTP %d %s", resp.StatusCode, tok.Error)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, d.UserinfoEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp2, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo HTTP %d", resp2.StatusCode)
	}
	claims := map[string]any{}
	if err := json.NewDecoder(io.LimitReader(resp2.Body, 1<<20)).Decode(&claims); err != nil {
		return nil, fmt.Errorf("userinfo response: %w", err)
	}
	return claims, nil
}

// mapUser 按用户名找到本地用户；不存在且允许自动创建时，以默认角色创建（密码不可用于本地登录）。
func (s *Service) mapUser(ctx context.Context, cfg *Config, username, name, email string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(ctx, `SELECT id FROM users WHERE lower(username)=lower($1)`, username).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if !cfg.AutoCreate {
		return 0, errors.New("user does not exist and auto_create is off")
	}
	if name == "" {
		name = username
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// 不可登录的占位哈希：本地密码登录永远校验失败，只能走 SSO
	if err := tx.QueryRow(ctx, `INSERT INTO users(username, display_name, email, password_hash, must_change_password)
		VALUES ($1,$2,$3,$4,FALSE) RETURNING id`, username, name, email, "!sso:"+random(16)).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_roles(user_id, role_id) SELECT $1, id FROM roles WHERE code=$2`, id, cfg.DefaultRole); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Service) token(c *gin.Context) {
	var in struct {
		Ticket string `json:"ticket" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	audit.SetResource(c, "auth", "sso_login")
	var uid int64
	if err := s.DB.QueryRow(c, `DELETE FROM sso_tickets WHERE ticket_hash=$1 AND expires_at > now() RETURNING user_id`, hashOf(in.Ticket)).Scan(&uid); err != nil {
		httpx.Fail(c, httpx.Unauthorized("登录票据无效或已过期，请重新登录"))
		return
	}
	pair, u, err := s.Auth.LoginExternal(c.Request.Context(), uid, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetUser(c, u.ID, u.Username)
	httpx.OK(c, gin.H{"token": pair, "user": gin.H{"id": u.ID, "username": u.Username, "display_name": u.DisplayName, "must_change_password": false}})
}
