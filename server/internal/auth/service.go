package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// AccessResolver resolves roles/permissions/env scope for a user (rbac.Service).
type AccessResolver interface {
	UserAccess(ctx context.Context, userID int64) (roles, perms, envScope []string, err error)
}

// Options configures the Service.
type Options struct {
	RefreshTTL       time.Duration
	MaxLoginFailures int
	LockoutDuration  time.Duration
	// PrincipalCacheTTL bounds how long role/permission changes may take to
	// apply on other replicas (local changes invalidate immediately).
	PrincipalCacheTTL time.Duration
}

// TokenPair is returned by login and refresh.
type TokenPair struct {
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	TokenType          string    `json:"token_type"`
	ExpiresIn          int64     `json:"expires_in"`
	ExpiresAt          time.Time `json:"expires_at"`
	MustChangePassword bool      `json:"must_change_password"`
}

// Service is the authentication service.
type Service struct {
	repo   Repo
	access AccessResolver
	tokens *TokenManager
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	mu    sync.RWMutex
	cache map[string]cacheEntry // by session id
}

type cacheEntry struct {
	p   *rbac.Principal
	exp time.Time
}

// NewService builds the auth service.
func NewService(repo Repo, access AccessResolver, tokens *TokenManager, opts Options, log *slog.Logger) *Service {
	if opts.PrincipalCacheTTL <= 0 {
		opts.PrincipalCacheTTL = 15 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, access: access, tokens: tokens, opts: opts, log: log, now: time.Now, cache: map[string]cacheEntry{}}
}

// Errors returned to clients.
var (
	errBadCredentials = httpx.NewError(http.StatusUnauthorized, httpx.CodeInvalidCredentials, "用户名或密码错误")
	errDisabled       = httpx.NewError(http.StatusForbidden, httpx.CodeAccountDisabled, "账号已被禁用")
	errSessionInvalid = httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "会话已失效，请重新登录")
	errTokenExpired   = httpx.NewError(http.StatusUnauthorized, httpx.CodeTokenExpired, "access token expired")
)

func lockedError(until time.Time, now time.Time) *httpx.Error {
	mins := int(until.Sub(now).Minutes()) + 1
	return httpx.NewError(http.StatusForbidden, httpx.CodeAccountLocked,
		fmt.Sprintf("登录失败次数过多，账号已锁定，请 %d 分钟后再试", mins)).
		WithDetails(map[string]any{"locked_until": until})
}

// Login verifies credentials and opens a session.
func (s *Service) Login(ctx context.Context, username, password, ip, userAgent string) (*TokenPair, *User, error) {
	now := s.now()
	u, err := s.repo.UserByUsername(ctx, username)
	if errors.Is(err, httpx.ErrNotFound) {
		VerifyPassword(string(dummyHash), password) // equalise timing
		return nil, nil, errBadCredentials
	}
	if err != nil {
		return nil, nil, err
	}
	if u.LockedUntil != nil && u.LockedUntil.After(now) {
		return nil, nil, lockedError(*u.LockedUntil, now)
	}
	if !VerifyPassword(u.PasswordHash, password) {
		until := now.Add(s.opts.LockoutDuration)
		failures, locked, err := s.repo.RecordLoginFailure(ctx, u.ID, s.opts.MaxLoginFailures, until)
		if err != nil {
			return nil, nil, err
		}
		if locked {
			s.log.Warn("account locked after repeated login failures", "user", u.Username, "ip", ip)
			return nil, nil, lockedError(until, now)
		}
		return nil, nil, errBadCredentials.WithDetails(map[string]any{"remaining_attempts": s.opts.MaxLoginFailures - failures})
	}
	if u.Status != "active" {
		return nil, nil, errDisabled
	}
	if err := s.repo.RecordLoginSuccess(ctx, u.ID, ip); err != nil {
		return nil, nil, err
	}
	sess := &Session{ID: NewSessionID(), UserID: u.ID, UserAgent: truncate(userAgent, 512), IP: ip, ExpiresAt: now.Add(s.opts.RefreshTTL)}
	refresh, hash := NewRefreshToken(sess.ID)
	sess.RefreshHash = hash
	if err := s.repo.CreateSession(ctx, sess); err != nil {
		return nil, nil, err
	}
	pair, err := s.pair(u, sess.ID, refresh)
	return pair, u, err
}

func (s *Service) pair(u *User, sid, refresh string) (*TokenPair, error) {
	access, exp, err := s.tokens.IssueAccess(u.ID, u.Username, sid)
	if err != nil {
		return nil, err
	}
	return &TokenPair{
		AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: int64(time.Until(exp).Seconds()), ExpiresAt: exp, MustChangePassword: u.MustChangePassword,
	}, nil
}

// Refresh rotates a refresh token. Presenting an already-rotated token revokes
// the whole session (refresh token reuse detection).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	sid, secret, ok := SplitRefreshToken(refreshToken)
	if !ok {
		return nil, errSessionInvalid
	}
	sess, err := s.repo.SessionByID(ctx, sid)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(s.now()) {
		return nil, errSessionInvalid
	}
	oldHash := HashSecret(secret)
	if !EqualHash(oldHash, sess.RefreshHash) {
		s.log.Warn("refresh token reuse detected; revoking session", "session", sid, "user_id", sess.UserID)
		_ = s.repo.RevokeSession(ctx, sid)
		s.invalidateSession(sid)
		return nil, errSessionInvalid
	}
	u, err := s.repo.UserByID(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, errSessionInvalid
		}
		return nil, err
	}
	if u.Status != "active" {
		_ = s.repo.RevokeSession(ctx, sid)
		return nil, errDisabled
	}
	newTok, newHash := NewRefreshToken(sid)
	rotated, err := s.repo.RotateSession(ctx, sid, oldHash, newHash)
	if err != nil {
		return nil, err
	}
	if !rotated { // lost a race with a concurrent refresh
		return nil, errSessionInvalid
	}
	return s.pair(u, sid, newTok)
}

