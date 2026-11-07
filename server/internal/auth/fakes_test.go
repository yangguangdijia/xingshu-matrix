package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// ---- fake Repo ----

type fakeRepo struct {
	mu       sync.Mutex
	users    map[int64]*User
	sessions map[string]*Session
	failures map[int64]int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{users: map[int64]*User{}, sessions: map[string]*Session{}, failures: map[int64]int{}}
}

func (r *fakeRepo) add(u *User) *User {
	if u.Status == "" {
		u.Status = "active"
	}
	if u.AccountType == "" {
		u.AccountType = "human"
	}
	r.users[u.ID] = u
	return u
}

func (r *fakeRepo) UserByUsername(_ context.Context, name string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, u := range r.users {
		if strings.EqualFold(u.Username, name) {
			c := *u
			return &c, nil
		}
	}
	return nil, httpx.ErrNotFound
}

func (r *fakeRepo) UserByID(_ context.Context, id int64) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.users[id]; ok {
		c := *u
		return &c, nil
	}
	return nil, httpx.ErrNotFound
}

func (r *fakeRepo) RecordLoginFailure(_ context.Context, id int64, max int, until time.Time) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[id]++
	if r.failures[id] >= max {
		r.failures[id] = 0
		t := until
		r.users[id].LockedUntil = &t
		return max, true, nil
	}
	return r.failures[id], false, nil
}

func (r *fakeRepo) RecordLoginSuccess(_ context.Context, id int64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[id] = 0
	r.users[id].LockedUntil = nil
	return nil
}

func (r *fakeRepo) UpdatePassword(_ context.Context, id int64, hash string, mc bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[id].PasswordHash, r.users[id].MustChangePassword = hash, mc
	return nil
}

func (r *fakeRepo) CreateSession(_ context.Context, s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *s
	r.sessions[s.ID] = &c
	return nil
}

func (r *fakeRepo) SessionByID(_ context.Context, id string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		c := *s
		return &c, nil
	}
	return nil, httpx.ErrNotFound
}

func (r *fakeRepo) RotateSession(_ context.Context, id, oldHash, newHash string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.RefreshHash != oldHash {
		return false, nil
	}
	s.RefreshHash = newHash
	return true, nil
}

func (r *fakeRepo) RevokeSession(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		n := time.Now()
		s.RevokedAt = &n
	}
	return nil
}

func (r *fakeRepo) RevokeUserSessions(_ context.Context, uid int64, except string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		if s.UserID == uid && id != except {
			n := time.Now()
			s.RevokedAt = &n
		}
	}
	return nil
}

func (r *fakeRepo) ListUserSessions(context.Context, int64) ([]Session, error) { return nil, nil }
func (r *fakeRepo) CountUsers(context.Context) (int64, error)                { return int64(len(r.users)), nil }
func (r *fakeRepo) CreateBuiltinAdmin(context.Context, string) error         { return nil }

// ---- fake AccessResolver ----

type fakeAccess struct {
	roles map[int64][]string
	perms map[int64][]string
}

func (a *fakeAccess) UserAccess(_ context.Context, id int64) ([]string, []string, []string, error) {
	return a.roles[id], a.perms[id], nil, nil
}

// ---- fake cipher ----

type fakeCipher struct{}

func (fakeCipher) EncryptBytes(p, aad []byte) (string, error) {
	return "v1:" + base64.StdEncoding.EncodeToString(aad) + ":" + base64.StdEncoding.EncodeToString(p), nil
}

func (fakeCipher) DecryptBytes(env string, aad []byte) ([]byte, error) {
	parts := strings.Split(env, ":")
	if len(parts) != 3 || parts[1] != base64.StdEncoding.EncodeToString(aad) {
		return nil, errors.New("aad mismatch")
	}
	return base64.StdEncoding.DecodeString(parts[2])
}

// ---- fake IdentityStore ----

type fakeIdentity struct {
	mu         sync.Mutex
	tokens     map[int64]*APIToken
	nextID     int64
	mfa        map[int64]*MFARecord
	challenges map[string]*MFAChallenge
	policy     MFAPolicy
	touched    int
}

func newFakeIdentity() *fakeIdentity {
	return &fakeIdentity{tokens: map[int64]*APIToken{}, mfa: map[int64]*MFARecord{}, challenges: map[string]*MFAChallenge{}}
}

