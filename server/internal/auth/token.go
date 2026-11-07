package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the JWT claims of an access token.
type Claims struct {
	SessionID string `json:"sid"`
	Username  string `json:"name"`
	Type      string `json:"typ"`
	jwt.RegisteredClaims
}

// UserID returns the numeric subject.
func (c *Claims) UserID() int64 {
	id, _ := strconv.ParseInt(c.Subject, 10, 64)
	return id
}

var (
	ErrTokenExpired = errors.New("token expired")
	ErrTokenInvalid = errors.New("token invalid")
)

// TokenManager issues and verifies HS256 access tokens. It holds a key ring:
// the first key signs new tokens (its id is written to the "kid" header) and
// every key verifies, so tokens signed before a secret rotation keep working
// until they expire (MATRIX_JWT_PREVIOUS_SECRETS).
type TokenManager struct {
	keys      []jwtKey // keys[0] is the current signing key
	issuer    string
	accessTTL time.Duration
	now       func() time.Time
}

type jwtKey struct {
	kid    string
	secret []byte
}

// NewTokenManager builds a TokenManager with a single key.
func NewTokenManager(secret, issuer string, accessTTL time.Duration) *TokenManager {
	return NewTokenManagerWithKeys(secret, nil, issuer, accessTTL)
}

// NewTokenManagerWithKeys builds a TokenManager signing with current and still
// accepting tokens signed with any of previous (empty / duplicate entries are ignored).
func NewTokenManagerWithKeys(current string, previous []string, issuer string, accessTTL time.Duration) *TokenManager {
	m := &TokenManager{issuer: issuer, accessTTL: accessTTL, now: time.Now}
	m.keys = append(m.keys, jwtKey{kid: KeyID(current), secret: []byte(current)})
	for _, p := range previous {
		p = strings.TrimSpace(p)
		if p == "" || p == current {
			continue
		}
		dup := false
		for _, k := range m.keys {
			if string(k.secret) == p {
				dup = true
			}
		}
		if !dup {
			m.keys = append(m.keys, jwtKey{kid: KeyID(p), secret: []byte(p)})
		}
	}
	return m
}

// KeyID derives a non-reversible key id (16 hex chars) from a secret.
func KeyID(secret string) string {
	h := sha256.Sum256([]byte("xingshu-matrix/jwt-kid/" + secret))
	return hex.EncodeToString(h[:8])
}

// CurrentKeyID returns the kid written into newly issued tokens.
func (m *TokenManager) CurrentKeyID() string { return m.keys[0].kid }

// KeyCount returns how many keys verify tokens (current + previous).
func (m *TokenManager) KeyCount() int { return len(m.keys) }

// IssueAccess returns a signed access token and its expiry.
func (m *TokenManager) IssueAccess(userID int64, username, sessionID string) (string, time.Time, error) {
	now := m.now()
	exp := now.Add(m.accessTTL)
	claims := Claims{
		SessionID: sessionID,
		Username:  username,
		Type:      "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        randomToken(12),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	t.Header["kid"] = m.keys[0].kid
	s, err := t.SignedString(m.keys[0].secret)
	return s, exp, err
}

func (m *TokenManager) keyFunc(t *jwt.Token) (any, error) {
	if kid, ok := t.Header["kid"].(string); ok && kid != "" {
		for _, k := range m.keys {
			if k.kid == kid {
				return k.secret, nil
			}
		}
		return nil, ErrTokenInvalid
	}
	// Tokens issued before key ids existed: try every key.
	set := jwt.VerificationKeySet{}
	for _, k := range m.keys {
		set.Keys = append(set.Keys, k.secret)
	}
	return set, nil
}

// ParseAccess verifies signature, issuer, expiry and type.
func (m *TokenManager) ParseAccess(token string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims, m.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	if claims.Type != "access" || claims.SessionID == "" || claims.UserID() <= 0 {
		return nil, ErrTokenInvalid
	}
	return &claims, nil
}

// Refresh tokens are opaque: "<sessionID>.<secret>". Only sha256(secret) is stored.

// NewRefreshToken returns a token for sessionID and the hash to persist.
func NewRefreshToken(sessionID string) (token, hash string) {
	secret := randomToken(32)
	return sessionID + "." + secret, HashSecret(secret)
}

// SplitRefreshToken returns (sessionID, secret) or ok=false.
func SplitRefreshToken(tok string) (sid, secret string, ok bool) {
	sid, secret, ok = strings.Cut(tok, ".")
	if !ok || sid == "" || len(secret) < 20 || len(sid) > 64 {
		return "", "", false
	}
	return sid, secret, true
}

// HashSecret returns hex(sha256(s)).
func HashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// EqualHash compares two hashes in constant time.
func EqualHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// NewSessionID returns a random session id.
func NewSessionID() string { return randomToken(18) }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
