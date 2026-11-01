package bastion

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
)

const (
	leaseSeconds      = 15
	heartbeatInterval = 2 * time.Second
	databaseTimeout   = 2 * time.Second
)

var errFenced = errors.New("session ownership or lease lost")

func newToken() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

type sessionRef struct {
	ID           int64
	Owner, Fence string
}

type sessionEnd struct {
	Status, Error, RecordingPath, RecordingState, StorageID string
}

type killResult struct {
	RequestID    string
	Status       string
	Acknowledged bool
}

func (r killResult) killed() bool {
	return r.RequestID != "" && r.Status == "killed" && r.Acknowledged
}

type lifecycleStore interface {
	renew(context.Context, sessionRef) (bool, error)
	finish(context.Context, sessionRef, sessionEnd) error
	reap(context.Context) error
}

type pgLifecycleStore struct{ db *pgxpool.Pool }

// Every mutation uses database time and the immutable process + session fence.
// Expired sessions cannot be renewed or "finished" by a delayed old worker.
func (s *pgLifecycleStore) renew(ctx context.Context, r sessionRef) (bool, error) {
	var kill bool
	err := s.db.QueryRow(ctx, `UPDATE bastion_sessions
		SET heartbeat_at=clock_timestamp(),lease_expires_at=clock_timestamp()+make_interval(secs => $4)
		WHERE id=$1 AND owner_id=$2 AND fence_token=$3 AND status='active'
		  AND lease_expires_at>clock_timestamp()
		RETURNING kill_requested_at IS NOT NULL`, r.ID, r.Owner, r.Fence, leaseSeconds).Scan(&kill)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errFenced
	}
	return kill, err
}

func (s *pgLifecycleStore) finish(ctx context.Context, r sessionRef, e sessionEnd) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE bastion_sessions SET
		status=$4,error=$5,recording_path=$6,recording_state=$7,recording_storage_id=$8,
		ended_at=clock_timestamp(),lease_expires_at=NULL,
		kill_acknowledged_at=CASE WHEN $4='killed' AND kill_requested_at IS NOT NULL
		  THEN clock_timestamp() ELSE NULL END
		WHERE id=$1 AND owner_id=$2 AND fence_token=$3 AND status='active'
		  AND lease_expires_at>clock_timestamp()
		  AND ($4<>'killed' OR kill_requested_at IS NOT NULL)`,
		r.ID, r.Owner, r.Fence, e.Status, e.Error, e.RecordingPath, e.RecordingState, e.StorageID)
	if err == nil && tag.RowsAffected() != 1 {
		return errFenced
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE bastion_commands SET delivery_state='unknown',
		delivery_error='session ended before delivery could be confirmed'
		WHERE session_id=$1 AND delivery_state='pending'`, r.ID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *pgLifecycleStore) reap(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `WITH expired AS (UPDATE bastion_sessions SET status='failed',
		error='session owner lease expired; remote outcome unknown',ended_at=clock_timestamp(),
		recording_state=CASE WHEN recording_state='recording' THEN 'failed' ELSE recording_state END
		WHERE status='active' AND (
			(lease_expires_at IS NOT NULL AND lease_expires_at<=clock_timestamp()) OR
			(owner_id='' AND lease_expires_at IS NULL AND started_at<clock_timestamp()-interval '60 seconds'))
		RETURNING id)
		UPDATE bastion_commands c SET delivery_state='unknown',
		delivery_error='session owner lease expired; remote delivery outcome unknown'
		FROM expired s WHERE c.session_id=s.id AND c.delivery_state='pending'`)
	return err
}

func (s *pgLifecycleStore) requestKill(ctx context.Context, id, user int64) (killResult, error) {
	var r killResult
	err := s.db.QueryRow(ctx, `UPDATE bastion_sessions SET
		kill_request_id=CASE WHEN kill_request_id='' THEN $3 ELSE kill_request_id END,
		kill_requested_at=COALESCE(kill_requested_at,clock_timestamp()),
		kill_requested_by=COALESCE(kill_requested_by,$2)
		WHERE id=$1 AND status='active' AND owner_id<>'' AND fence_token<>''
		  AND lease_expires_at>clock_timestamp()
		RETURNING kill_request_id,status,kill_acknowledged_at IS NOT NULL`,
		id, user, newToken()).Scan(&r.RequestID, &r.Status, &r.Acknowledged)
	return r, err
}

func (s *pgLifecycleStore) killState(ctx context.Context, id int64, request string) (killResult, error) {
	var r killResult
	err := s.db.QueryRow(ctx, `SELECT kill_request_id,status,kill_acknowledged_at IS NOT NULL
		FROM bastion_sessions WHERE id=$1 AND kill_request_id=$2`, id, request).
		Scan(&r.RequestID, &r.Status, &r.Acknowledged)
	return r, err
}

// Start one watchdog immediately after registration, including during SSH setup.
// A database failure cancels transport instead of extending an unverified lease.
func (m *Module) watchLease(ctx context.Context, ref sessionRef) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(heartbeatInterval)
		defer tick.Stop()
		for {
			check, cancel := context.WithTimeout(ctx, databaseTimeout)
			kill, err := m.store.renew(check, ref)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				m.cancelSession(ref.ID, "lease_lost")
				return
			}
			if kill {
				m.cancelSession(ref.ID, "killed")
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return done
}
