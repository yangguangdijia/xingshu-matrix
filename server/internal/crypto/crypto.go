// Package crypto provides AES-256-GCM envelope helpers for secrets stored in
// the database (kubeconfigs, host passwords, API keys, ...).
//
// Ciphertext formats:
//
//	enc:v1:<base64(nonce || sealed)>          legacy, no key id (read only)
//	enc:v2:<kid>:<base64(nonce || sealed)>    current; kid = first 8 hex chars of sha256(key)
//
// A Cipher holds a key ring: one current key (used for every new encryption)
// and any number of previous keys (used only to decrypt). Rotating the master
// key therefore means: set the new key as current, move the old one to the
// previous list, then run the background re-wrap job so every stored envelope
// is re-encrypted with the current key, and finally drop the old key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	prefixV1 = "enc:v1:"
	prefixV2 = "enc:v2:"
	// KIDLegacy is reported by KeyID for enc:v1: envelopes, which carry no key id.
	KIDLegacy = "v1"
)

// ErrNotEncrypted is returned by Decrypt when the input lacks the envelope prefix.
var ErrNotEncrypted = errors.New("crypto: value is not an encrypted envelope")

// ErrUnknownKey is returned when an enc:v2 envelope names a key id that is not in the ring.
var ErrUnknownKey = errors.New("crypto: envelope was encrypted with a key that is not configured")

// Encrypter is the surface every secret store depends on. *Cipher implements
// it with local AES keys; a KMS/Vault-backed implementation can be dropped in
// later without touching callers.
type Encrypter interface {
	EncryptBytes(plaintext, aad []byte) (string, error)
	DecryptBytes(envelope string, aad []byte) ([]byte, error)
	CurrentKID() string
}

// KeyWrapper is reserved for an external key service (cloud KMS, Vault
// transit). It would wrap per-record data keys; no implementation ships yet.
type KeyWrapper interface {
	KeyID() string
	Wrap(plaintextKey []byte) ([]byte, error)
	Unwrap(wrappedKey []byte) ([]byte, error)
}

var _ Encrypter = (*Cipher)(nil)

type ringKey struct {
	kid  string
	aead cipher.AEAD
}

// Cipher encrypts and decrypts secrets with AES-256-GCM over a key ring.
type Cipher struct {
	current ringKey
	byKID   map[string]cipher.AEAD
	order   []ringKey // current first, then previous keys in configured order
}

// KeyFingerprint returns the key id used in enc:v2 envelopes for a raw key.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// New builds a Cipher from a single 32-byte key (no previous keys).
func New(key []byte) (*Cipher, error) { return NewKeyring(key) }

// NewKeyring builds a Cipher whose current key is current; previous keys are
// only used to decrypt. Duplicate keys are ignored.
func NewKeyring(current []byte, previous ...[]byte) (*Cipher, error) {
	a, err := newAEAD(current)
	if err != nil {
		return nil, err
	}
	c := &Cipher{byKID: map[string]cipher.AEAD{}}
	c.current = ringKey{kid: KeyFingerprint(current), aead: a}
	c.byKID[c.current.kid] = a
	c.order = append(c.order, c.current)
	for i, k := range previous {
		pa, err := newAEAD(k)
		if err != nil {
			return nil, fmt.Errorf("crypto: previous key #%d: %w", i+1, err)
		}
		kid := KeyFingerprint(k)
		if _, dup := c.byKID[kid]; dup {
			continue
		}
		c.byKID[kid] = pa
		c.order = append(c.order, ringKey{kid: kid, aead: pa})
	}
	return c, nil
}

// CurrentKID is the key id new envelopes are written with.
func (c *Cipher) CurrentKID() string { return c.current.kid }

// KIDs lists every key id in the ring, current first.
func (c *Cipher) KIDs() []string {
	out := make([]string, 0, len(c.order))
	for _, k := range c.order {
		out = append(out, k.kid)
	}
	return out
}

// EncryptBytes seals plaintext with the current key; aad (may be nil) binds the
// ciphertext to a context such as "cluster:42:kubeconfig" so it cannot be
// swapped between rows.
func (c *Cipher) EncryptBytes(plaintext, aad []byte) (string, error) {
	a := c.current.aead
	nonce := make([]byte, a.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := a.Seal(nonce, nonce, plaintext, aad)
	return prefixV2 + c.current.kid + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

func open(a cipher.AEAD, raw, aad []byte) ([]byte, error) {
	ns := a.NonceSize()
	if len(raw) < ns+a.Overhead() {
		return nil, errors.New("crypto: envelope too short")
	}
	return a.Open(nil, raw[:ns], raw[ns:], aad)
}

var errDecrypt = errors.New("crypto: decryption failed (wrong key or tampered data)")

// DecryptBytes opens a value produced by EncryptBytes (any key in the ring, or
// a legacy enc:v1 envelope) with the same aad.
func (c *Cipher) DecryptBytes(envelope string, aad []byte) ([]byte, error) {
	switch {
	case strings.HasPrefix(envelope, prefixV2):
		rest := envelope[len(prefixV2):]
		kid, body, ok := strings.Cut(rest, ":")
		if !ok || kid == "" {
			return nil, errors.New("crypto: bad envelope (missing key id)")
		}
		a, found := c.byKID[kid]
		if !found {
			return nil, fmt.Errorf("%w (kid %s)", ErrUnknownKey, kid)
		}
		raw, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("crypto: bad envelope encoding: %w", err)
		}
		pt, err := open(a, raw, aad)
		if err != nil {
			return nil, errDecrypt
		}
		return pt, nil
	case strings.HasPrefix(envelope, prefixV1):
		raw, err := base64.StdEncoding.DecodeString(envelope[len(prefixV1):])
		if err != nil {
			return nil, fmt.Errorf("crypto: bad envelope encoding: %w", err)
		}
		// v1 carries no key id: try the current key, then the previous ones.
		for _, k := range c.order {
			if pt, err := open(k.aead, raw, aad); err == nil {
				return pt, nil
			}
		}
		return nil, errDecrypt
	default:
		return nil, ErrNotEncrypted
	}
}

// Rewrap re-encrypts envelope with the current key. It reports changed=false
// (and returns the input) when the envelope is already enc:v2 under the
// current key, which makes re-wrap jobs idempotent.
func (c *Cipher) Rewrap(envelope string, aad []byte) (out string, changed bool, err error) {
	if KeyID(envelope) == c.current.kid {
		return envelope, false, nil
	}
	pt, err := c.DecryptBytes(envelope, aad)
	if err != nil {
		return "", false, err
	}
	out, err = c.EncryptBytes(pt, aad)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
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
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, prefixV1) || strings.HasPrefix(s, prefixV2)
}

// KeyID returns the key id of an envelope: the kid of enc:v2 values, KIDLegacy
// for enc:v1 values and "" for anything that is not an envelope.
func KeyID(envelope string) string {
	switch {
	case strings.HasPrefix(envelope, prefixV2):
		kid, _, ok := strings.Cut(envelope[len(prefixV2):], ":")
		if !ok {
			return ""
		}
		return kid
	case strings.HasPrefix(envelope, prefixV1):
		return KIDLegacy
	}
	return ""
}

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
