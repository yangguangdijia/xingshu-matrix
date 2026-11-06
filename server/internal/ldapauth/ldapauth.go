// Package ldapauth 让平台登录页直接使用 LDAP / Active Directory 账号。
//
// 登录流程（auth.Service.Login 调用 Authenticate）：
//  1. 本地存在且是普通本地账号 → 只校验本地密码，不访问 LDAP；
//  2. 本地不存在，或是此前由 LDAP 创建的账号（密码哈希以 "!ldap:" 开头）→
//     以服务账号绑定 → 按 user_filter 搜索唯一条目 → 以该条目 DN 和用户输入的密码再次绑定；
//  3. 成功后按需自动建号（默认角色不可为 admin），每次登录同步显示名、邮箱，配置了组映射时同步角色。
//
// 配置存放在设置项 auth.ldap（整体加密）。
package ldapauth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// SettingKey 保存 LDAP 配置。
const SettingKey = "auth.ldap"

// HashPrefix 标记由 LDAP 创建的本地账号（本地密码不可用）。
const HashPrefix = "!ldap:"

// Config 是 LDAP 配置。
type Config struct {
	Enabled            bool              `json:"enabled"`
	URL                string            `json:"url"` // ldap://host:389 或 ldaps://host:636
	StartTLS           bool              `json:"start_tls"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify"`
	BindDN             string            `json:"bind_dn"`
	BindPassword       string            `json:"bind_password,omitempty"`
	BaseDN             string            `json:"base_dn"`
	UserFilter         string            `json:"user_filter"` // 例如 (uid={username}) 或 (sAMAccountName={username})
	AttrUsername       string            `json:"attr_username"`
	AttrDisplayName    string            `json:"attr_display_name"`
	AttrEmail          string            `json:"attr_email"`
	AttrGroups         string            `json:"attr_groups"` // 例如 memberOf
	GroupRoles         map[string]string `json:"group_roles"` // 组 DN（或 CN）→ 平台角色编码
	AutoCreate         bool              `json:"auto_create"`
	DefaultRole        string            `json:"default_role"`
}

func (c *Config) normalize() {
	c.URL = strings.TrimSpace(c.URL)
	c.BaseDN = strings.TrimSpace(c.BaseDN)
	c.BindDN = strings.TrimSpace(c.BindDN)
	if c.UserFilter == "" {
		c.UserFilter = "(uid={username})"
	}
	if c.AttrUsername == "" {
		c.AttrUsername = "uid"
	}
	if c.AttrDisplayName == "" {
		c.AttrDisplayName = "cn"
	}
	if c.AttrEmail == "" {
		c.AttrEmail = "mail"
	}
	if c.DefaultRole == "" {
		c.DefaultRole = "viewer"
	}
	if c.GroupRoles == nil {
		c.GroupRoles = map[string]string{}
	}
}

// Validate 检查配置。
func (c *Config) Validate() error {
	c.normalize()
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
		return errors.New("url must be ldap://host:port or ldaps://host:port")
	}
	if c.BaseDN == "" {
		return errors.New("base_dn is required")
	}
	if !strings.Contains(c.UserFilter, "{username}") {
		return errors.New("user_filter must contain {username}")
	}
	if _, err := ldap.CompileFilter(strings.ReplaceAll(c.UserFilter, "{username}", "x")); err != nil {
		return fmt.Errorf("user_filter is not a valid LDAP filter: %v", err)
	}
	if c.DefaultRole == "admin" {
		return errors.New("default_role must not be admin")
	}
	for g, r := range c.GroupRoles {
		if strings.TrimSpace(g) == "" || strings.TrimSpace(r) == "" {
			return errors.New("group_roles entries must be non-empty")
		}
	}
	return nil
}

// Filter 生成搜索过滤器（用户名经 LDAP 转义，防止过滤器注入）。
func (c *Config) Filter(username string) string {
	return strings.ReplaceAll(c.UserFilter, "{username}", ldap.EscapeFilter(username))
}

