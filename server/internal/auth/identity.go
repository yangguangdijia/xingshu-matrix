package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// SecretCipher encrypts TOTP secrets at rest (*crypto.Cipher satisfies it).
type SecretCipher interface {
	EncryptBytes(plaintext, aad []byte) (string, error)
	DecryptBytes(envelope string, aad []byte) ([]byte, error)
}

// identity bundles API tokens and MFA; nil when not enabled.
type identity struct {
	store   IdentityStore
	cipher  SecretCipher
	limiter *RateLimiter // per-user self-service MFA attempts
	issuer  string

	policyCache cachedMFAPolicy
}

type cachedMFAPolicy struct {
	p   MFAPolicy
	exp time.Time
}

const (
	// APITokenPrefix marks platform API tokens ("xsm_..."), so they can be told
	// apart from session JWTs and found by secret scanners.
	APITokenPrefix      = "xsm_"
	apiTokenDisplayLen  = len(APITokenPrefix) + 8
	maxActiveTokens     = 50
	maxTokenDays        = 3650
	mfaChallengeTTL     = 5 * time.Minute
	mfaChallengeMaxTry  = 5
	mfaSelfServicePerMn = 10
)

// EnableIdentity turns on API tokens, service-account tokens and MFA.
func (s *Service) EnableIdentity(st IdentityStore, c SecretCipher) {
	s.identity = &identity{store: st, cipher: c, limiter: NewRateLimiter(mfaSelfServicePerMn), issuer: "星枢 Matrix"}
}

// IdentityEnabled reports whether EnableIdentity was called.
func (s *Service) IdentityEnabled() bool { return s.identity != nil }

var errIdentityOff = httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "身份与令牌功能未启用")

func (s *Service) id() (*identity, error) {
	if s.identity == nil {
		return nil, errIdentityOff
	}
	return s.identity, nil
}

// ---------------------------------------------------------------------------
// API tokens

// IsAPIToken reports whether tok looks like a platform API token.
func IsAPIToken(tok string) bool {
	return strings.HasPrefix(tok, APITokenPrefix) && len(tok) > apiTokenDisplayLen
}

// NewAPITokenSecret returns a new plaintext token and its digest.
func NewAPITokenSecret() (token, hash string) {
	token = APITokenPrefix + randomToken(30)
	return token, HashSecret(token)
}

