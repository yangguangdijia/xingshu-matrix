package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// User is the auth view of a user row.
type User struct {
	ID                 int64
	Username           string
	DisplayName        string
	PasswordHash       string
	Status             string
	MustChangePassword bool
	LockedUntil        *time.Time
}

// Session is a refresh-token family.
type Session struct {
	ID          string     `json:"id"`
	UserID      int64      `json:"user_id"`
	RefreshHash string     `json:"-"`
	UserAgent   string     `json:"user_agent"`
	IP          string     `json:"ip"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  time.Time  `json:"last_used_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	Current     bool       `json:"current"`
}

// Repo is the persistence the auth service needs (PgRepo in production, fakes in tests).
type Repo interface {
	UserByUsername(ctx context.Context, username string) (*User, error) // httpx.ErrNotFound when missing
	UserByID(ctx context.Context, id int64) (*User, error)
	// RecordLoginFailure increments the failure counter; when it reaches max the
	// account is locked until lockUntil and the counter resets. Returns the new
	// counter and whether the account is now locked.
	RecordLoginFailure(ctx context.Context, userID int64, max int, lockUntil time.Time) (failures int, locked bool, err error)
	RecordLoginSuccess(ctx context.Context, userID int64, ip string) error
	UpdatePassword(ctx context.Context, userID int64, hash string, mustChange bool) error
	CreateSession(ctx context.Context, s *Session) error
	SessionByID(ctx context.Context, id string) (*Session, error)
	// RotateSession swaps the refresh hash only if it still equals oldHash.
	RotateSession(ctx context.Context, id, oldHash, newHash string) (bool, error)
	RevokeSession(ctx context.Context, id string) error
	RevokeUserSessions(ctx context.Context, userID int64, exceptID string) error
	ListUserSessions(ctx context.Context, userID int64) ([]Session, error)
	CountUsers(ctx context.Context) (int64, error)
	// CreateBuiltinAdmin creates user "admin" with the admin role.
	CreateBuiltinAdmin(ctx context.Context, hash string) error
}

// PgRepo implements Repo on PostgreSQL.
type PgRepo struct{ DB store.DB }

func (r *PgRepo) scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Status, &u.MustChangePassword, &u.LockedUntil)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &u, err
}

const userCols = `id, username, display_name, password_hash, status, must_change_password, locked_until`

func (r *PgRepo) UserByUsername(ctx context.Context, username string) (*User, error) {
	return r.scanUser(r.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(username)=lower($1)`, username))
}

func (r *PgRepo) UserByID(ctx context.Context, id int64) (*User, error) {
	return r.scanUser(r.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r *PgRepo) RecordLoginFailure(ctx context.Context, userID int64, max int, lockUntil time.Time) (int, bool, error) {
	var failures int
	var locked bool
	err := r.DB.QueryRow(ctx, `
		UPDATE users SET
		  locked_until    = CASE WHEN failed_attempts + 1 >= $2 THEN $3 ELSE locked_until END,
		  failed_attempts = CASE WHEN failed_attempts + 1 >= $2 THEN 0 ELSE failed_attempts + 1 END,
		  updated_at = now()
		WHERE id=$1
		RETURNING failed_attempts, (locked_until IS NOT NULL AND locked_until > now())`,
		userID, max, lockUntil).Scan(&failures, &locked)
	if locked {
		failures = max
	}
	return failures, locked, err
}

func (r *PgRepo) RecordLoginSuccess(ctx context.Context, userID int64, ip string) error {
	_, err := r.DB.Exec(ctx, `UPDATE users SET failed_attempts=0, locked_until=NULL, last_login_at=now(), last_login_ip=$2 WHERE id=$1`, userID, ip)
	return err
}

func (r *PgRepo) UpdatePassword(ctx context.Context, userID int64, hash string, mustChange bool) error {
	tag, err := r.DB.Exec(ctx, `UPDATE users SET password_hash=$2, must_change_password=$3, password_changed_at=now(),
		failed_attempts=0, locked_until=NULL, updated_at=now() WHERE id=$1`, userID, hash, mustChange)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.ErrNotFound
	}
	return err
}

func (r *PgRepo) CreateSession(ctx context.Context, s *Session) error {
	return r.DB.QueryRow(ctx, `INSERT INTO sessions(id, user_id, refresh_hash, user_agent, ip, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at, last_used_at`,
		s.ID, s.UserID, s.RefreshHash, s.UserAgent, s.IP, s.ExpiresAt).Scan(&s.CreatedAt, &s.LastUsedAt)
}

const sessionCols = `id, user_id, refresh_hash, user_agent, ip, created_at, last_used_at, expires_at, revoked_at`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.RefreshHash, &s.UserAgent, &s.IP, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt, &s.RevokedAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	return &s, err
}

func (r *PgRepo) SessionByID(ctx context.Context, id string) (*Session, error) {
	return scanSession(r.DB.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=$1`, id))
}

func (r *PgRepo) RotateSession(ctx context.Context, id, oldHash, newHash string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `UPDATE sessions SET refresh_hash=$3, last_used_at=now()
		WHERE id=$1 AND refresh_hash=$2 AND revoked_at IS NULL AND expires_at > now()`, id, oldHash, newHash)
	return tag.RowsAffected() == 1, err
}

func (r *PgRepo) RevokeSession(ctx context.Context, id string) error {
	_, err := r.DB.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	return err
}

func (r *PgRepo) RevokeUserSessions(ctx context.Context, userID int64, exceptID string) error {
	_, err := r.DB.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND id<>$2 AND revoked_at IS NULL`, userID, exceptID)
	return err
}

func (r *PgRepo) ListUserSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := r.DB.Query(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE user_id=$1 AND revoked_at IS NULL AND expires_at > now() ORDER BY last_used_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (r *PgRepo) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := r.DB.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (r *PgRepo) CreateBuiltinAdmin(ctx context.Context, hash string) error {
	return store.InTx(ctx, r.DB, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO users(username, display_name, password_hash, is_builtin, must_change_password)
			VALUES ('admin', '系统管理员', $1, TRUE, TRUE)
			ON CONFLICT DO NOTHING RETURNING id`, hash).Scan(&id)
		if store.IsNoRows(err) {
			return nil // created concurrently by another replica
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_roles(user_id, role_id) SELECT $1, id FROM roles WHERE code='admin'`, id)
		return err
	})
}

// PurgeExpiredSessions deletes sessions that expired or were revoked more than a day ago.
func (r *PgRepo) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := r.DB.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() - interval '1 day' OR revoked_at < now() - interval '1 day'`)
	return tag.RowsAffected(), err
}
