package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// MFARequiredError is returned by Login when the password was correct but a
// second factor is needed. The HTTP layer turns it into a 200 response carrying
// the challenge ticket instead of a token pair.
type MFARequiredError struct {
	Ticket    string `json:"mfa_ticket"`
	Enroll    bool   `json:"mfa_enroll"`
	ExpiresIn int    `json:"expires_in"`
	// Secret / OTPAuthURI are set only for Enroll (MFA enforced but not set up yet).
	Secret     string `json:"secret,omitempty"`
	OTPAuthURI string `json:"otpauth_uri,omitempty"`
}

func (e *MFARequiredError) Error() string { return "mfa required" }

func mfaAAD(userID int64) []byte { return []byte("user_mfa:" + strconv.FormatInt(userID, 10)) }

var (
	errChallengeInvalid = httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "验证已过期，请重新登录")
	errMFACode          = httpx.NewError(http.StatusUnauthorized, httpx.CodeInvalidCredentials, "验证码错误")
	errMFATooMany       = httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests, "验证码错误次数过多，请重新登录")
	errMFANotEnabled    = httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "尚未启用两步验证")
)

// mfaGate returns nil when no second factor is needed, *MFARequiredError when
// it is (a challenge was created), or another error.
func (s *Service) mfaGate(ctx context.Context, u *User, ip, userAgent string) error {
	if s.identity == nil {
		return nil
	}
	st := s.identity.store
	rec, err := st.GetMFA(ctx, u.ID)
	if err != nil && !errors.Is(err, httpx.ErrNotFound) {
		return err
	}
	ch := &MFAChallenge{UserID: u.ID, IP: ip, UserAgent: truncate(userAgent, 512), ExpiresAt: s.now().Add(mfaChallengeTTL)}
	out := &MFARequiredError{ExpiresIn: int(mfaChallengeTTL.Seconds())}
	if rec != nil && rec.Enabled {
		ch.Purpose = "verify"
	} else {
		required, err := s.mfaRequiredFor(ctx, u.ID)
		if err != nil {
			return err
		}
		if !required {
			return nil
		}
		secret := NewTOTPSecret()
		enc, err := s.identity.cipher.EncryptBytes([]byte(secret), mfaAAD(u.ID))
		if err != nil {
			return err
		}
		ch.Purpose, ch.PendingSecret = "enroll", enc
		out.Enroll, out.Secret, out.OTPAuthURI = true, secret, TOTPURI(s.identity.issuer, u.Username, secret)
	}
	ticket := randomToken(32)
	ch.Hash = HashSecret(ticket)
	if err := st.CreateChallenge(ctx, ch); err != nil {
		return err
	}
	out.Ticket = ticket
	return out
}

// MFALoginResult is returned when a challenge is completed.
type MFALoginResult struct {
	Pair *TokenPair
	User *User
	// RecoveryCodes is set once, when the challenge enrolled the user.
	RecoveryCodes []string
}

// CompleteMFA exchanges a challenge ticket and a TOTP / recovery code for a session.
// Failed codes count toward the challenge limit and the account lockout.
func (s *Service) CompleteMFA(ctx context.Context, ticket, code, ip, userAgent string) (*MFALoginResult, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	if ticket == "" || len(ticket) > 128 {
		return nil, errChallengeInvalid
	}
	hash := HashSecret(ticket)
	ch, err := idt.store.ChallengeByHash(ctx, hash)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errChallengeInvalid
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !ch.ExpiresAt.After(now) || ch.Attempts >= mfaChallengeMaxTry {
		_, _ = idt.store.DeleteChallenge(ctx, hash)
		return nil, errChallengeInvalid
	}
	u, err := s.repo.UserByID(ctx, ch.UserID)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, errChallengeInvalid
		}
		return nil, err
	}
	if u.LockedUntil != nil && u.LockedUntil.After(now) {
		_, _ = idt.store.DeleteChallenge(ctx, hash)
		return nil, lockedError(*u.LockedUntil, now)
	}
	if u.Status != "active" || u.IsService() {
		_, _ = idt.store.DeleteChallenge(ctx, hash)
		return nil, errDisabled
	}

	var recovery []string
	ok := false
	switch ch.Purpose {
	case "verify":
		ok, err = s.verifySecondFactor(ctx, u.ID, code)
		if errors.Is(err, errMFANotEnabled) { // reset by an administrator meanwhile
			_, _ = idt.store.DeleteChallenge(ctx, hash)
			return nil, errChallengeInvalid
		}
		if err != nil {
			return nil, err
		}
	case "enroll":
		secret, derr := idt.cipher.DecryptBytes(ch.PendingSecret, mfaAAD(u.ID))
		if derr != nil {
			return nil, derr
		}
		if step, match := VerifyTOTP(string(secret), code, now, 0); match {
			codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
			activated, aerr := idt.store.ActivateMFA(ctx, u.ID, ch.PendingSecret, step, hashes)
			if aerr != nil {
				return nil, aerr
			}
			if !activated { // enrolled concurrently elsewhere: log in again with that device
				_, _ = idt.store.DeleteChallenge(ctx, hash)
				return nil, errChallengeInvalid
			}
			ok, recovery = true, codes
		}
	default:
		return nil, errChallengeInvalid
	}
	if !ok {
		return nil, s.mfaFailure(ctx, u, hash)
	}
	consumed, err := idt.store.DeleteChallenge(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !consumed { // the same ticket completed concurrently
		return nil, errChallengeInvalid
	}
	pair, err := s.openSession(ctx, u, ip, userAgent, s.sessionTTL(ctx))
	if err != nil {
		return nil, err
	}
	return &MFALoginResult{Pair: pair, User: u, RecoveryCodes: recovery}, nil
}

