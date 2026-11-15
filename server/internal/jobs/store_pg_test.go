package jobs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPgStoreLifecycle needs a migrated PostgreSQL (MATRIX_TEST_DSN) with
// core migration 0012_jobs applied; it is skipped otherwise.
func TestPgStoreLifecycle(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DSN")
	if dsn == "" {
		t.Skip("MATRIX_TEST_DSN not set; skipping PostgreSQL job queue test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `SELECT 1 FROM platform_jobs LIMIT 1`); err != nil {
		t.Skipf("platform_jobs missing: %v", err)
	}
	typ := "test.jobs." + time.Now().Format("150405.000000")
	defer pool.Exec(ctx, `DELETE FROM platform_jobs WHERE type=$1`, typ)
	s := &PgStore{DB: pool}

	id, err := s.Enqueue(ctx, EnqueueRequest{Type: typ, Payload: map[string]any{"n": 1}, MaxAttempts: 2, DedupeKey: typ})
	if err != nil {
		t.Fatal(err)
	}
	if dup, err := s.Enqueue(ctx, EnqueueRequest{Type: typ, DedupeKey: typ}); !errors.Is(err, ErrDuplicate) || dup != id {
		t.Fatalf("dedupe: id=%d err=%v", dup, err)
	}

	// two workers: SKIP LOCKED must hand the single job to only one of them
	a, err := s.Claim(ctx, "wa", []string{typ}, time.Minute)
	if err != nil || a == nil || a.ID != id || a.Attempts != 1 || a.Status != StatusRunning {
		t.Fatalf("claim a: %+v %v", a, err)
	}
	if b, err := s.Claim(ctx, "wb", []string{typ}, time.Minute); err != nil || b != nil {
		t.Fatalf("claim b must be empty: %+v %v", b, err)
	}
	if err := s.Heartbeat(ctx, id, "wb", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign heartbeat: %v", err)
	}
	if err := s.Finish(ctx, id, "wa", Outcome{Status: StatusFailed, Delay: 0, Error: "e1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("delete failed job: %v", err)
	}

	// second attempt: lease expires -> reaper dead-letters (attempts == max)
	c, err := s.Claim(ctx, "wa", []string{typ}, time.Millisecond)
	if err != nil || c == nil || c.Attempts != 2 {
		t.Fatalf("claim c: %+v %v", c, err)
	}
	time.Sleep(20 * time.Millisecond)
	reaped, err := s.ReapExpired(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range reaped {
		found = found || (r.ID == id && r.Status == StatusDead)
	}
	if !found {
		t.Fatalf("not reaped to dead: %+v", reaped)
	}
	if err := s.Finish(ctx, id, "wa", Outcome{Status: StatusSucceeded}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late finish after reap: %v", err)
	}

	j, err := s.Retry(ctx, id)
	if err != nil || j.Status != StatusQueued || j.Attempts != 0 {
		t.Fatalf("retry dead: %+v %v", j, err)
	}
	d, err := s.Claim(ctx, "wa", []string{typ}, time.Minute)
	if err != nil || d == nil {
		t.Fatalf("claim d: %v", err)
	}
	if err := s.Finish(ctx, id, "wa", Outcome{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	items, total, err := s.List(ctx, ListFilter{Statuses: []string{StatusSucceeded}, Type: typ}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].FinishedAt == nil {
		t.Fatalf("list: %d %+v %v", total, items, err)
	}
	if _, err := s.Retry(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry succeeded: %v", err)
	}
	if _, err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}
