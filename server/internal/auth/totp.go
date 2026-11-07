package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) with the parameters every authenticator app supports:
// HMAC-SHA1, 6 digits, 30 second steps. Implemented with the standard library.
const (
	totpDigits = 6
	totpPeriod = 30
	// totpSkew accepts codes one step before / after the current one (clock drift).
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret, base32 encoded (no padding).
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	s = strings.TrimRight(s, "=")
	return b32.DecodeString(s)
}

// totpStep returns the time step counter for t.
func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// hotp computes the RFC 4226 value for counter.
func hotp(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, v%mod)
}

// TOTPCode returns the code for secret at time t.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, totpStep(t)), nil
}

// VerifyTOTP checks code against secret at time t (±totpSkew steps). It returns
// the matched step so callers can reject reuse (a step may be used only once).
// Only steps strictly greater than afterStep are accepted.
func VerifyTOTP(secret, code string, t time.Time, afterStep int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil || len(key) == 0 {
		return 0, false
	}
	now := totpStep(t)
	matched := int64(0)
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		s := now + d
		// constant-time compare; keep looping so timing does not reveal the offset
		if subtle.ConstantTimeCompare([]byte(hotp(key, s)), []byte(code)) == 1 && s > afterStep && matched == 0 {
			matched = s
		}
	}
	return matched, matched != 0
}

// TOTPURI returns the otpauth:// provisioning URI understood by authenticator apps.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Recovery codes: 16 base32 characters (80 bits) shown as XXXX-XXXX-XXXX-XXXX.
// Only sha256 digests are stored; each code can be used once.

// RecoveryCodeCount is how many recovery codes are generated at once.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns n fresh codes and their digests.
func NewRecoveryCodes(n int) (codes, hashes []string) {
	for i := 0; i < n; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		raw := b32.EncodeToString(b) // 16 chars
		code := raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
		codes = append(codes, code)
		hashes = append(hashes, HashRecoveryCode(code))
	}
	return codes, hashes
}

// NormalizeRecoveryCode upper-cases and strips separators / spaces.
func NormalizeRecoveryCode(code string) string {
	r := strings.NewReplacer("-", "", " ", "", "\t", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(code)))
}

// HashRecoveryCode returns the stored digest of a recovery code.
func HashRecoveryCode(code string) string {
	return HashSecret("xsm-recovery:" + NormalizeRecoveryCode(code))
}

// looksLikeRecoveryCode distinguishes a recovery code from a 6-digit TOTP.
func looksLikeRecoveryCode(code string) bool {
	return len(NormalizeRecoveryCode(code)) == 16
}