func (f *fakeIdentity) CreateAPIToken(_ context.Context, t *APIToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	t.ID, t.CreatedAt = f.nextID, time.Now()
	c := *t
	f.tokens[t.ID] = &c
	return nil
}

func (f *fakeIdentity) APITokenByHash(_ context.Context, h string) (*APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.Hash == h {
			c := *t
			return &c, nil
		}
	}
	return nil, httpx.ErrNotFound
}

func (f *fakeIdentity) ListAPITokens(_ context.Context, uid int64) ([]APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []APIToken
	for _, t := range f.tokens {
		if t.UserID == uid {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (f *fakeIdentity) RevokeAPIToken(_ context.Context, uid, id, by int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	if !ok || t.UserID != uid || t.RevokedAt != nil {
		return httpx.NotFound("令牌不存在或已吊销")
	}
	n := time.Now()
	t.RevokedAt = &n
	return nil
}

func (f *fakeIdentity) TouchAPIToken(context.Context, int64, string) error {
	f.mu.Lock()
	f.touched++
	f.mu.Unlock()
	return nil
}

func (f *fakeIdentity) GetMFA(_ context.Context, uid int64) (*MFARecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mfa[uid]
	if !ok {
		return nil, httpx.ErrNotFound
	}
	c := *m
	c.RecoveryHashes = append([]string(nil), m.RecoveryHashes...)
	return &c, nil
}

func (f *fakeIdentity) SavePendingMFA(_ context.Context, uid int64, enc string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mfa[uid]; ok && m.Enabled {
		return false, nil
	}
	f.mfa[uid] = &MFARecord{UserID: uid, SecretEnc: enc}
	return true, nil
}

func (f *fakeIdentity) ActivateMFA(_ context.Context, uid int64, enc string, step int64, hashes []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mfa[uid]; ok && m.Enabled {
		return false, nil
	}
	n := time.Now()
	f.mfa[uid] = &MFARecord{UserID: uid, SecretEnc: enc, Enabled: true, LastStep: step, RecoveryHashes: hashes, EnabledAt: &n}
	return true, nil
}

func (f *fakeIdentity) AdvanceMFAStep(_ context.Context, uid, step int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mfa[uid]
	if !ok || !m.Enabled || m.LastStep >= step {
		return false, nil
	}
	m.LastStep = step
	return true, nil
}

func (f *fakeIdentity) ConsumeRecoveryCode(_ context.Context, uid int64, h string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mfa[uid]
	if !ok || !m.Enabled {
		return false, nil
	}
	for i, x := range m.RecoveryHashes {
		if x == h {
			m.RecoveryHashes = append(m.RecoveryHashes[:i:i], m.RecoveryHashes[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeIdentity) SetRecoveryCodes(_ context.Context, uid int64, hs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mfa[uid]
	if !ok || !m.Enabled {
		return httpx.ErrNotFound
	}
	m.RecoveryHashes = hs
	return nil
}

func (f *fakeIdentity) DeleteMFA(_ context.Context, uid int64) error {
	f.mu.Lock()
	delete(f.mfa, uid)
	f.mu.Unlock()
	return nil
}

func (f *fakeIdentity) CreateChallenge(_ context.Context, ch *MFAChallenge) error {
	f.mu.Lock()
	c := *ch
	f.challenges[ch.Hash] = &c
	f.mu.Unlock()
	return nil
}

func (f *fakeIdentity) ChallengeByHash(_ context.Context, h string) (*MFAChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.challenges[h]
	if !ok {
		return nil, httpx.ErrNotFound
	}
	c := *ch
	return &c, nil
}

func (f *fakeIdentity) BumpChallenge(_ context.Context, h string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.challenges[h]
	if !ok {
		return 0, httpx.ErrNotFound
	}
	ch.Attempts++
	return ch.Attempts, nil
}

func (f *fakeIdentity) DeleteChallenge(_ context.Context, h string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.challenges[h]
	delete(f.challenges, h)
	return ok, nil
}

func (f *fakeIdentity) LoadMFAPolicy(context.Context) (MFAPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}

func (f *fakeIdentity) SaveMFAPolicy(_ context.Context, p MFAPolicy, _ int64) error {
	f.mu.Lock()
	f.policy = p
	f.mu.Unlock()
	return nil
}
