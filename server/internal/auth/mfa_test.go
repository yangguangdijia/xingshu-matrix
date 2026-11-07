package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const testPassword = "Str0ng!Passw0rd"

type harness struct {
	svc    *Service
	repo   *fakeRepo
	idt    *fakeIdentity
	access *fakeAccess
	now    time.Time
}

func newHarness(t *testing.T, maxFail int) *harness {
	t.Helper()
	BcryptCost = 4
	h := &harness{repo: newFakeRepo(), idt: newFakeIdentity(), access: &fakeAccess{roles: map[int64][]string{}, perms: map[int64][]string{}}}
	// 对齐到时间窗起点，避免测试中途跨窗
	h.now = time.Unix(time.Now().Unix()/30*30, 0)
	tm := NewTokenManager(strings.Repeat("k", 40), "matrix", 15*time.Minute)
	h.svc = NewService(h.repo, h.access, tm, Options{RefreshTTL: 24 * time.Hour, MaxLoginFailures: maxFail, LockoutDuration: 15 * time.Minute}, nil)
	h.svc.now = func() time.Time { return h.now }
	h.svc.EnableIdentity(h.idt, fakeCipher{})
	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	h.repo.add(&User{ID: 1, Username: "alice", PasswordHash: hash})
	h.repo.add(&User{ID: 2, Username: "robot", PasswordHash: "!service", AccountType: "service"})
	h.access.roles[1], h.access.perms[1] = []string{"ops"}, []string{"host:*", "user:read"}
	h.access.roles[2], h.access.perms[2] = []string{"viewer"}, []string{"*:read"}
	return h
}

// enroll turns MFA on for alice and returns the secret and recovery codes.
func (h *harness) enroll(t *testing.T) (string, []string) {
	t.Helper()
	secret := NewTOTPSecret()
	enc, _ := fakeCipher{}.EncryptBytes([]byte(secret), mfaAAD(1))
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	if ok, _ := h.idt.ActivateMFA(context.Background(), 1, enc, 0, hashes); !ok {
		t.Fatal("activate failed")
	}
	return secret, codes
}

func (h *harness) login(t *testing.T) *MFARequiredError {
	t.Helper()
	pair, _, err := h.svc.Login(context.Background(), "alice", testPassword, "10.0.0.1", "ua")
	var mfa *MFARequiredError
	if !errors.As(err, &mfa) {
		t.Fatalf("expected MFA challenge, got pair=%v err=%v", pair, err)
	}
	if mfa.Ticket == "" || mfa.ExpiresIn != int(mfaChallengeTTL.Seconds()) {
		t.Fatalf("bad challenge %+v", mfa)
	}
	return mfa
}

func status(err error) int {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := TOTPCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginWithoutMFAIssuesSession(t *testing.T) {
	h := newHarness(t, 5)
	pair, u, err := h.svc.Login(context.Background(), "alice", testPassword, "ip", "ua")
	if err != nil || pair == nil || pair.AccessToken == "" || u.ID != 1 {
		t.Fatalf("login failed: %v", err)
	}
	if len(h.idt.challenges) != 0 {
		t.Fatal("no challenge expected")
	}
}

func TestMFALoginFlow(t *testing.T) {
	h := newHarness(t, 100)
	secret, _ := h.enroll(t)
	ctx := context.Background()

	// 密码错误时不产生挑战
	if _, _, err := h.svc.Login(ctx, "alice", "wrong", "ip", "ua"); status(err) != http.StatusUnauthorized {
		t.Fatalf("wrong password: %v", err)
	}
	if len(h.idt.challenges) != 0 {
		t.Fatal("challenge created for wrong password")
	}

	ch := h.login(t)
	if ch.Enroll || ch.Secret != "" {
		t.Fatal("enrolled user must not get a new secret")
	}
	// 错误验证码：计数并返回剩余次数
	_, err := h.svc.CompleteMFA(ctx, ch.Ticket, "000000", "ip", "ua")
	if code(t, secret, h.now) == "000000" {
		t.Skip("unlucky code")
	}
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("wrong code: %v", err)
	}
	if d, _ := he.Details.(map[string]any); d["remaining_attempts"] != mfaChallengeMaxTry-1 {
		t.Fatalf("remaining attempts: %v", he.Details)
	}
	// 正确验证码换取会话
	c1 := code(t, secret, h.now)
	res, err := h.svc.CompleteMFA(ctx, ch.Ticket, c1, "ip", "ua")
	if err != nil || res.Pair == nil || res.Pair.AccessToken == "" || len(res.RecoveryCodes) != 0 {
		t.Fatalf("complete: %v", err)
	}
	if p, err := h.svc.Authenticate(ctx, res.Pair.AccessToken); err != nil || p.UserID != 1 || p.AuthMethod != "session" {
		t.Fatalf("session token unusable: %v", err)
	}
	// 票据一次性
	if _, err := h.svc.CompleteMFA(ctx, ch.Ticket, c1, "ip", "ua"); status(err) != http.StatusUnauthorized {
		t.Fatalf("ticket reused: %v", err)
	}
	// 防重放：同一时间窗的验证码不能再次登录
	ch2 := h.login(t)
	if _, err := h.svc.CompleteMFA(ctx, ch2.Ticket, c1, "ip", "ua"); err == nil {
		t.Fatal("replayed TOTP accepted")
	}
	// 下一个时间窗的新验证码可以
	h.now = h.now.Add(30 * time.Second)
	if _, err := h.svc.CompleteMFA(ctx, ch2.Ticket, code(t, secret, h.now), "ip", "ua"); err != nil {
		t.Fatalf("next window code: %v", err)
	}
}

