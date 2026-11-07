package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 测试向量（密钥 "12345678901234567890"）。
// 本实现输出 6 位，等于 8 位参考值的后 6 位（同一截断值对 10^6 取模）。
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},          // 94287082
		{1111111109, "081804"},  // 07081804
		{1111111111, "050471"},  // 14050471
		{1234567890, "005924"},  // 89005924
		{2000000000, "279037"},  // 69279037
		{20000000000, "353130"}, // 65353130
	}
	for _, c := range cases {
		got, err := TOTPCode(secret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("T=%d: got %s want %s", c.unix, got, c.want)
		}
		if step, ok := VerifyTOTP(secret, c.want, time.Unix(c.unix, 0), 0); !ok || step != c.unix/30 {
			t.Errorf("T=%d: verify failed (step=%d ok=%v)", c.unix, step, ok)
		}
	}
	// 小写 / 带空格 / 带填充的密钥也能解析
	lower := strings.ToLower(secret[:8]) + " " + secret[8:] + "===="
	if got, _ := TOTPCode(lower, time.Unix(59, 0)); got != "287082" {
		t.Fatalf("lenient secret parse: %s", got)
	}
}

func TestVerifyTOTPSkewAndReplay(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	prev, _ := TOTPCode(secret, now.Add(-30*time.Second))
	next, _ := TOTPCode(secret, now.Add(30*time.Second))
	far, _ := TOTPCode(secret, now.Add(-90*time.Second))
	cur, _ := TOTPCode(secret, now)

	if _, ok := VerifyTOTP(secret, prev, now, 0); !ok {
		t.Error("previous step should be accepted (clock drift)")
	}
	if _, ok := VerifyTOTP(secret, next, now, 0); !ok {
		t.Error("next step should be accepted (clock drift)")
	}
	if far != cur && far != prev && far != next {
		if _, ok := VerifyTOTP(secret, far, now, 0); ok {
			t.Error("code 3 steps old must be rejected")
		}
	}
	step, ok := VerifyTOTP(secret, cur, now, 0)
	if !ok || step != totpStep(now) {
		t.Fatalf("current code rejected: %d %v", step, ok)
	}
	// 防重放：同一时间窗（及更早）已用过后不再接受
	if _, ok := VerifyTOTP(secret, cur, now, step); ok {
		t.Error("replayed code accepted")
	}
	if _, ok := VerifyTOTP(secret, prev, now, step); ok {
		t.Error("older step accepted after a newer one was used")
	}
	if _, ok := VerifyTOTP(secret, next, now, step); !ok {
		t.Error("newer step should still be accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "12 456"} {
		if _, ok := VerifyTOTP(secret, bad, now, 0); ok {
			t.Errorf("malformed code %q accepted", bad)
		}
	}
	if _, ok := VerifyTOTP("!!!", cur, now, 0); ok {
		t.Error("invalid secret accepted")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("星枢 Matrix", "alice", "ABCDEF")
	if !strings.HasPrefix(u, "otpauth://totp/") || !strings.Contains(u, "secret=ABCDEF") ||
		!strings.Contains(u, "digits=6") || !strings.Contains(u, "period=30") || !strings.Contains(u, "alice") {
		t.Fatalf("unexpected uri %s", u)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes(RecoveryCodeCount)
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 19 || strings.Count(c, "-") != 3 {
			t.Errorf("bad format %q", c)
		}
		if !looksLikeRecoveryCode(c) {
			t.Errorf("%q not recognised as recovery code", c)
		}
		if HashRecoveryCode(c) != hashes[i] {
			t.Error("hash mismatch")
		}
		// 大小写、分隔符、空格不影响
		alt := strings.ToLower(strings.ReplaceAll(c, "-", " "))
		if HashRecoveryCode(alt) != hashes[i] {
			t.Errorf("normalisation failed for %q", alt)
		}
		if seen[c] {
			t.Error("duplicate recovery code")
		}
		seen[c] = true
		if strings.Contains(hashes[i], c) {
			t.Error("hash leaks code")
		}
	}
	if looksLikeRecoveryCode("123456") {
		t.Error("TOTP code treated as recovery code")
	}
}
