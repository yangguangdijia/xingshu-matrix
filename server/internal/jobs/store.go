package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// QueueStore is the persistence the worker needs (PgStore in production; a
// fake in unit tests).
type QueueStore interface {
	// Claim locks the next runnable job of one of types and marks it running
	// under workerID for lease. It returns (nil, nil) when nothing is runnable.
	Claim(ctx context.Context, workerID string, types []string, lease time.Duration) (*Job, error)
	// Heartbeat extends the lease; ErrLeaseLost when workerID no longer owns it.
	Heartbeat(ctx context.Context, id int64, workerID string, lease time.Duration) error
	// Finish records an attempt's outcome; ErrLeaseLost when the lease was lost.
	Finish(ctx context.Context, id int64, workerID string, o Outcome) error
	// ReapExpired moves running jobs whose lease expired to failed / dead.
	ReapExpired(ctx context.Context, limit int) ([]Reaped, error)
	// CountByStatus returns the number of jobs per status.
	CountByStatus(ctx context.Context) (map[string]int64, error)
}

// Reaped is one job recovered by ReapExpired.
type Reaped struct {
	ID     int64
	Type   string
	Status string
}

// EnqueueRequest describes a new job.
type EnqueueRequest struct {
	Type    string
	Payload any // marshalled to JSON; nil = {}
	// MaxAttempts 0 uses the registered type's default (or DefaultMaxAttempts).
	MaxAttempts int
	Priority    int
	// RunAt zero = now. Delay is added to now when RunAt is zero.
	RunAt time.Time
	Delay time.Duration
	// DedupeKey, when set, rejects the enqueue (ErrDuplicate, returning the
	// existing id) while another unfinished job has the same key.
	DedupeKey string
	CreatedBy int64
}

// PgStore implements QueueStore and the admin operations on platform_jobs.
type PgStore struct {
	DB       store.DB
	Registry *Registry // optional: supplies per-type max_attempts defaults
}