func TestMFARecoveryCodeSingleUse(t *testing.T) {
	h := newHarness(t, 100)
	_, codes := h.enroll(t)
	ctx := context.Background()
	ch := h.login(t)
	if _, err := h.svc.CompleteMFA(ctx, ch.Ticket, strings.ToLower(codes[3]), "ip", "ua"); err != nil {
		t.Fatalf("recovery code rejected: %v", err)
	}
	st, err := h.svc.MFAStatusOf(ctx, 1)
	if err != nil || !st.Enabled || st.RecoveryCodesLeft != RecoveryCodeCount-1 {
		t.Fatalf("status %+v %v", st, err)
	}
	ch = h.login(t)
	if _, err := h.svc.CompleteMFA(ctx, ch.Ticket, codes[3], "ip", "ua"); err == nil {
		t.Fatal("recovery code reused")
	}
}

func TestMFAChallengeAttemptLimit(t *testing.T) {
	h := newHarness(t, 100)
	secret, _ := h.enroll(t)
	ctx := context.Background()
	ch := h.login(t)
	bad := "111111"
	if code(t, secret, h.now) == bad || code(t, secret, h.now.Add(-30*time.Second)) == bad || code(t, secret, h.now.Add(30*time.Second)) == bad {
		bad = "222222"
	}
	var last error
	for i := 0; i < mfaChallengeMaxTry; i++ {
		_, last = h.svc.CompleteMFA(ctx, ch.Ticket, bad, "ip", "ua")
	}
	if status(last) != http.StatusTooManyRequests {
		t.Fatalf("want 429 after %d attempts, got %v", mfaChallengeMaxTry, last)
	}
	// 票据已作废，正确验证码也不行
	if _, err := h.svc.CompleteMFA(ctx, ch.Ticket, code(t, secret, h.now), "ip", "ua"); status(err) != http.StatusUnauthorized {
		t.Fatalf("ticket should be gone: %v", err)
	}
}

func TestMFAFailuresLockAccount(t *testing.T) {
	h := newHarness(t, 3)
	secret, _ := h.enroll(t)
	ctx := context.Background()
	ch := h.login(t)
	bad := "333333"
	if code(t, secret, h.now) == bad {
		bad = "444444"
	}
	var last error
	for i := 0; i < 3; i++ {
		_, last = h.svc.CompleteMFA(ctx, ch.Ticket, bad, "ip", "ua")
	}
	var he *httpx.Error
	if !errors.As(last, &he) || he.Code != httpx.CodeAccountLocked {
		t.Fatalf("want account locked, got %v", last)
	}
	if _, _, err := h.svc.Login(ctx, "alice", testPassword, "ip", "ua"); !errors.As(err, &he) || he.Code != httpx.CodeAccountLocked {
		t.Fatalf("locked account logged in: %v", err)
	}
}

