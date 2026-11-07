package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	oldSecret = strings.Repeat("o", 40)
	newSecret = strings.Repeat("n", 40)
)

func TestJWTKeyRotation(t *testing.T) {
	before := NewTokenManager(oldSecret, "matrix", 15*time.Minute)
	oldTok, _, err := before.IssueAccess(7, "alice", "sid1")
	if err != nil {
		t.Fatal(err)
	}
	// 轮换：新密钥签发，旧密钥只验签
	after := NewTokenManagerWithKeys(newSecret, []string{oldSecret, "", newSecret, oldSecret}, "matrix", 15*time.Minute)
	if after.KeyCount() != 2 {
		t.Fatalf("duplicates/empties must be ignored, got %d keys", after.KeyCount())
	}
	if after.CurrentKeyID() != KeyID(newSecret) || after.CurrentKeyID() == KeyID(oldSecret) {
		t.Fatal("current kid should derive from the new secret")
	}
	c, err := after.ParseAccess(oldTok)
	if err != nil || c.UserID() != 7 || c.SessionID != "sid1" {
		t.Fatalf("token signed with previous key must verify during grace period: %v", err)
	}
	newTok, _, _ := after.IssueAccess(8, "bob", "sid2")
	if _, err := after.ParseAccess(newTok); err != nil {
		t.Fatal(err)
	}
	// 未配置新密钥的旧副本不接受新令牌
	if _, err := before.ParseAccess(newTok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("old replica accepted unknown kid: %v", err)
	}
	// 宽限期结束（旧密钥移除）后旧令牌失效
	final := NewTokenManager(newSecret, "matrix", 15*time.Minute)
	if _, err := final.ParseAccess(oldTok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("old token must fail after previous key removed: %v", err)
	}
	if _, err := final.ParseAccess(newTok); err != nil {
		t.Fatal(err)
	}
}

func TestJWTExpiryAndIssuer(t *testing.T) {
	m := NewTokenManager(newSecret, "matrix", time.Minute)
	base := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return base }
	tok, _, _ := m.IssueAccess(1, "a", "s")
	m.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := m.ParseAccess(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want expired, got %v", err)
	}
	other := NewTokenManager(newSecret, "other", time.Minute)
	other.now = func() time.Time { return base }
	m.now = func() time.Time { return base }
	if _, err := other.ParseAccess(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("issuer mismatch accepted: %v", err)
	}
}

func TestAPITokenFormat(t *testing.T) {
	tok, hash := NewAPITokenSecret()
	if !IsAPIToken(tok) || !strings.HasPrefix(tok, APITokenPrefix) {
		t.Fatalf("bad token %s", tok)
	}
	if HashSecret(tok) != hash || strings.Contains(hash, tok) {
		t.Fatal("hash mismatch")
	}
	if IsAPIToken("xsm_short") || IsAPIToken("eyJhbGciOi.x.y") {
		t.Fatal("non-token recognised")
	}
}
