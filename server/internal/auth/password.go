// Package auth implements login (bcrypt + lockout + per-IP rate limit), JWT
// access tokens bound to server-side sessions, rotating refresh tokens, logout,
// password change and the forced first-login password change for admin.
package auth

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// BcryptCost is the work factor used for new hashes.
var BcryptCost = 12

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	if len(pw) > 72 {
		return "", httpx.Validation("password must be at most 72 bytes")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	return string(b), err
}

// VerifyPassword reports whether pw matches hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// a valid bcrypt hash used to equalise timing for unknown usernames
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("xingshu-matrix-dummy"), bcrypt.MinCost)

// ErrWeakPassword is wrapped by ValidatePassword errors.
var ErrWeakPassword = errors.New("weak password")

// ValidatePassword enforces the platform password policy: 8-72 bytes, at least
// three of {lowercase, uppercase, digit, symbol}, and not containing the username.
func ValidatePassword(pw, username string) error {
	pol := CurrentPolicy(context.Background())
	if utf8.RuneCountInString(pw) < pol.PasswordMinLength || len(pw) > 72 {
		return httpx.Validation(fmt.Sprintf("密码长度需为 %d-72 位", pol.PasswordMinLength))
	}
	var lower, upper, digit, symbol bool
	for _, r := range pw {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsSpace(r):
			return httpx.Validation("密码不能包含空白字符")
		default:
			symbol = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, symbol} {
		if b {
			classes++
		}
	}
	if classes < pol.PasswordMinClasses {
		return httpx.Validation(fmt.Sprintf("密码需至少包含大写字母、小写字母、数字、符号中的 %d 类", pol.PasswordMinClasses))
	}
	if username != "" && len(username) >= 3 && containsFold(pw, username) {
		return httpx.Validation("密码不能包含用户名")
	}
	return nil
}

func containsFold(s, sub string) bool {
	ls, lsub := []rune(toLower(s)), []rune(toLower(sub))
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if string(ls[i:i+len(lsub)]) == string(lsub) {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	r := []rune(s)
	for i := range r {
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}

func policyRangeError(name string, lo, hi int) error {
	return httpx.Validation(fmt.Sprintf("%s需在 %d-%d 之间", name, lo, hi))
}