func TestMFAEnforcedEnrollAtLogin(t *testing.T) {
	h := newHarness(t, 100)
	ctx := context.Background()
	if _, err := h.svc.SaveMFAPolicy(ctx, MFAPolicy{RequiredRoles: []string{" ops ", "ops", ""}}, 1); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.svc.LoadMFAPolicy(ctx); len(p.RequiredRoles) != 1 || p.RequiredRoles[0] != "ops" {
		t.Fatalf("policy not normalised: %+v", p)
	}
	if _, err := h.svc.SaveMFAPolicy(ctx, MFAPolicy{RequiredRoles: []string{"Bad Role"}}, 1); status(err) != http.StatusBadRequest {
		t.Fatalf("invalid role accepted: %v", err)
	}
	ch := h.login(t)
	if !ch.Enroll || ch.Secret == "" || !strings.HasPrefix(ch.OTPAuthURI, "otpauth://totp/") {
		t.Fatalf("expected enrolment challenge: %+v", ch)
	}
	// 挑战里的待绑定密钥加密保存
	for _, c := range h.idt.challenges {
		if c.Purpose != "enroll" || strings.Contains(c.PendingSecret, ch.Secret) {
			t.Fatalf("pending secret stored in clear / wrong purpose: %+v", c)
		}
	}
	res, err := h.svc.CompleteMFA(ctx, ch.Ticket, code(t, ch.Secret, h.now), "ip", "ua")
	if err != nil || len(res.RecoveryCodes) != RecoveryCodeCount || res.Pair == nil {
		t.Fatalf("enroll complete: %v", err)
	}
	st, _ := h.svc.MFAStatusOf(ctx, 1)
	if !st.Enabled || !st.Required {
		t.Fatalf("status after enroll: %+v", st)
	}
	// 强制策略下不能自行关闭
	p := &rbac.Principal{UserID: 1, Username: "alice", SessionID: "s"}
	h.now = h.now.Add(30 * time.Second)
	if err := h.svc.DisableMFA(ctx, p, code(t, ch.Secret, h.now)); status(err) != http.StatusForbidden {
		t.Fatalf("disable under policy: %v", err)
	}
	// 管理员重置后下次登录重新绑定
	if err := h.svc.ResetMFA(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ch := h.login(t); !ch.Enroll {
		t.Fatal("expected re-enrolment after reset")
	}
}

func TestMFASelfService(t *testing.T) {
	h := newHarness(t, 100)
	ctx := context.Background()
	p := &rbac.Principal{UserID: 1, Username: "alice", SessionID: "s"}
	if _, err := h.svc.ActivateMFASetup(ctx, p, "123456"); status(err) != http.StatusBadRequest {
		t.Fatalf("activate without setup: %v", err)
	}
	setup, err := h.svc.BeginMFASetup(ctx, p)
	if err != nil || setup.Secret == "" {
		t.Fatal(err)
	}
	if _, err := h.svc.ActivateMFASetup(ctx, p, "12345"); err == nil {
		t.Fatal("bad code accepted")
	}
	codes, err := h.svc.ActivateMFASetup(ctx, p, code(t, setup.Secret, h.now))
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("activate: %v", err)
	}
	if _, err := h.svc.BeginMFASetup(ctx, p); status(err) != http.StatusConflict {
		t.Fatalf("re-setup while enabled: %v", err)
	}
	// 绑定用过的时间窗不能再用于重新生成恢复码
	if _, err := h.svc.RegenerateRecoveryCodes(ctx, p, code(t, setup.Secret, h.now)); err == nil {
		t.Fatal("replayed code accepted for regenerate")
	}
	newCodes, err := h.svc.RegenerateRecoveryCodes(ctx, p, codes[0])
	if err != nil || len(newCodes) != RecoveryCodeCount {
		t.Fatalf("regenerate with recovery code: %v", err)
	}
	// 旧恢复码全部失效
	h.now = h.now.Add(30 * time.Second)
	if err := h.svc.DisableMFA(ctx, p, codes[1]); err == nil {
		t.Fatal("old recovery code still valid")
	}
	if err := h.svc.DisableMFA(ctx, p, code(t, setup.Secret, h.now)); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if st, _ := h.svc.MFAStatusOf(ctx, 1); st.Enabled {
		t.Fatal("still enabled")
	}
}

func TestServiceAccountCannotLogin(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	if _, _, err := h.svc.Login(ctx, "robot", "anything", "ip", "ua"); status(err) != http.StatusUnauthorized {
		t.Fatalf("service account password login: %v", err)
	}
	if _, _, err := h.svc.LoginExternal(ctx, 2, "ip", "ua"); status(err) != http.StatusForbidden {
		t.Fatalf("service account SSO login: %v", err)
	}
	if err := h.svc.ResetPassword(ctx, 2, testPassword); status(err) != http.StatusBadRequest {
		t.Fatalf("service account password reset: %v", err)
	}
}

