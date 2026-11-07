package auth

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// APIToken is a personal access token or a service-account token. Only the
// sha256 digest of the token is stored; Prefix ("xsm_" + 8 chars) identifies it.
type APIToken struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"-"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP string     `json:"last_used_ip"`
	CreatedBy  *int64     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	// Status is derived: active | expired | revoked.
	Status string `json:"status"`
}

func (t *APIToken) derive(now time.Time) {
	switch {
	case t.RevokedAt != nil:
		t.Status = "revoked"
	case t.ExpiresAt != nil && !t.ExpiresAt.After(now):
		t.Status = "expired"
	default:
		t.Status = "active"
	}
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
}

// MFARecord is a user's TOTP enrolment.
type MFARecord struct {
	UserID         int64
	SecretEnc      string
	Enabled        bool
	LastStep       int64
	RecoveryHashes []string
	EnabledAt      *time.Time
}

// MFAChallenge is the short-lived ticket between a correct password and the
// second factor. Purpose "verify" = enrolled user; "enroll" = MFA is enforced
// for the user's role but not set up yet (PendingSecret holds the new secret).
type MFAChallenge struct {
	Hash          string
	UserID        int64
	Purpose       string
	PendingSecret string // encrypted
	Attempts      int
	IP            string
	UserAgent     string
	ExpiresAt     time.Time
}

// MFAPolicy is stored under settings key "security.mfa".
type MFAPolicy struct {
	// RequiredRoles lists role codes whose members must use MFA (password and LDAP logins).
	RequiredRoles []string `json:"required_roles"`
}

// IdentityStore persists API tokens, MFA enrolments and challenges.
type IdentityStore interface {
	CreateAPIToken(ctx context.Context, t *APIToken) error
	APITokenByHash(ctx context.Context, hash string) (*APIToken, error) // httpx.ErrNotFound when missing
	ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error)
	RevokeAPIToken(ctx context.Context, userID, id, by int64) error // httpx.ErrNotFound when not the user's / already revoked
	TouchAPIToken(ctx context.Context, id int64, ip string) error

	GetMFA(ctx context.Context, userID int64) (*MFARecord, error) // httpx.ErrNotFound when not enrolled
	// SavePendingMFA stores a not-yet-enabled secret; it never overwrites an enabled enrolment.
	SavePendingMFA(ctx context.Context, userID int64, secretEnc string) (bool, error)
	// ActivateMFA enables MFA with secretEnc unless already enabled.
	ActivateMFA(ctx context.Context, userID int64, secretEnc string, step int64, recoveryHashes []string) (bool, error)
	// AdvanceMFAStep records step as used if it is newer than the last used one (replay protection).
	AdvanceMFAStep(ctx context.Context, userID, step int64) (bool, error)
	// ConsumeRecoveryCode removes hash from the user's codes; false when absent.
	ConsumeRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error)
	SetRecoveryCodes(ctx context.Context, userID int64, hashes []string) error
	DeleteMFA(ctx context.Context, userID int64) error

	CreateChallenge(ctx context.Context, ch *MFAChallenge) error
	ChallengeByHash(ctx context.Context, hash string) (*MFAChallenge, error)
	// BumpChallenge increments the attempt counter and returns the new value.
	BumpChallenge(ctx context.Context, hash string) (int, error)
	// DeleteChallenge consumes the challenge; false when it was already consumed.
	DeleteChallenge(ctx context.Context, hash string) (bool, error)

	LoadMFAPolicy(ctx context.Context) (MFAPolicy, error)
	SaveMFAPolicy(ctx context.Context, p MFAPolicy, by int64) error
}

// PgIdentityStore implements IdentityStore on PostgreSQL.
type PgIdentityStore struct{ DB store.DB }

const apiTokenCols = `id, user_id, name, prefix, token_hash, scopes, expires_at, last_used_at, last_used_ip, created_by, created_at, revoked_at`

func scanAPIToken(row pgx.Row) (*APIToken, error) {
	var t APIToken
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &t.Scopes, &t.ExpiresAt, &t.LastUsedAt, &t.LastUsedIP, &t.CreatedBy, &t.CreatedAt, &t.RevokedAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &t, err
}

func (r *PgIdentityStore) CreateAPIToken(ctx context.Context, t *APIToken) error {
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	return r.DB.QueryRow(ctx, `INSERT INTO api_tokens(user_id, name, prefix, token_hash, scopes, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
		t.UserID, t.Name, t.Prefix, t.Hash, t.Scopes, t.ExpiresAt, t.CreatedBy).Scan(&t.ID, &t.CreatedAt)
}

func (r *PgIdentityStore) APITokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	return scanAPIToken(r.DB.QueryRow(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE token_hash=$1`, hash))
}

func (r *PgIdentityStore) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := r.DB.Query(ctx, `SELECT `+apiTokenCols+` FROM api_tokens WHERE user_id=$1
		AND (revoked_at IS NULL OR revoked_at > now() - interval '30 days') ORDER BY created_at DESC LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *PgIdentityStore) RevokeAPIToken(ctx context.Context, userID, id, by int64) error {
	tag, err := r.DB.Exec(ctx, `UPDATE api_tokens SET revoked_at=now(), revoked_by=$3 WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, userID, by)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.NotFound("令牌不存在或已吊销")
	}
	return err
}

func (r *PgIdentityStore) TouchAPIToken(ctx context.Context, id int64, ip string) error {
	_, err := r.DB.Exec(ctx, `UPDATE api_tokens SET last_used_at=now(), last_used_ip=$2
		WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute' OR last_used_ip <> $2)`, id, ip)
	return err
}