// Identity 是 LDAP 返回的用户信息。
type Identity struct {
	DN          string   `json:"dn"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Email       string   `json:"email"`
	Groups      []string `json:"groups"`
	Roles       []string `json:"roles"`
}

// RolesFor 按组映射求角色；组既可写完整 DN，也可只写组名（DN 第一个 RDN 的值，大小写不敏感）。
func (c *Config) RolesFor(groups []string) []string {
	set := map[string]bool{}
	for _, g := range groups {
		lg := strings.ToLower(g)
		cn := lg // 组名：取 DN 第一个 RDN 的值（cn=ops,… 或 ou=ops,…）
		if first := strings.SplitN(lg, ",", 2)[0]; strings.Contains(first, "=") {
			cn = strings.TrimSpace(first[strings.Index(first, "=")+1:])
		}
		for k, r := range c.GroupRoles {
			lk := strings.ToLower(strings.TrimSpace(k))
			if lk == lg || lk == cn {
				set[strings.TrimSpace(r)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Service 实现 auth.ExternalAuthenticator 并提供设置路由。
type Service struct {
	DB       store.DB
	Settings *settings.Service
	// Dial 可在测试中替换。
	Dial func(cfg *Config) (*ldap.Conn, error)
}

func (s *Service) load(ctx context.Context) Config {
	var c Config
	_ = s.Settings.Get(ctx, SettingKey, &c)
	c.normalize()
	return c
}

func dial(cfg *Config) (*ldap.Conn, error) {
	tc := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // 管理员显式开启
	if u, err := url.Parse(cfg.URL); err == nil {
		tc.ServerName, _, _ = net.SplitHostPort(u.Host)
		if tc.ServerName == "" {
			tc.ServerName = u.Host
		}
	}
	conn, err := ldap.DialURL(cfg.URL, ldap.DialWithTLSConfig(tc), ldap.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}))
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(15 * time.Second)
	if cfg.StartTLS && strings.HasPrefix(cfg.URL, "ldap://") {
		if err := conn.StartTLS(tc); err != nil {
			conn.Close()
			return nil, fmt.Errorf("StartTLS: %w", err)
		}
	}
	return conn, nil
}

// lookup 用服务账号搜索用户；password 非空时再以用户 DN 绑定验证密码。
func (s *Service) lookup(cfg *Config, username, password string, verify bool) (*Identity, error) {
	d := s.Dial
	if d == nil {
		d = dial
	}
	conn, err := d(cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 LDAP 失败: %w", err)
	}
	defer conn.Close()
	if cfg.BindDN != "" {
		if err := conn.Bind(cfg.BindDN, cfg.BindPassword); err != nil {
			return nil, fmt.Errorf("服务账号绑定失败: %w", err)
		}
	}
	attrs := []string{"dn", cfg.AttrUsername, cfg.AttrDisplayName, cfg.AttrEmail}
	if cfg.AttrGroups != "" {
		attrs = append(attrs, cfg.AttrGroups)
	}
	res, err := conn.Search(ldap.NewSearchRequest(cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 10, false,
		cfg.Filter(username), attrs, nil))
	if err != nil {
		return nil, fmt.Errorf("搜索用户失败: %w", err)
	}
	if len(res.Entries) != 1 {
		return nil, errNotFound
	}
	e := res.Entries[0]
	id := &Identity{DN: e.DN, Username: e.GetAttributeValue(cfg.AttrUsername), DisplayName: e.GetAttributeValue(cfg.AttrDisplayName), Email: e.GetAttributeValue(cfg.AttrEmail)}
	if id.Username == "" {
		id.Username = username
	}
	if cfg.AttrGroups != "" {
		id.Groups = e.GetAttributeValues(cfg.AttrGroups)
	}
	id.Roles = cfg.RolesFor(id.Groups)
	if verify {
		if password == "" { // 空密码会成为匿名绑定，必须拒绝
			return nil, errBadPassword
		}
		if err := conn.Bind(e.DN, password); err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
				return nil, errBadPassword
			}
			return nil, fmt.Errorf("用户绑定失败: %w", err)
		}
	}
	return id, nil
}

var (
	errNotFound    = errors.New("ldap user not found")
	errBadPassword = errors.New("ldap invalid credentials")
)

// Authenticate 由 auth.Service.Login 调用。handled=false 表示 LDAP 未启用，交还本地流程。
// 返回 userID>0 表示认证成功。
func (s *Service) Authenticate(ctx context.Context, username, password string) (userID int64, handled bool, err error) {
	cfg := s.load(ctx)
	if !cfg.Enabled {
		return 0, false, nil
	}
	id, err := s.lookup(&cfg, username, password, true)
	if errors.Is(err, errNotFound) || errors.Is(err, errBadPassword) {
		return 0, true, nil
	}
	if err != nil {
		return 0, true, err
	}
	uid, err := s.sync(ctx, &cfg, id)
	return uid, true, err
}

func randHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// sync 建号或更新显示名、邮箱；配置了组映射时以组映射结果覆盖角色（无命中时保留默认角色）。
func (s *Service) sync(ctx context.Context, cfg *Config, id *Identity) (int64, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var uid int64
	var hash string
	err = tx.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE lower(username)=lower($1)`, id.Username).Scan(&uid, &hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if !cfg.AutoCreate {
			return 0, errors.New("该 LDAP 用户在平台中不存在，且未开启自动创建")
		}
		name := id.DisplayName
		if name == "" {
			name = id.Username
		}
		if err := tx.QueryRow(ctx, `INSERT INTO users(username, display_name, email, password_hash, must_change_password)
			VALUES ($1,$2,$3,$4,FALSE) RETURNING id`, id.Username, name, id.Email, HashPrefix+randHex(16)).Scan(&uid); err != nil {
			return 0, err
		}
		roles := id.Roles
		if len(roles) == 0 {
			roles = []string{cfg.DefaultRole}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_roles(user_id, role_id) SELECT $1, id FROM roles WHERE code = ANY($2) ON CONFLICT DO NOTHING`, uid, roles); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if !strings.HasPrefix(hash, HashPrefix) {
			// 同名本地账号：不接管，避免 LDAP 冒用本地管理员
			return 0, errors.New("同名本地账号已存在，请使用本地密码登录")
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET display_name=COALESCE(NULLIF($2,''),display_name), email=COALESCE(NULLIF($3,''),email), updated_at=now() WHERE id=$1`,
			uid, id.DisplayName, id.Email); err != nil {
			return 0, err
		}
		if len(cfg.GroupRoles) > 0 && len(id.Roles) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id=$1`, uid); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO user_roles(user_id, role_id) SELECT $1, id FROM roles WHERE code = ANY($2) ON CONFLICT DO NOTHING`, uid, id.Roles); err != nil {
				return 0, err
			}
		}
	}
	return uid, tx.Commit(ctx)
}

