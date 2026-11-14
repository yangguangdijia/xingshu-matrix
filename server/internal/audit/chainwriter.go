package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// chainHead is the locked state of the live segment inside one transaction.
type chainHead struct {
	segID   int64
	startID *int64
	anchor  string
	headID  *int64
	head    string
	added   int64
}

// prev returns the hash the next row chains to: head_hash, or the segment
// anchor (GenesisHash by default) when the segment is still empty.
func (h *chainHead) prev() string {
	if h.head != "" {
		return h.head
	}
	if h.anchor != "" {
		return h.anchor
	}
	return GenesisHash
}

// link chains row r onto the head and advances it, returning (prev, hash).
func (h *chainHead) link(r ChainRow) (string, string) {
	prev := h.prev()
	hash := CanonicalHash(prev, r)
	if h.startID == nil {
		id := r.ID
		h.startID = &id
	}
	id := r.ID
	h.headID, h.head = &id, hash
	h.added++
	return prev, hash
}

const lockLiveSQL = `SELECT id, start_id, anchor_hash, head_id, head_hash FROM audit_chain_segments WHERE kind='live' FOR UPDATE`

func lockLive(ctx context.Context, tx pgx.Tx) (*chainHead, error) {
	h := &chainHead{}
	err := tx.QueryRow(ctx, lockLiveSQL).Scan(&h.segID, &h.startID, &h.anchor, &h.headID, &h.head)
	if store.IsNoRows(err) {
		return nil, errors.New("audit chain: live segment missing (migration 0011 not applied?)")
	}
	if err != nil {
		return nil, fmt.Errorf("audit chain: lock live segment: %w", err)
	}
	return h, nil
}

const insertChainedSQL = `INSERT INTO audit_logs(user_id, username, ip, method, path, resource, resource_id, action,
	status_code, result, duration_ms, request_id, user_agent, detail, created_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,COALESCE($15::timestamptz, now()))
	RETURNING ` + entryCols

// insertLinked inserts e, hashes the row exactly as stored (RETURNING, so jsonb
// and timestamp normalisation match what verification later reads) and writes
// prev_hash/hash back.
func insertLinked(ctx context.Context, tx pgx.Tx, h *chainHead, e Entry) (int64, error) {
	var detail, created any
	if len(e.Detail) > 0 {
		detail = string(e.Detail)
	}
	if !e.CreatedAt.IsZero() {
		created = e.CreatedAt
	}
	if e.Result == "" {
		e.Result = ResultSuccess
	}
	stored, err := scanEntry(tx.QueryRow(ctx, insertChainedSQL,
		e.UserID, e.Username, e.IP, e.Method, e.Path, e.Resource, e.ResourceID, e.Action,
		e.StatusCode, e.Result, e.DurationMs, e.RequestID, e.UserAgent, detail, created))
	if err != nil {
		return 0, fmt.Errorf("audit chain: insert: %w", err)
	}
	prev, hash := h.link(entryToChainRow(stored))
	if _, err := tx.Exec(ctx, `UPDATE audit_logs SET prev_hash=$2, hash=$3 WHERE id=$1`, stored.ID, prev, hash); err != nil {
		return 0, fmt.Errorf("audit chain: set hash: %w", err)
	}
	return stored.ID, nil
}

func saveHead(ctx context.Context, tx pgx.Tx, h *chainHead) error {
	if h.added == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE audit_chain_segments
		SET head_id=$2, head_hash=$3, row_count=row_count+$4, start_id=COALESCE(start_id, $5), updated_at=now()
		WHERE id=$1`, h.segID, h.headID, h.head, h.added, h.startID)
	if err != nil {
		return fmt.Errorf("audit chain: advance head: %w", err)
	}
	return nil
}

// WriteChained appends one entry to the live hash chain inside the caller's
// transaction: it locks the live segment (SELECT ... FOR UPDATE), inserts the
// row, links it to head_hash (GenesisHash/anchor for an empty segment) and
// advances the segment head. The lock is held until tx ends, so concurrent
// writers serialise and ids along the chain are strictly increasing. The
// returned id is the new audit_logs row.
func WriteChained(ctx context.Context, tx pgx.Tx, e Entry) (int64, error) {
	ids, err := writeChainedBatch(ctx, tx, []Entry{e})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func writeChainedBatch(ctx context.Context, tx pgx.Tx, entries []Entry) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	h, err := lockLive(ctx, tx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		id, err := insertLinked(ctx, tx, h, e)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, saveHead(ctx, tx, h)
}

func entryToChainRow(e Entry) ChainRow {
	return ChainRow{
		ID: e.ID, UserID: e.UserID, Username: e.Username, IP: e.IP, Method: e.Method, Path: e.Path,
		Resource: e.Resource, ResourceID: e.ResourceID, Action: e.Action, StatusCode: e.StatusCode,
		Result: e.Result, DurationMs: e.DurationMs, RequestID: e.RequestID, UserAgent: e.UserAgent,
		Detail: e.Detail, CreatedAt: e.CreatedAt,
	}
}