func (r *PgIdentityStore) GetMFA(ctx context.Context, userID int64) (*MFARecord, error) {
	var m MFARecord
	err := r.DB.QueryRow(ctx, `SELECT user_id, secret_enc, enabled, last_step, recovery_hashes, enabled_at FROM user_mfa WHERE user_id=$1`, userID).
		Scan(&m.UserID, &m.SecretEnc, &m.Enabled, &m.LastStep, &m.RecoveryHashes, &m.EnabledAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &m, err
}

func (r *PgIdentityStore) SavePendingMFA(ctx context.Context, userID int64, secretEnc string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `INSERT INTO user_mfa(user_id, secret_enc, enabled) VALUES ($1,$2,FALSE)
		ON CONFLICT (user_id) DO UPDATE SET secret_enc=EXCLUDED.secret_enc, last_step=0, recovery_hashes='{}', updated_at=now()
		WHERE user_mfa.enabled = FALSE`, userID, secretEnc)
	return tag.RowsAffected() == 1, err
}

func (r *PgIdentityStore) ActivateMFA(ctx context.Context, userID int64, secretEnc string, step int64, hashes []string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `INSERT INTO user_mfa(user_id, secret_enc, enabled, last_step, recovery_hashes, enabled_at)
		VALUES ($1,$2,TRUE,$3,$4,now())
		ON CONFLICT (user_id) DO UPDATE SET secret_enc=EXCLUDED.secret_enc, enabled=TRUE, last_step=EXCLUDED.last_step,
		  recovery_hashes=EXCLUDED.recovery_hashes, enabled_at=now(), updated_at=now()
		WHERE user_mfa.enabled = FALSE`, userID, secretEnc, step, hashes)
	return tag.RowsAffected() == 1, err
}

func (r *PgIdentityStore) AdvanceMFAStep(ctx context.Context, userID, step int64) (bool, error) {
	tag, err := r.DB.Exec(ctx, `UPDATE user_mfa SET last_step=$2, updated_at=now() WHERE user_id=$1 AND enabled AND last_step < $2`, userID, step)
	return tag.RowsAffected() == 1, err
}

func (r *PgIdentityStore) ConsumeRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `UPDATE user_mfa SET recovery_hashes=array_remove(recovery_hashes, $2), updated_at=now()
		WHERE user_id=$1 AND enabled AND $2 = ANY(recovery_hashes)`, userID, hash)
	return tag.RowsAffected() == 1, err
}

func (r *PgIdentityStore) SetRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	tag, err := r.DB.Exec(ctx, `UPDATE user_mfa SET recovery_hashes=$2, updated_at=now() WHERE user_id=$1 AND enabled`, userID, hashes)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.ErrNotFound
	}
	return err
}

func (r *PgIdentityStore) DeleteMFA(ctx context.Context, userID int64) error {
	_, err := r.DB.Exec(ctx, `DELETE FROM user_mfa WHERE user_id=$1`, userID)
	return err
}

func (r *PgIdentityStore) CreateChallenge(ctx context.Context, ch *MFAChallenge) error {
	// opportunistic cleanup of expired tickets
	_, _ = r.DB.Exec(ctx, `DELETE FROM mfa_challenges WHERE expires_at < now() - interval '1 hour'`)
	_, err := r.DB.Exec(ctx, `INSERT INTO mfa_challenges(ticket_hash, user_id, purpose, pending_secret, ip, user_agent, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, ch.Hash, ch.UserID, ch.Purpose, ch.PendingSecret, ch.IP, ch.UserAgent, ch.ExpiresAt)
	return err
}

func (r *PgIdentityStore) ChallengeByHash(ctx context.Context, hash string) (*MFAChallenge, error) {
	var ch MFAChallenge
	err := r.DB.QueryRow(ctx, `SELECT ticket_hash, user_id, purpose, pending_secret, attempts, ip, user_agent, expires_at
		FROM mfa_challenges WHERE ticket_hash=$1`, hash).
		Scan(&ch.Hash, &ch.UserID, &ch.Purpose, &ch.PendingSecret, &ch.Attempts, &ch.IP, &ch.UserAgent, &ch.ExpiresAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &ch, err
}

func (r *PgIdentityStore) BumpChallenge(ctx context.Context, hash string) (int, error) {
	var n int
	err := r.DB.QueryRow(ctx, `UPDATE mfa_challenges SET attempts=attempts+1 WHERE ticket_hash=$1 RETURNING attempts`, hash).Scan(&n)
	if store.IsNoRows(err) {
		return 0, httpx.ErrNotFound
	}
	return n, err
}

func (r *PgIdentityStore) DeleteChallenge(ctx context.Context, hash string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `DELETE FROM mfa_challenges WHERE ticket_hash=$1`, hash)
	return tag.RowsAffected() == 1, err
}

// MFAPolicyKey is the settings key of the MFA enforcement policy.
const MFAPolicyKey = "security.mfa"

func (r *PgIdentityStore) LoadMFAPolicy(ctx context.Context) (MFAPolicy, error) {
	var raw []byte
	var p MFAPolicy
	err := r.DB.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1 AND NOT secret`, MFAPolicyKey).Scan(&raw)
	if store.IsNoRows(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}

func (r *PgIdentityStore) SaveMFAPolicy(ctx context.Context, p MFAPolicy, by int64) error {
	b, _ := json.Marshal(p)
	_, err := r.DB.Exec(ctx, `INSERT INTO settings(key, value, secret, description, updated_by) VALUES ($1,$2::jsonb,FALSE,'两步验证强制策略',$3)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, secret=FALSE, updated_by=EXCLUDED.updated_by, updated_at=now()`,
		MFAPolicyKey, string(b), by)
	return err
}