const jobCols = `id, type, payload, status, priority, attempts, max_attempts, run_at, locked_by, locked_until,
	last_error, dedupe_key, created_by, created_at, updated_at, started_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var payload []byte
	if err := row.Scan(&j.ID, &j.Type, &payload, &j.Status, &j.Priority, &j.Attempts, &j.MaxAttempts, &j.RunAt,
		&j.LockedBy, &j.LockedUntil, &j.LastError, &j.DedupeKey, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt,
		&j.StartedAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	j.Payload = payload
	return &j, nil
}

func secs(d time.Duration) float64 { return d.Seconds() }

// Enqueue inserts a job using s.DB.
func (s *PgStore) Enqueue(ctx context.Context, req EnqueueRequest) (int64, error) {
	return s.EnqueueTx(ctx, s.DB, req)
}

// EnqueueTx inserts a job through db, so callers can enqueue inside their own
// transaction (the job becomes visible only if the transaction commits).
func (s *PgStore) EnqueueTx(ctx context.Context, db store.DB, req EnqueueRequest) (int64, error) {
	if err := ValidateType(req.Type); err != nil {
		return 0, err
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts == 0 {
		if s.Registry != nil {
			maxAttempts = s.Registry.Options(req.Type).MaxAttempts
		} else {
			maxAttempts = DefaultMaxAttempts
		}
	}
	if maxAttempts < 1 || maxAttempts > MaxMaxAttempts {
		return 0, errBadMaxAttempt
	}
	payload := []byte("{}")
	if req.Payload != nil {
		switch p := req.Payload.(type) {
		case json.RawMessage:
			if len(p) > 0 {
				payload = p
			}
		case []byte:
			if len(p) > 0 {
				payload = p
			}
		default:
			b, err := json.Marshal(p)
			if err != nil {
				return 0, fmt.Errorf("marshal job payload: %w", err)
			}
			payload = b
		}
		if !json.Valid(payload) {
			return 0, errors.New("job payload is not valid JSON")
		}
	}
	var runAt any
	if !req.RunAt.IsZero() {
		runAt = req.RunAt
	}
	var dedupe any
	if k := strings.TrimSpace(req.DedupeKey); k != "" {
		dedupe = k
	}
	var createdBy any
	if req.CreatedBy > 0 {
		createdBy = req.CreatedBy
	}
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO platform_jobs (type, payload, priority, max_attempts, run_at, dedupe_key, created_by)
		VALUES ($1, $2::jsonb, $3, $4, COALESCE($5::timestamptz, now() + make_interval(secs => $6)), $7, $8)
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued','running','failed') DO NOTHING
		RETURNING id`,
		req.Type, string(payload), req.Priority, maxAttempts, runAt, secs(req.Delay), dedupe, createdBy).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) && dedupe != nil {
		var existing int64
		if e := db.QueryRow(ctx, `SELECT id FROM platform_jobs WHERE dedupe_key=$1 AND status IN ('queued','running','failed') ORDER BY id LIMIT 1`,
			dedupe).Scan(&existing); e != nil {
			return 0, ErrDuplicate
		}
		return existing, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Claim implements QueueStore.
func (s *PgStore) Claim(ctx context.Context, workerID string, types []string, lease time.Duration) (*Job, error) {
	if len(types) == 0 {
		return nil, nil
	}
	j, err := scanJob(s.DB.QueryRow(ctx, `UPDATE platform_jobs SET status='running', attempts=attempts+1,
			locked_by=$1, locked_until=now() + make_interval(secs => $2), started_at=now(), updated_at=now()
		WHERE id = (SELECT id FROM platform_jobs
			WHERE status IN ('queued','failed') AND run_at <= now() AND type = ANY($3)
			ORDER BY priority DESC, run_at, id
			FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+jobCols, workerID, secs(lease), types))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// Heartbeat implements QueueStore.
func (s *PgStore) Heartbeat(ctx context.Context, id int64, workerID string, lease time.Duration) error {
	tag, err := s.DB.Exec(ctx, `UPDATE platform_jobs SET locked_until=now() + make_interval(secs => $3), updated_at=now()
		WHERE id=$1 AND status='running' AND locked_by=$2`, id, workerID, secs(lease))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Finish implements QueueStore. On success the previous last_error is kept as
// history of the last failed attempt.
func (s *PgStore) Finish(ctx context.Context, id int64, workerID string, o Outcome) error {
	switch o.Status {
	case StatusSucceeded, StatusFailed, StatusDead:
	default:
		return fmt.Errorf("jobs: invalid outcome status %q", o.Status)
	}
	tag, err := s.DB.Exec(ctx, `UPDATE platform_jobs SET status=$3,
			run_at = CASE WHEN $3='failed' THEN now() + make_interval(secs => $4) ELSE run_at END,
			last_error = CASE WHEN $3='succeeded' THEN last_error ELSE $5 END,
			locked_by='', locked_until=NULL,
			finished_at = CASE WHEN $3 IN ('succeeded','dead') THEN now() ELSE NULL END,
			updated_at=now()
		WHERE id=$1 AND status='running' AND locked_by=$2`, id, workerID, o.Status, secs(o.Delay), o.Error)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// leaseExpiredMsg is stored in last_error for jobs recovered by the reaper.
const leaseExpiredMsg = "执行租约超时（执行节点可能已退出），已由回收器回收"

// ReapExpired implements QueueStore. Recovered jobs are runnable again
// immediately (the expired lease already delayed them).
func (s *PgStore) ReapExpired(ctx context.Context, limit int) ([]Reaped, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `UPDATE platform_jobs SET
			status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'failed' END,
			finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
			run_at=now(), locked_by='', locked_until=NULL, last_error=$2, updated_at=now()
		WHERE id IN (SELECT id FROM platform_jobs WHERE status='running' AND locked_until < now()
			ORDER BY locked_until FOR UPDATE SKIP LOCKED LIMIT $1)
		RETURNING id, type, status`, limit, leaseExpiredMsg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reaped
	for rows.Next() {
		var r Reaped
		if err := rows.Scan(&r.ID, &r.Type, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountByStatus implements QueueStore. Every status is present (0 if none).
func (s *PgStore) CountByStatus(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(AllStatuses))
	for _, st := range AllStatuses {
		out[st] = 0
	}
	rows, err := s.DB.Query(ctx, `SELECT status, count(*) FROM platform_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ListFilter selects jobs for the admin list.
type ListFilter struct {
	Statuses []string
	Type     string
}

// List returns jobs newest first and the total matching count.
func (s *PgStore) List(ctx context.Context, f ListFilter, limit, offset int) ([]Job, int64, error) {
	where, args := []string{"TRUE"}, []any{}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("type = $%d", len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM platform_jobs WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM platform_jobs WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`,
		jobCols, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *j)
	}
	return out, total, rows.Err()
}

// Get returns one job or ErrNotFound.
func (s *PgStore) Get(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.DB.QueryRow(ctx, `SELECT `+jobCols+` FROM platform_jobs WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Retry requeues a failed or dead job to run now. A dead job gets a fresh
// attempt budget (attempts reset to 0); a failed job keeps its count and only
// skips the remaining backoff. last_error is kept until the next attempt.
func (s *PgStore) Retry(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.DB.QueryRow(ctx, `UPDATE platform_jobs SET
			attempts = CASE WHEN status='dead' THEN 0 ELSE attempts END,
			status='queued', run_at=now(), locked_by='', locked_until=NULL, finished_at=NULL, updated_at=now()
		WHERE id=$1 AND status IN ('failed','dead')
		RETURNING `+jobCols, id))
	switch {
	case err == nil:
		return j, nil
	case store.IsUniqueViolation(err):
		return nil, ErrDuplicate
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if _, gerr := s.Get(ctx, id); gerr != nil {
		return nil, gerr
	}
	return nil, ErrInvalidState
}

// Delete removes a dead or succeeded job and returns it as it was.
func (s *PgStore) Delete(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.DB.QueryRow(ctx, `DELETE FROM platform_jobs WHERE id=$1 AND status IN ('dead','succeeded') RETURNING `+jobCols, id))
	if err == nil {
		return j, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, gerr := s.Get(ctx, id); gerr != nil {
		return nil, gerr
	}
	return nil, ErrInvalidState
}

var _ QueueStore = (*PgStore)(nil)
