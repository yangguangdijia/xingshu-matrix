package automation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func TestAnsibleRunCredentialPersistenceAndCancellation(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run PostgreSQL lifecycle integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("automation_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(ctx, `
		CREATE TABLE automation_workflows (id BIGINT PRIMARY KEY);
		CREATE TABLE automation_workflow_versions (
			workflow_id BIGINT NOT NULL, version INTEGER NOT NULL, steps JSONB NOT NULL, playbook TEXT NOT NULL,
			PRIMARY KEY(workflow_id, version)
		);
		CREATE TABLE automation_runs (
			id BIGSERIAL PRIMARY KEY, workflow_id BIGINT NOT NULL, version INTEGER NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('queued','awaiting_approval','running','cancelling','succeeded','failed','cancelled')),
			triggered_by BIGINT NOT NULL, env TEXT NOT NULL DEFAULT '', payload_enc TEXT NOT NULL DEFAULT '',
			approval_id BIGINT, logs JSONB NOT NULL DEFAULT '[]'::jsonb, attempt INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, error TEXT
		);
		INSERT INTO automation_workflows(id) VALUES (1);
		INSERT INTO automation_workflow_versions(workflow_id,version,steps,playbook) VALUES (1,1,'[]','');
	`)
	if err != nil {
		t.Fatal(err)
	}
	module := New()
	module.d = &app.Deps{DB: db}

	cipher, err := crypto.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	secret := `{"targets":[{"password":"integration-secret"}]}`
	var queuedID int64
	err = db.QueryRow(ctx, `INSERT INTO automation_runs(workflow_id,version,status,triggered_by,env,payload_enc)
		VALUES(1,1,'queued',7,'prod','') RETURNING id`).Scan(&queuedID)
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte(fmt.Sprintf("automation:run:%d", queuedID))
	encrypted, err := cipher.EncryptBytes([]byte(secret), aad)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted == "" || strings.Contains(encrypted, "integration-secret") {
		t.Fatal("run credentials must be encrypted before persistence")
	}
	if _, err := db.Exec(ctx, "UPDATE automation_runs SET payload_enc=$2 WHERE id=$1", queuedID, encrypted); err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := db.QueryRow(ctx, "SELECT payload_enc FROM automation_runs WHERE id=$1", queuedID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted == "" || strings.Contains(persisted, "integration-secret") {
		t.Fatal("credential persistence must store only ciphertext")
	}
	plain, err := cipher.DecryptBytes(persisted, aad)
	if err != nil || string(plain) != secret {
		t.Fatalf("persisted credentials could not be opened with the run-specific key: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/runs/:id/cancel", func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 7, Permissions: []string{"automation:run"}})
		module.cancel(c)
	})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/runs/%d/cancel", queuedID), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel response = %d: %s", response.Code, response.Body.String())
	}
	if _, err := module.claim(ctx); err != pgx.ErrNoRows {
		t.Fatalf("cancelled queued run was claimable: run=%+v err=%v", err, err)
	}
	assertRunState(t, ctx, db, queuedID, "cancelled", "")

	var runningID int64
	err = db.QueryRow(ctx, `INSERT INTO automation_runs(workflow_id,version,status,triggered_by,env,payload_enc)
		VALUES(1,1,'running',7,'prod',$1) RETURNING id`, encrypted).Scan(&runningID)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.recoverInterruptedRuns(ctx); err != nil {
		t.Fatal(err)
	}
	assertRunState(t, ctx, db, runningID, "failed", "")

	var terminalID int64
	err = db.QueryRow(ctx, `INSERT INTO automation_runs(workflow_id,version,status,triggered_by,env,payload_enc)
		VALUES(1,1,'running',7,'prod',$1) RETURNING id`, encrypted).Scan(&terminalID)
	if err != nil {
		t.Fatal(err)
	}
	module.finish(terminalID, "succeeded", "", `["Workflow completed successfully"]`)
	assertRunState(t, ctx, db, terminalID, "succeeded", "")
}

func assertRunState(t *testing.T, ctx context.Context, db *pgxpool.Pool, id int64, wantStatus, wantPayload string) {
	t.Helper()
	var status, payload string
	if err := db.QueryRow(ctx, "SELECT status,payload_enc FROM automation_runs WHERE id=$1", id).Scan(&status, &payload); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || payload != wantPayload {
		t.Fatalf("run %d state = %q, payload empty=%v; want %q, payload empty=%v", id, status, payload == "", wantStatus, wantPayload == "")
	}
}