func TestAPITokenScopesAndRevocation(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	if _, err := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "ci", Scopes: []string{"user:manage"}}); status(err) != http.StatusBadRequest {
		t.Fatalf("scope beyond owner perms accepted: %v", err)
	}
	if _, err := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "  "}); status(err) != http.StatusBadRequest {
		t.Fatalf("empty name accepted: %v", err)
	}
	if _, err := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "x", ExpiresInDays: 4000}); status(err) != http.StatusBadRequest {
		t.Fatalf("too long expiry accepted: %v", err)
	}
	ct, err := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "ci", Scopes: []string{"host:read", "host:read"}, ExpiresInDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !IsAPIToken(ct.Token) || ct.Prefix != ct.Token[:apiTokenDisplayLen] || len(ct.Scopes) != 1 || ct.Status != "active" {
		t.Fatalf("bad created token %+v", ct.APIToken)
	}
	for _, st := range h.idt.tokens {
		if st.Hash == ct.Token || strings.Contains(st.Hash, ct.Token) {
			t.Fatal("plaintext stored")
		}
	}
	p, err := h.svc.AuthenticateAPIToken(ctx, ct.Token, "10.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if !p.ViaToken() || p.AuthMethod != "token" || p.TokenName != "ci" || p.SessionID != "" {
		t.Fatalf("principal %+v", p)
	}
	if !p.Can("host:read") || p.Can("host:write") || p.Can("user:read") {
		t.Fatalf("scope not applied: %v", p.Permissions)
	}
	// 通用入口也识别令牌
	if p2, err := h.svc.Authenticate(ctx, ct.Token); err != nil || p2.TokenID != ct.ID {
		t.Fatalf("Authenticate(token): %v", err)
	}
	// 无 scopes 继承所有者权限；所有者权限收缩后令牌随之收缩
	full, _ := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "full"})
	h.access.perms[1] = []string{"host:read"}
	h.svc.InvalidateAll()
	pf, err := h.svc.AuthenticateAPIToken(ctx, full.Token, "ip")
	if err != nil || pf.Can("host:write") || !pf.Can("host:read") {
		t.Fatalf("inherited perms: %v %v", pf, err)
	}
	// 吊销：他人不能吊销，本人吊销后立即失效（缓存清除）
	if err := h.svc.RevokeAPIToken(ctx, 2, ct.ID, 2); err == nil {
		t.Fatal("revoked someone else's token")
	}
	if err := h.svc.RevokeAPIToken(ctx, 1, ct.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AuthenticateAPIToken(ctx, ct.Token, "ip"); status(err) != http.StatusUnauthorized {
		t.Fatalf("revoked token accepted: %v", err)
	}
	ts, _ := h.svc.ListAPITokens(ctx, 1)
	revoked := 0
	for _, x := range ts {
		if x.Status == "revoked" {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("list status: %+v", ts)
	}
	// 过期
	h.now = h.now.Add(31 * 24 * time.Hour)
	exp, _ := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "short", ExpiresInDays: 1})
	h.now = h.now.Add(25 * time.Hour)
	h.svc.InvalidateAll()
	if _, err := h.svc.AuthenticateAPIToken(ctx, exp.Token, "ip"); status(err) != http.StatusUnauthorized {
		t.Fatalf("expired token accepted: %v", err)
	}
	// 禁用账号不能建令牌，已有令牌失效
	h.repo.users[1].Status = "disabled"
	h.svc.InvalidateAll()
	if _, err := h.svc.AuthenticateAPIToken(ctx, full.Token, "ip"); err == nil {
		t.Fatal("token of disabled user accepted")
	}
	if _, err := h.svc.CreateAPIToken(ctx, 1, 1, TokenInput{Name: "x"}); err == nil {
		t.Fatal("token created for disabled user")
	}
	if _, err := h.svc.AuthenticateAPIToken(ctx, "xsm_doesnotexist_000000000000000", "ip"); status(err) != http.StatusUnauthorized {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestIdentityDisabled(t *testing.T) {
	tm := NewTokenManager(strings.Repeat("k", 40), "matrix", time.Minute)
	s := NewService(newFakeRepo(), &fakeAccess{}, tm, Options{RefreshTTL: time.Hour}, nil)
	if s.IdentityEnabled() {
		t.Fatal("enabled by default")
	}
	if _, err := s.ListAPITokens(context.Background(), 1); status(err) != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
	if _, err := s.AuthenticateAPIToken(context.Background(), "xsm_aaaaaaaaaaaaaaaaaaaa", ""); status(err) != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
}