// Register 挂载设置路由。
func (s *Service) Register(authed *gin.RouterGroup) {
	authed.GET("/settings/ldap", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		cfg := s.load(c)
		has := cfg.BindPassword != ""
		cfg.BindPassword = ""
		httpx.OK(c, gin.H{"config": cfg, "has_bind_password": has})
	})
	authed.PUT("/settings/ldap", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var in Config
		if !httpx.Bind(c, &in) {
			return
		}
		if in.BindPassword == "" {
			in.BindPassword = s.load(c).BindPassword
		}
		if err := in.Validate(); err != nil {
			httpx.Fail(c, httpx.Validation(err.Error()))
			return
		}
		raw, _ := json.Marshal(in)
		var by int64
		if p := rbac.PrincipalFrom(c); p != nil {
			by = p.UserID
		}
		if _, err := s.Settings.Put(c, SettingKey, raw, true, "LDAP / AD 登录", by); err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "setting", SettingKey)
		in.BindPassword = ""
		httpx.OK(c, gin.H{"config": in})
	})
	// 测试：连接并以服务账号绑定；填了 username 时搜索该用户，再填 password 时验证其密码（不建号、不登录）。
	authed.POST("/settings/ldap/test", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var in struct {
			Config
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		cfg := in.Config
		if cfg.URL == "" {
			cfg = s.load(c)
		} else if cfg.BindPassword == "" {
			cfg.BindPassword = s.load(c).BindPassword
		}
		cfg.Enabled = true
		if err := cfg.Validate(); err != nil {
			httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if in.Username == "" {
			d := s.Dial
			if d == nil {
				d = dial
			}
			conn, err := d(&cfg)
			if err == nil && cfg.BindDN != "" {
				err = conn.Bind(cfg.BindDN, cfg.BindPassword)
			}
			if conn != nil {
				conn.Close()
			}
			if err != nil {
				httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
				return
			}
			httpx.OK(c, gin.H{"ok": true, "message": "连接与服务账号绑定成功"})
			return
		}
		id, err := s.lookup(&cfg, in.Username, in.Password, in.Password != "")
		switch {
		case errors.Is(err, errNotFound):
			httpx.OK(c, gin.H{"ok": false, "error": "未找到该用户（或匹配到多个条目），请检查 Base DN 与用户过滤器"})
		case errors.Is(err, errBadPassword):
			httpx.OK(c, gin.H{"ok": false, "error": "用户已找到，但密码错误"})
		case err != nil:
			httpx.OK(c, gin.H{"ok": false, "error": err.Error()})
		default:
			msg := "已找到用户"
			if in.Password != "" {
				msg = "用户名与密码验证通过"
			}
			httpx.OK(c, gin.H{"ok": true, "message": msg, "identity": id})
		}
	})
}