// mfaFailure records a failed code on the challenge and toward account lockout.
func (s *Service) mfaFailure(ctx context.Context, u *User, hash string) error {
	n, err := s.identity.store.BumpChallenge(ctx, hash)
	if err != nil && !errors.Is(err, httpx.ErrNotFound) {
		return err
	}
	pol := CurrentPolicy(ctx)
	maxFail, lockFor := s.opts.MaxLoginFailures, s.opts.LockoutDuration
	if pol.MaxLoginFailures > 0 {
		maxFail = pol.MaxLoginFailures
	}
	if pol.LockoutMinutes > 0 {
		lockFor = time.Duration(pol.LockoutMinutes) * time.Minute
	}
	if maxFail > 0 {
		until := s.now().Add(lockFor)
		if _, locked, lerr := s.repo.RecordLoginFailure(ctx, u.ID, maxFail, until); lerr == nil && locked {
			_, _ = s.identity.store.DeleteChallenge(ctx, hash)
			s.log.Warn("account locked after repeated MFA failures", "user", u.Username)
			return lockedError(until, s.now())
		}
	}
	if n >= mfaChallengeMaxTry {
		_, _ = s.identity.store.DeleteChallenge(ctx, hash)
		return errMFATooMany
	}
	return errMFACode.WithDetails(map[string]any{"remaining_attempts": mfaChallengeMaxTry - n})
}

// verifySecondFactor checks a TOTP code (once per time step) or consumes a recovery code.
func (s *Service) verifySecondFactor(ctx context.Context, userID int64, code string) (bool, error) {
	idt := s.identity
	rec, err := idt.store.GetMFA(ctx, userID)
	if errors.Is(err, httpx.ErrNotFound) || (err == nil && !rec.Enabled) {
		return false, errMFANotEnabled
	}
	if err != nil {
		return false, err
	}
	if looksLikeRecoveryCode(code) {
		return idt.store.ConsumeRecoveryCode(ctx, userID, HashRecoveryCode(code))
	}
	secret, err := idt.cipher.DecryptBytes(rec.SecretEnc, mfaAAD(userID))
	if err != nil {
		return false, err
	}
	step, ok := VerifyTOTP(string(secret), code, s.now(), rec.LastStep)
	if !ok {
		return false, nil
	}
	// Conditional update: a concurrent request with the same code loses.
	return idt.store.AdvanceMFAStep(ctx, userID, step)
}

// ---------------------------------------------------------------------------
// Self-service

// MFAStatus is the self-service view.
type MFAStatus struct {
	Enabled           bool       `json:"enabled"`
	EnabledAt         *time.Time `json:"enabled_at"`
	RecoveryCodesLeft int        `json:"recovery_codes_left"`
	Required          bool       `json:"required"`
}

// MFAStatusOf returns the MFA state of a user.
func (s *Service) MFAStatusOf(ctx context.Context, userID int64) (*MFAStatus, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	out := &MFAStatus{}
	rec, err := idt.store.GetMFA(ctx, userID)
	if err != nil && !errors.Is(err, httpx.ErrNotFound) {
		return nil, err
	}
	if rec != nil && rec.Enabled {
		out.Enabled, out.EnabledAt, out.RecoveryCodesLeft = true, rec.EnabledAt, len(rec.RecoveryHashes)
	}
	if out.Required, err = s.mfaRequiredFor(ctx, userID); err != nil {
		return nil, err
	}
	return out, nil
}

