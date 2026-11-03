package auth

import (
	"context"
	"sync/atomic"
	"time"
)

// Policy is the runtime-adjustable security policy (settings key "security.policy").
// Zero values fall back to the static configuration / built-in defaults.
type Policy struct {
	PasswordMinLength  int `json:"password_min_length"`  // 8-64
	PasswordMinClasses int `json:"password_min_classes"` // 2-4 of lower/upper/digit/symbol
	MaxLoginFailures   int `json:"max_login_failures"`   // 3-20
	LockoutMinutes     int `json:"lockout_minutes"`      // 5-1440
	SessionHours       int `json:"session_hours"`        // refresh-token lifetime, 1-720
}

// DefaultPolicy mirrors the historical hard-coded rules.
var DefaultPolicy = Policy{PasswordMinLength: 8, PasswordMinClasses: 3}

// Normalize clamps every field into its allowed range; zero means "use default".
func (p Policy) Normalize() Policy {
	clamp := func(v, lo, hi int) int {
		if v == 0 {
			return 0
		}
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	p.PasswordMinLength = clamp(p.PasswordMinLength, 8, 64)
	p.PasswordMinClasses = clamp(p.PasswordMinClasses, 2, 4)
	p.MaxLoginFailures = clamp(p.MaxLoginFailures, 3, 20)
	p.LockoutMinutes = clamp(p.LockoutMinutes, 5, 1440)
	p.SessionHours = clamp(p.SessionHours, 1, 720)
	if p.PasswordMinLength == 0 {
		p.PasswordMinLength = DefaultPolicy.PasswordMinLength
	}
	if p.PasswordMinClasses == 0 {
		p.PasswordMinClasses = DefaultPolicy.PasswordMinClasses
	}
	return p
}

// Validate reports out-of-range values (used when the policy is saved).
func (p Policy) Validate() error {
	type rng struct {
		name   string
		v      int
		lo, hi int
	}
	for _, r := range []rng{
		{"密码最小长度", p.PasswordMinLength, 8, 64},
		{"密码字符类别", p.PasswordMinClasses, 2, 4},
		{"登录失败锁定次数", p.MaxLoginFailures, 3, 20},
		{"锁定时长（分钟）", p.LockoutMinutes, 5, 1440},
		{"会话有效期（小时）", p.SessionHours, 1, 720},
	} {
		if r.v != 0 && (r.v < r.lo || r.v > r.hi) {
			return policyRangeError(r.name, r.lo, r.hi)
		}
	}
	return nil
}

type policySource func(ctx context.Context) (Policy, error)

type cachedPolicy struct {
	p   Policy
	exp time.Time
}

var (
	policyLoader atomic.Value // policySource
	policyCache  atomic.Value // cachedPolicy
)

// SetPolicySource installs the loader (typically backed by the settings store).
func SetPolicySource(fn func(ctx context.Context) (Policy, error)) {
	policyLoader.Store(policySource(fn))
}

// InvalidatePolicy drops the cached policy so the next read reloads it.
func InvalidatePolicy() { policyCache.Store(cachedPolicy{}) }

// CurrentPolicy returns the effective policy (30 s cache).
func CurrentPolicy(ctx context.Context) Policy {
	if c, ok := policyCache.Load().(cachedPolicy); ok && time.Now().Before(c.exp) {
		return c.p
	}
	p := DefaultPolicy
	if fn, ok := policyLoader.Load().(policySource); ok && fn != nil {
		if got, err := fn(ctx); err == nil {
			p = got
		}
	}
	p = p.Normalize()
	policyCache.Store(cachedPolicy{p: p, exp: time.Now().Add(30 * time.Second)})
	return p
}
