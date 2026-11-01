package bastion

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

func bastionTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run real PostgreSQL ownership/kill/stale/audit tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid disposable test database configuration")
	}
	if err = admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatal("disposable test PostgreSQL unavailable")
	}
	schema := "bastion_test_" + newToken()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("cannot clean isolated Bastion test schema")
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open isolated test pool")
	}
	t.Cleanup(pool.Close)
	names, err := fs.Glob(migFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body, err := migFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s failed: %v", name, err)
		}
	}
	return pool
}

func insertTestSession(t *testing.T, db *pgxpool.Pool, owner string) sessionRef {
	t.Helper()
	ref := sessionRef{Owner: owner, Fence: newToken()}
	err := db.QueryRow(context.Background(), `INSERT INTO bastion_sessions
		(user_id,username,asset_id,asset_name,account_id,account_user,protocol,
		 owner_id,fence_token,heartbeat_at,lease_expires_at)
		VALUES (1,'acceptance',1,'node',1,'operator','ssh',$1,$2,clock_timestamp(),clock_timestamp()+interval '15 seconds')
		RETURNING id`, ref.Owner, ref.Fence).Scan(&ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestPostgresOwnershipKillAndStaleRecovery(t *testing.T) {
	db := bastionTestDB(t)
	a, b := &pgLifecycleStore{db: db}, &pgLifecycleStore{db: db}
	ctx := context.Background()
	refA := insertTestSession(t, db, "owner-a")
	refB := insertTestSession(t, db, "owner-b")
	t.Run("new process does not close healthy owners", func(t *testing.T) {
		m := New()
		if err := m.Start(ctx, &app.Deps{DB: db, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
			t.Fatal(err)
		}
		defer m.Stop(ctx)
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM bastion_sessions WHERE id=ANY($1) AND status='active'`, []int64{refA.ID, refB.ID}).Scan(&n); err != nil || n != 2 {
			t.Fatalf("healthy foreign sessions changed: %d %v", n, err)
		}
	})
	t.Run("wrong owner and fence cannot renew or finish", func(t *testing.T) {
		for _, r := range []sessionRef{
			{ID: refA.ID, Owner: refB.Owner, Fence: refA.Fence},
			{ID: refA.ID, Owner: refA.Owner, Fence: newToken()},
		} {
			if _, err := b.renew(ctx, r); !errors.Is(err, errFenced) {
				t.Fatalf("renew accepted foreign fence: %v", err)
			}
			if err := b.finish(ctx, r, sessionEnd{Status: "closed"}); !errors.Is(err, errFenced) {
				t.Fatalf("finish accepted foreign fence: %v", err)
			}
		}
		if err := a.finish(ctx, refA, sessionEnd{Status: "killed"}); !errors.Is(err, errFenced) {
			t.Fatal("unrequested kill accepted")
		}
	})
	t.Run("other replica requests owner observes and acknowledges", func(t *testing.T) {
		request, err := b.requestKill(ctx, refA.ID, 99)
		if err != nil || request.killed() || request.RequestID == "" {
			t.Fatalf("invalid request result: %+v %v", request, err)
		}
		repeated, err := b.requestKill(ctx, refA.ID, 100)
		if err != nil || repeated.RequestID != request.RequestID {
			t.Fatal("kill request is not idempotent")
		}
		if kill, err := a.renew(ctx, refA); err != nil || !kill {
			t.Fatalf("owner missed persisted kill: %t %v", kill, err)
		}
		if err := a.finish(ctx, refA, sessionEnd{Status: "killed", RecordingState: "ready"}); err != nil {
			t.Fatal(err)
		}
		state, err := b.killState(ctx, refA.ID, request.RequestID)
		if err != nil || !state.killed() {
			t.Fatalf("owner acknowledgement not visible: %+v %v", state, err)
		}
		if _, err := a.renew(ctx, refB); err != nil {
			t.Fatal("other live owner affected")
		}
		if _, err := b.requestKill(ctx, refA.ID, 99); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("closed session accepted kill")
		}
	})
	t.Run("unobserved request and stale process never acknowledge kill", func(t *testing.T) {
		r := insertTestSession(t, db, "stale-owner")
		var commandID int64
		if err := db.QueryRow(ctx, `INSERT INTO bastion_commands
			(session_id,command,action,delivery_state) VALUES ($1,'public-marker','allow','pending') RETURNING id`, r.ID).Scan(&commandID); err != nil {
			t.Fatal(err)
		}
		request, err := b.requestKill(ctx, r.ID, 99)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(ctx, `UPDATE bastion_sessions SET lease_expires_at=clock_timestamp()-interval '1 second',recording_state='recording' WHERE id=$1`, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = a.renew(ctx, r); !errors.Is(err, errFenced) {
			t.Fatal("expired lease resurrected")
		}
		if err = a.finish(ctx, r, sessionEnd{Status: "killed", RecordingState: "ready"}); !errors.Is(err, errFenced) {
			t.Fatal("expired owner finalized recording")
		}
		if _, err = b.requestKill(ctx, r.ID, 99); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("expired owner accepted a kill request")
		}
		if err = b.reap(ctx); err != nil {
			t.Fatal(err)
		}
		state, err := b.killState(ctx, r.ID, request.RequestID)
		if err != nil || state.Status != "failed" || state.killed() || state.Acknowledged {
			t.Fatalf("stale recovery fabricated kill: %+v %v", state, err)
		}
		var recording string
		if err = db.QueryRow(ctx, `SELECT recording_state FROM bastion_sessions WHERE id=$1`, r.ID).Scan(&recording); err != nil || recording != "failed" {
			t.Fatal("stale partial recording advertised ready")
		}
		var delivery string
		if err = db.QueryRow(ctx, `SELECT delivery_state FROM bastion_commands WHERE id=$1`, commandID).Scan(&delivery); err != nil || delivery != "unknown" {
			t.Fatalf("stale command outcome was not marked unknown: %q %v", delivery, err)
		}
	})
	t.Run("normal completion racing a request is not observed kill", func(t *testing.T) {
		r := insertTestSession(t, db, "normal-owner")
		var commandID int64
		if err := db.QueryRow(ctx, `INSERT INTO bastion_commands
			(session_id,command,action,delivery_state) VALUES ($1,'public-marker','confirmed','pending') RETURNING id`, r.ID).Scan(&commandID); err != nil {
			t.Fatal(err)
		}
		request, err := b.requestKill(ctx, r.ID, 99)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.finish(ctx, r, sessionEnd{Status: "closed"}); err != nil {
			t.Fatal(err)
		}
		state, err := b.killState(ctx, r.ID, request.RequestID)
		if err != nil || state.Acknowledged || state.killed() {
			t.Fatal("normal completion fabricated a kill")
		}
		var delivery string
		if err = db.QueryRow(ctx, `SELECT delivery_state FROM bastion_commands WHERE id=$1`, commandID).Scan(&delivery); err != nil || delivery != "unknown" {
			t.Fatalf("unfinished command outcome was not marked unknown: %q %v", delivery, err)
		}
	})
	t.Run("session row mapping includes new columns", func(t *testing.T) {
		m := New()
		m.d = &app.Deps{DB: db}
		s, err := queryOne[Session](ctx, m, `SELECT * FROM bastion_sessions WHERE id=$1`, refB.ID)
		if err != nil || s.OwnerID != refB.Owner || s.FenceToken != refB.Fence {
			t.Fatalf("session mapping failed: %v", err)
		}
	})
}

func TestPostgresCommandAuditDeliveryAndFencing(t *testing.T) {
	db := bastionTestDB(t)
	m := New()
	m.d = &app.Deps{DB: db}
	ctx := context.Background()
	r := insertTestSession(t, db, m.owner)
	ids, err := m.auditCommands(ctx, r, []CommandEvent{
		{Command: "printf public-marker", Action: "allow"},
		{Command: "printf blocked-marker", Action: "deny"},
		{Command: "public-marker", Action: "cancelled"},
	})
	if err != nil || len(ids) != 1 {
		t.Fatalf("command audit failed: %v", err)
	}
	if err = m.commandDelivery(ctx, r, ids, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := queryAll[Command](ctx, m, `SELECT * FROM bastion_commands WHERE session_id=$1 ORDER BY id`, r.ID)
	if err != nil || len(rows) != 3 || rows[0].DeliveryState != "forwarded" ||
		rows[1].DeliveryState != "blocked" || rows[2].DeliveryState != "cancelled" {
		t.Fatalf("audit delivery states incorrect: %+v %v", rows, err)
	}
	ids, err = m.auditCommands(ctx, r, []CommandEvent{{Command: "printf uncertain-marker", Action: "confirmed"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.commandDelivery(ctx, r, ids, io.ErrShortWrite); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = db.QueryRow(ctx, `SELECT delivery_state FROM bastion_commands WHERE id=$1`, ids[0]).Scan(&state); err != nil || state != "write_failed" {
		t.Fatal("write failure became execution success")
	}
	wrong := r
	wrong.Fence = newToken()
	if _, err = m.auditCommands(ctx, wrong, []CommandEvent{{Command: "printf forbidden-marker", Action: "allow"}}); err == nil {
		t.Fatal("foreign fence inserted command audit")
	}
	store := &pgLifecycleStore{db: db}
	if _, err = store.requestKill(ctx, r.ID, 99); err != nil {
		t.Fatal(err)
	}
	if _, err = m.auditCommands(ctx, r, []CommandEvent{{Command: "printf late-marker", Action: "allow"}}); err == nil {
		t.Fatal("input admitted after kill request")
	}
	gin.SetMode(gin.TestMode)
}