// MFASetup is returned when self-service enrolment starts.
type MFASetup struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauth_uri"`
}

func (s *Service) selfServiceAllowed(p *rbac.Principal) error {
	if ok, wait := s.identity.limiter.Allow("mfa:" + strconv.FormatInt(p.UserID, 10)); !ok {
		return httpx.NewError(http.StatusTooManyRequests, httpx.CodeTooManyRequests,
			fmt.Sprintf("操作过于频繁，请 %d 秒后再试", int(wait.Seconds())+1))
	}
	return nil
}

// BeginMFASetup creates a pending secret for the caller.
func (s *Service) BeginMFASetup(ctx context.Context, p *rbac.Principal) (*MFASetup, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	secret := NewTOTPSecret()
	enc, err := idt.cipher.EncryptBytes([]byte(secret), mfaAAD(p.UserID))
	if err != nil {
		return nil, err
	}
	saved, err := idt.store.SavePendingMFA(ctx, p.UserID, enc)
	if err != nil {
		return nil, err
	}
	if !saved {
		return nil, httpx.Conflict("已启用两步验证；如需更换设备，请先关闭后重新绑定")
	}
	return &MFASetup{Secret: secret, OTPAuthURI: TOTPURI(idt.issuer, p.Username, secret)}, nil
}

// ActivateMFASetup confirms the pending secret with a code and returns recovery codes (shown once).
func (s *Service) ActivateMFASetup(ctx context.Context, p *rbac.Principal, code string) ([]string, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	if err := s.selfServiceAllowed(p); err != nil {
		return nil, err
	}
	rec, err := idt.store.GetMFA(ctx, p.UserID)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, httpx.Validation("请先生成绑定密钥")
	}
	if err != nil {
		return nil, err
	}
	if rec.Enabled {
		return nil, httpx.Conflict("已启用两步验证")
	}
	secret, err := idt.cipher.DecryptBytes(rec.SecretEnc, mfaAAD(p.UserID))
	if err != nil {
		return nil, err
	}
	step, ok := VerifyTOTP(string(secret), code, s.now(), 0)
	if !ok {
		return nil, errMFACode
	}
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	activated, err := idt.store.ActivateMFA(ctx, p.UserID, rec.SecretEnc, step, hashes)
	if err != nil {
		return nil, err
	}
	if !activated {
		return nil, httpx.Conflict("已启用两步验证")
	}
	return codes, nil
}

// DisableMFA turns MFA off after verifying a current code; refused when a role enforces it.
func (s *Service) DisableMFA(ctx context.Context, p *rbac.Principal, code string) error {
	idt, err := s.id()
	if err != nil {
		return err
	}
	if err := s.selfServiceAllowed(p); err != nil {
		return err
	}
	required, err := s.mfaRequiredFor(ctx, p.UserID)
	if err != nil {
		return err
	}
	if required {
		return httpx.Forbidden("你所在的角色要求启用两步验证，不能关闭")
	}
	ok, err := s.verifySecondFactor(ctx, p.UserID, code)
	if err != nil {
		return err
	}
	if !ok {
		return errMFACode
	}
	return idt.store.DeleteMFA(ctx, p.UserID)
}

// RegenerateRecoveryCodes replaces all recovery codes after verifying a current code.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, p *rbac.Principal, code string) ([]string, error) {
	idt, err := s.id()
	if err != nil {
		return nil, err
	}
	if err := s.selfServiceAllowed(p); err != nil {
		return nil, err
	}
	ok, err := s.verifySecondFactor(ctx, p.UserID, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errMFACode
	}
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	if err := idt.store.SetRecoveryCodes(ctx, p.UserID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// ResetMFA removes a user's MFA enrolment (administrator action; the caller audits it).
// If a role enforces MFA the user is asked to enrol again at the next login.
func (s *Service) ResetMFA(ctx context.Context, userID int64) error {
	idt, err := s.id()
	if err != nil {
		return err
	}
	if _, err := s.repo.UserByID(ctx, userID); err != nil {
		return err
	}
	return idt.store.DeleteMFA(ctx, userID)
}

// MFAEnabledFor reports whether a user has MFA enabled (for admin lists).
func (s *Service) MFAEnabledFor(ctx context.Context, userID int64) (bool, error) {
	idt, err := s.id()
	if err != nil {
		return false, err
	}
	rec, err := idt.store.GetMFA(ctx, userID)
	if errors.Is(err, httpx.ErrNotFound) {
		return false, nil
	}
	return err == nil && rec.Enabled, err
}
