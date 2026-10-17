package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

var errNotPending = errors.New("ticket not pending")

// ListFilter narrows ticket listings.
type ListFilter struct {
	Status             string
	Type               string
	Env                string
	ApplicantID        int64
	ExcludeApplicantID int64
}

// Repo persists tickets (PgRepo in production, fakes in tests).
type Repo interface {
	Insert(ctx context.Context, t *Ticket) error
	Get(ctx context.Context, id int64) (*Ticket, error)
	// Decide moves a pending ticket to status; errNotPending if it was not pending.
	Decide(ctx context.Context, id int64, status string, approverID int64, approverName, comment string) (*Ticket, error)
	Finish(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error
	List(ctx context.Context, f ListFilter, p httpx.Page) ([]Ticket, int64, error)
}

// PgRepo implements Repo on PostgreSQL.
type PgRepo struct{ DB store.DB }

const ticketCols = `id, type, title, env, reason, payload, status, applicant_id, applicant_name, approver_id,
	approver_name, comment, result, error, created_at, updated_at, decided_at, executed_at`

func scanTicket(row pgx.Row) (*Ticket, error) {
	var t Ticket
	var payload, result []byte
	err := row.Scan(&t.ID, &t.Type, &t.Title, &t.Env, &t.Reason, &payload, &t.Status, &t.ApplicantID, &t.ApplicantName,
		&t.ApproverID, &t.ApproverName, &t.Comment, &result, &t.Error, &t.CreatedAt, &t.UpdatedAt, &t.DecidedAt, &t.ExecutedAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	t.Payload, t.Result = payload, result
	return &t, err
}

func (r *PgRepo) Insert(ctx context.Context, t *Ticket) error {
	return r.DB.QueryRow(ctx, `INSERT INTO approval_tickets(type, title, env, reason, payload, status, applicant_id, applicant_name)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8) RETURNING id, created_at, updated_at`,
		t.Type, t.Title, t.Env, t.Reason, string(t.Payload), t.Status, t.ApplicantID, t.ApplicantName).
		Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

func (r *PgRepo) Get(ctx context.Context, id int64) (*Ticket, error) {
	return scanTicket(r.DB.QueryRow(ctx, `SELECT `+ticketCols+` FROM approval_tickets WHERE id=$1`, id))
}

func (r *PgRepo) Decide(ctx context.Context, id int64, status string, approverID int64, approverName, comment string) (*Ticket, error) {
	t, err := scanTicket(r.DB.QueryRow(ctx, `UPDATE approval_tickets SET status=$2, approver_id=$3, approver_name=$4, comment=$5,
		decided_at=now(), updated_at=now() WHERE id=$1 AND status='pending' RETURNING `+ticketCols,
		id, status, approverID, approverName, comment))
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, errNotPending
	}
	return t, err
}

func (r *PgRepo) Finish(ctx context.Context, id int64, status string, result json.RawMessage, errMsg string) error {
	var res any
	if len(result) > 0 {
		res = string(result)
	}
	_, err := r.DB.Exec(ctx, `UPDATE approval_tickets SET status=$2, result=$3::jsonb, error=$4, executed_at=now(), updated_at=now()
		WHERE id=$1`, id, status, res, errMsg)
	return err
}

func (r *PgRepo) List(ctx context.Context, f ListFilter, p httpx.Page) ([]Ticket, int64, error) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add("status = $%d", f.Status)
	}
	if f.Type != "" {
		add("type = $%d", f.Type)
	}
	if f.Env != "" {
		add("env = $%d", f.Env)
	}
	if f.ApplicantID > 0 {
		add("applicant_id = $%d", f.ApplicantID)
	}
	if f.ExcludeApplicantID > 0 {
		add("applicant_id <> $%d", f.ExcludeApplicantID)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int64
	if err := r.DB.QueryRow(ctx, `SELECT count(*) FROM approval_tickets`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := r.DB.Query(ctx, fmt.Sprintf(`SELECT %s FROM approval_tickets%s ORDER BY id DESC LIMIT $%d OFFSET $%d`,
		ticketCols, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}
