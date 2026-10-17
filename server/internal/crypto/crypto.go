// Package crypto provides AES-256-GCM envelope helpers for secrets stored in
// the database (kubeconfigs, host passwords, API keys, ...).
//
// Ciphertext format: "enc:v1:" + base64(nonce || sealed). The prefix lets
// callers detect already-encrypted values and allows future key rotation.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const prefixV1 = "enc:v1:"

// ErrNotEncrypted is returned by Decrypt when the input lacks the envelope prefix.
var ErrNotEncrypted = errors.New("crypto: value is not an encrypted envelope")

// Cipher encrypts and decrypts secrets with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// EncryptBytes seals plaintext; aad (may be nil) binds the ciphertext to a
// context such as "cluster:42:kubeconfig" so it cannot be swapped between rows.
func (c *Cipher) EncryptBytes(plaintext, aad []byte) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, aad)
	return prefixV1 + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptBytes opens a value produced by EncryptBytes with the same aad.
func (c *Cipher) DecryptBytes(envelope string, aad []byte) ([]byte, error) {
	if !strings.HasPrefix(envelope, prefixV1) {
		return nil, ErrNotEncrypted
	}
	raw, err := base64.StdEncoding.DecodeString(envelope[len(prefixV1):])
	if err != nil {
		return nil, fmt.Errorf("crypto: bad envelope encoding: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns+c.aead.Overhead() {
		return nil, errors.New("crypto: envelope too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], aad)
	if err != nil {
		return nil, errors.New("crypto: decryption failed (wrong key or tampered data)")
	}
	return pt, nil
}

// Encrypt is EncryptBytes for strings without aad.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	return c.EncryptBytes([]byte(plaintext), nil)
}

// Decrypt is DecryptBytes for strings without aad.
func (c *Cipher) Decrypt(envelope string) (string, error) {
	b, err := c.DecryptBytes(envelope, nil)
	return string(b), err
}

// IsEncrypted reports whether s looks like an envelope produced by this package.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefixV1) }

// Mask returns a display-safe representation of a secret ("sk-a****xyz9").
func Mask(secret string) string {
	r := []rune(secret)
	switch {
	case len(r) == 0:
		return ""
	case len(r) <= 8:
		return "********"
	default:
		return string(r[:4]) + "****" + string(r[len(r)-4:])
	}
}