// Logout revokes the session.
func (s *Service) Logout(ctx context.Context, sid string) error {
	s.invalidateSession(sid)
	return s.repo.RevokeSession(ctx, sid)
}

// ChangePassword verifies the old password, enforces policy, clears the
// forced-change flag and revokes every other session of the user.
func (s *Service) ChangePassword(ctx context.Context, p *rbac.Principal, oldPw, newPw string) error {
	u, err := s.repo.UserByID(ctx, p.UserID)
	if err != nil {
		return err
	}
	if !VerifyPassword(u.PasswordHash, oldPw) {
		return httpx.NewError(http.StatusBadRequest, httpx.CodeInvalidCredentials, "原密码错误")
	}
	if oldPw == newPw {
		return httpx.Validation("新密码不能与原密码相同")
	}
	if err := ValidatePassword(newPw, u.Username); err != nil {
		return err
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		return err
	}
	if err := s.repo.UpdatePassword(ctx, u.ID, hash, false); err != nil {
		return err
	}
	if err := s.repo.RevokeUserSessions(ctx, u.ID, p.SessionID); err != nil {
		return err
	}
	s.InvalidateUser(u.ID)
	return nil
}

// Sessions lists the active sessions of the principal.
func (s *Service) Sessions(ctx context.Context, p *rbac.Principal) ([]Session, error) {
	ss, err := s.repo.ListUserSessions(ctx, p.UserID)
	for i := range ss {
		ss[i].Current = ss[i].ID == p.SessionID
	}
	if ss == nil {
		ss = []Session{}
	}
	return ss, err
}

// RevokeOwnSession revokes one of the principal's own sessions.
func (s *Service) RevokeOwnSession(ctx context.Context, p *rbac.Principal, sid string) error {
	sess, err := s.repo.SessionByID(ctx, sid)
	if err != nil {
		return err
	}
	if sess.UserID != p.UserID {
		return httpx.ErrNotFound
	}
	return s.Logout(ctx, sid)
}

// RevokeAllSessions revokes every session of a user (used when an admin
// disables a user or resets their password).
func (s *Service) RevokeAllSessions(ctx context.Context, userID int64) error {
	s.InvalidateUser(userID)
	return s.repo.RevokeUserSessions(ctx, userID, "")
}

// Authenticate verifies an access token and returns the principal. The session
// must still be live and the user active.
func (s *Service) Authenticate(ctx context.Context, token string) (*rbac.Principal, error) {
	claims, err := s.tokens.ParseAccess(token)
	if errors.Is(err, ErrTokenExpired) {
		return nil, errTokenExpired
	}
	if err != nil {
		return nil, httpx.Unauthorized("invalid access token")
	}
	now := s.now()
	s.mu.RLock()
	e, ok := s.cache[claims.SessionID]
	s.mu.RUnlock()
	if ok && e.exp.After(now) && e.p.UserID == claims.UserID() {
		return e.p, nil
	}
	sess, err := s.repo.SessionByID(ctx, claims.SessionID)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(now) || sess.UserID != claims.UserID() {
		return nil, errSessionInvalid
	}
	p, err := s.loadPrincipal(ctx, claims.UserID(), claims.SessionID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[claims.SessionID] = cacheEntry{p: p, exp: now.Add(s.opts.PrincipalCacheTTL)}
	if len(s.cache) > 50_000 { // crude bound
		for k, v := range s.cache {
			if v.exp.Before(now) {
				delete(s.cache, k)
			}
		}
	}
	s.mu.Unlock()
	return p, nil
}

func (s *Service) loadPrincipal(ctx context.Context, userID int64, sid string) (*rbac.Principal, error) {
	u, err := s.repo.UserByID(ctx, userID)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, errDisabled
	}
	roles, perms, scope, err := s.access.UserAccess(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &rbac.Principal{
		UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName, SessionID: sid,
		Roles: roles, Permissions: perms, EnvScope: scope, MustChangePassword: u.MustChangePassword,
	}, nil
}

// InvalidateAll drops the whole principal cache (role/permission changes).
func (s *Service) InvalidateAll() {
	s.mu.Lock()
	s.cache = map[string]cacheEntry{}
	s.mu.Unlock()
}

// InvalidateUser drops cached principals of one user.
func (s *Service) InvalidateUser(userID int64) {
	s.mu.Lock()
	for k, v := range s.cache {
		if v.p.UserID == userID {
			delete(s.cache, k)
		}
	}
	s.mu.Unlock()
}

func (s *Service) invalidateSession(sid string) {
	s.mu.Lock()
	delete(s.cache, sid)
	s.mu.Unlock()
}

// EnsureAdmin creates the built-in admin (password = initialPassword, forced
// change on first login) when the users table is empty.
func (s *Service) EnsureAdmin(ctx context.Context, initialPassword string) (bool, error) {
	n, err := s.repo.CountUsers(ctx)
	if err != nil || n > 0 {
		return false, err
	}
	hash, err := HashPassword(initialPassword)
	if err != nil {
		return false, err
	}
	if err := s.repo.CreateBuiltinAdmin(ctx, hash); err != nil {
		return false, err
	}
	s.log.Warn("created built-in admin user; the password must be changed at first login", "username", "admin")
	return true, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
