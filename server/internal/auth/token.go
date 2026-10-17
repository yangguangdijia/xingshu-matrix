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

// TokenManager issues and verifies HS256 access tokens.
type TokenManager struct {
	secret    []byte
	issuer    string
	accessTTL time.Duration
	now       func() time.Time
}

// NewTokenManager builds a TokenManager.
func NewTokenManager(secret, issuer string, accessTTL time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), issuer: issuer, accessTTL: accessTTL, now: time.Now}
}

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
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return s, exp, err
}

// ParseAccess verifies signature, issuer, expiry and type.
func (m *TokenManager) ParseAccess(token string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) { return m.secret, nil },
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