// TokenInput creates an API token.
type TokenInput struct {
	Name string `json:"name" binding:"required,max=64"`
	// Scopes narrows the token to a subset of the owner's permissions; empty = all of them.
	Scopes []string `json:"scopes"`
	// ExpiresInDays: 0 = never expires; otherwise 1-3650. ExpiresAt wins when both are set.
	ExpiresInDays int        `json:"expires_in_days"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

// CreatedToken is returned once on creation; Token is never shown again.
type CreatedToken struct {
	APIToken
	Token string `json:"token"`
}

// CreateAPIToken issues a token for ownerID on behalf of actorID. Scopes must be
// covered by the owner's current permissions.
func (s *Service) CreateAPIToken(ctx context.Context, actorID, ownerID int64, in TokenInput) (*CreatedToken, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return nil, httpx.Validation("令牌名称为 1-64 个字符")
	}
	now := s.now()
	var exp *time.Time
	switch {
	case in.ExpiresAt != nil:
		t := in.ExpiresAt.UTC()
		if !t.After(now.Add(time.Minute)) {
			return nil, httpx.Validation("到期时间必须晚于当前时间")
		}
		if t.After(now.AddDate(0, 0, maxTokenDays)) {
			return nil, httpx.Validation("有效期最长 3650 天")
		}
		exp = &t
	case in.ExpiresInDays < 0 || in.ExpiresInDays > maxTokenDays:
		return nil, httpx.Validation("有效期需为 0（永不过期）或 1-3650 天")
	case in.ExpiresInDays > 0:
		t := now.AddDate(0, 0, in.ExpiresInDays).UTC()
		exp = &t
	}
	owner, err := s.repo.UserByID(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if owner.Status != "active" {
		return nil, httpx.Validation("账号已禁用，不能创建令牌")
	}
	_, perms, _, err := s.access.UserAccess(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	scopes, err := normalizeScopes(in.Scopes, perms)
	if err != nil {
		return nil, err
	}
	existing, err := idt.store.ListAPITokens(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	active := 0
	for i := range existing {
		existing[i].derive(now)
		if existing[i].Status == "active" {
			active++
		}
	}
	if active >= maxActiveTokens {
		return nil, httpx.Conflict(fmt.Sprintf("每个账号最多 %d 个有效令牌，请先吊销不用的令牌", maxActiveTokens))
	}
	plain, hash := NewAPITokenSecret()
	t := &APIToken{UserID: ownerID, Name: name, Prefix: plain[:apiTokenDisplayLen], Hash: hash, Scopes: scopes, ExpiresAt: exp}
	if actorID > 0 {
		a := actorID
		t.CreatedBy = &a
	}
	if err := idt.store.CreateAPIToken(ctx, t); err != nil {
		return nil, err
	}
	t.derive(now)
	return &CreatedToken{APIToken: *t, Token: plain}, nil
}

// normalizeScopes validates scopes against the owner's permissions.
func normalizeScopes(scopes, ownerPerms []string) ([]string, error) {
	set := map[string]bool{}
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" {
			continue
		}
		if !rbac.ValidCode(sc) {
			return nil, httpx.Validation(fmt.Sprintf("权限格式不正确：%s", sc))
		}
		if !rbac.Covers(ownerPerms, sc) {
			return nil, httpx.Validation(fmt.Sprintf("令牌权限不能超过账号自身权限：%s", sc))
		}
		set[sc] = true
	}
	out := make([]string, 0, len(set))
	for sc := range set {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out, nil
}

// ListAPITokens lists a user's tokens (revoked ones for 30 days).
func (s *Service) ListAPITokens(ctx context.Context, ownerID int64) ([]APIToken, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	ts, err := idt.store.ListAPITokens(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for i := range ts {
		ts[i].derive(now)
	}
	if ts == nil {
		ts = []APIToken{}
	}
	return ts, nil
}

// RevokeAPIToken revokes one of ownerID's tokens and drops it from the cache.
func (s *Service) RevokeAPIToken(ctx context.Context, ownerID, tokenID, by int64) error {
	idt, err := s.id()
	if err != nil {
		return err
	}
	if err := idt.store.RevokeAPIToken(ctx, ownerID, tokenID, by); err != nil {
		return err
	}
	s.mu.Lock()
	for k, v := range s.cache {
		if v.p.TokenID == tokenID {
			delete(s.cache, k)
		}
	}
	s.mu.Unlock()
	return nil
}

var errTokenInvalid = httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "访问令牌无效、已过期或已吊销")

// AuthenticateAPIToken resolves an "xsm_" token to a principal whose
// permissions are the owner's current permissions narrowed to the token scopes.
func (s *Service) AuthenticateAPIToken(ctx context.Context, token, ip string) (*rbac.Principal, error) {
	if s.identity == nil || !IsAPIToken(token) || len(token) > 128 {
		return nil, errTokenInvalid
	}
	hash := HashSecret(token)
	key := "tok:" + hash
	now := s.now()
	s.mu.RLock()
	e, ok := s.cache[key]
	s.mu.RUnlock()
	if ok && e.exp.After(now) {
		return e.p, nil
	}
	t, err := s.identity.store.APITokenByHash(ctx, hash)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if !EqualHash(t.Hash, hash) {
		return nil, errTokenInvalid
	}
	t.derive(now)
	if t.Status != "active" {
		return nil, errTokenInvalid
	}
	base, err := s.loadPrincipal(ctx, t.UserID, "")
	if err != nil {
		return nil, err
	}
	p := *base
	p.AuthMethod = "token"
	p.TokenID = t.ID
	p.TokenName = t.Name
	if len(t.Scopes) > 0 {
		p.Permissions = rbac.Intersect(base.Permissions, t.Scopes)
	}
	if p.Permissions == nil {
		p.Permissions = []string{}
	}
	if err := s.identity.store.TouchAPIToken(ctx, t.ID, ip); err != nil {
		s.log.Warn("record token use failed", "token_id", t.ID, "err", err)
	}
	ttl := s.opts.PrincipalCacheTTL
	if t.ExpiresAt != nil && t.ExpiresAt.Sub(now) < ttl {
		ttl = t.ExpiresAt.Sub(now)
	}
	s.mu.Lock()
	s.cache[key] = cacheEntry{p: &p, exp: now.Add(ttl)}
	s.mu.Unlock()
	return &p, nil
}

// ---------------------------------------------------------------------------
// MFA policy

// LoadMFAPolicy returns the enforcement policy (30 s cache).
func (s *Service) LoadMFAPolicy(ctx context.Context) (MFAPolicy, error) {
	idt, err := s.id()
	if err != nil {
		return MFAPolicy{}, err
	}
	now := s.now()
	s.mu.RLock()
	c := idt.policyCache
	s.mu.RUnlock()
	if now.Before(c.exp) {
		return c.p, nil
	}
	p, err := idt.store.LoadMFAPolicy(ctx)
	if err != nil {
		return MFAPolicy{}, err
	}
	if p.RequiredRoles == nil {
		p.RequiredRoles = []string{}
	}
	s.mu.Lock()
	idt.policyCache = cachedMFAPolicy{p: p, exp: now.Add(30 * time.Second)}
	s.mu.Unlock()
	return p, nil
}

// SaveMFAPolicy validates and stores the enforcement policy.
func (s *Service) SaveMFAPolicy(ctx context.Context, p MFAPolicy, by int64) (MFAPolicy, error) {
	idt, err := s.id()
	if err != nil {
		return MFAPolicy{}, err
	}
	set := map[string]bool{}
	for _, r := range p.RequiredRoles {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if len(r) > 64 || !rbac.ValidCode(r+":x") {
			return MFAPolicy{}, httpx.Validation("角色编码格式不正确：" + r)
		}
		set[r] = true
	}
	out := MFAPolicy{RequiredRoles: []string{}}
	for r := range set {
		out.RequiredRoles = append(out.RequiredRoles, r)
	}
	sort.Strings(out.RequiredRoles)
	if err := idt.store.SaveMFAPolicy(ctx, out, by); err != nil {
		return MFAPolicy{}, err
	}
	s.mu.Lock()
	idt.policyCache = cachedMFAPolicy{}
	s.mu.Unlock()
	return out, nil
}

// mfaRequiredFor reports whether one of the user's roles enforces MFA.
func (s *Service) mfaRequiredFor(ctx context.Context, userID int64) (bool, error) {
	pol, err := s.LoadMFAPolicy(ctx)
	if err != nil || len(pol.RequiredRoles) == 0 {
		return false, err
	}
	roles, _, _, err := s.access.UserAccess(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, r := range roles {
		for _, q := range pol.RequiredRoles {
			if r == q {
				return true, nil
			}
		}
	}
	return false, nil
}
