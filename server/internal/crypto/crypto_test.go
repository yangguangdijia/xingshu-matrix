package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// legacyV1 reproduces the pre-v1.29 envelope format.
func legacyV1(t *testing.T, k, pt, aad []byte) string {
	t.Helper()
	block, _ := aes.NewCipher(k)
	a, _ := cipher.NewGCM(block)
	nonce := make([]byte, a.NonceSize())
	_, _ = rand.Read(nonce)
	return prefixV1 + base64.StdEncoding.EncodeToString(a.Seal(nonce, nonce, pt, aad))
}

func TestEncryptV2RoundTrip(t *testing.T) {
	c, err := New(key(1))
	if err != nil {
		t.Fatal(err)
	}
	env, err := c.EncryptBytes([]byte("hello"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env, "enc:v2:"+KeyFingerprint(key(1))+":") {
		t.Fatalf("unexpected envelope %q", env)
	}
	if len(c.CurrentKID()) != 8 || KeyID(env) != c.CurrentKID() {
		t.Fatalf("kid mismatch %q %q", KeyID(env), c.CurrentKID())
	}
	pt, err := c.DecryptBytes(env, []byte("ctx"))
	if err != nil || string(pt) != "hello" {
		t.Fatalf("decrypt: %v %q", err, pt)
	}
	if _, err := c.DecryptBytes(env, []byte("other")); err == nil {
		t.Fatal("aad mismatch must fail")
	}
	if !IsEncrypted(env) {
		t.Fatal("IsEncrypted false for v2")
	}
}

func TestLegacyV1Compat(t *testing.T) {
	old := legacyV1(t, key(1), []byte("secret"), nil)
	c, _ := New(key(1))
	if s, err := c.Decrypt(old); err != nil || s != "secret" {
		t.Fatalf("v1 with current key: %v %q", err, s)
	}
	rotated, _ := NewKeyring(key(2), key(1))
	if s, err := rotated.Decrypt(old); err != nil || s != "secret" {
		t.Fatalf("v1 with previous key: %v %q", err, s)
	}
	if KeyID(old) != KIDLegacy || !IsEncrypted(old) {
		t.Fatal("v1 detection")
	}
	other, _ := New(key(3))
	if _, err := other.Decrypt(old); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestRotationAndRewrap(t *testing.T) {
	c1, _ := New(key(1))
	env1, _ := c1.EncryptBytes([]byte("pw"), []byte("a"))
	c2, err := NewKeyring(key(2), key(1), key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.KIDs(); len(got) != 2 || got[0] != KeyFingerprint(key(2)) {
		t.Fatalf("kids %v", got)
	}
	if pt, err := c2.DecryptBytes(env1, []byte("a")); err != nil || string(pt) != "pw" {
		t.Fatalf("decrypt with previous: %v", err)
	}
	env2, changed, err := c2.Rewrap(env1, []byte("a"))
	if err != nil || !changed || KeyID(env2) != c2.CurrentKID() {
		t.Fatalf("rewrap: %v %v %q", err, changed, env2)
	}
	same, changed, err := c2.Rewrap(env2, []byte("a"))
	if err != nil || changed || same != env2 {
		t.Fatal("rewrap must be idempotent")
	}
	// After the old key is dropped, re-wrapped values still decrypt; old ones do not.
	c3, _ := New(key(2))
	if _, err := c3.DecryptBytes(env2, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := c3.DecryptBytes(env1, []byte("a")); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

func TestBadInputs(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewKeyring(key(1), make([]byte, 3)); err == nil {
		t.Fatal("short previous key accepted")
	}
	c, _ := New(key(1))
	for _, in := range []string{"plain", "enc:v2:", "enc:v2:abc", "enc:v2:abcd1234:!!", "enc:v1:!!"} {
		if _, err := c.Decrypt(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
	if _, err := c.Decrypt("plain"); !errors.Is(err, ErrNotEncrypted) {
		t.Fatal("want ErrNotEncrypted")
	}
	if KeyID("plain") != "" {
		t.Fatal("KeyID of plain")
	}
	if Mask("abcdefghijk") != "abcd****hijk" || Mask("short") != "********" || Mask("") != "" {
		t.Fatal("mask")
	}
}
