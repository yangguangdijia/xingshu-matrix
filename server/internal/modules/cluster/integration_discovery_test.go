package cluster

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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

func TestIntegrationDiscoveryMigrationAndState(t *testing.T) {
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 3 || migrations[2].Version != 3 {
		t.Fatalf("missing cluster discovery migration: %+v", migrations)
	}
	sql := migrations[2].SQL
	for _, required := range []string{"cluster_id", "PRIMARY KEY REFERENCES cluster_clusters(id) ON DELETE CASCADE",
		"cluster_updated_at", "lease_until", "generation", "snapshot", "INSERT INTO cluster_integration_discovery"} {
		if !strings.Contains(sql, required) {
			t.Errorf("migration missing %q", required)
		}
	}
	if delay := integrationRetryDelay(1); delay != time.Minute {
		t.Fatalf("first retry %s", delay)
	}
	if delay := integrationRetryDelay(10); delay != 32*time.Minute {
		t.Fatalf("bounded retry %s", delay)
	}
	for _, tc := range []struct {
		result integrationCandidateResult
		state  string
	}{
		{integrationCandidateResult{Sources: map[string]networkReadStatus{"services": {Available: true}}}, "ready"},
		{integrationCandidateResult{Truncated: true}, "partial"},
		{integrationCandidateResult{Sources: map[string]networkReadStatus{"services": {State: "forbidden"}}}, "partial"},
	} {
		state, _ := integrationResultState(tc.result)
		if state != tc.state {
			t.Fatalf("got state %q want %q", state, tc.state)
		}
	}
}

func TestIntegrationDiscoveryRoutesRequirePermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path string
		perms        []string
	}{
		{http.MethodGet, "/api/v1/clusters/3/integration-candidates", nil},
		{http.MethodGet, "/api/v1/clusters/3/integration-discovery", []string{PermWorkloadRead}},
		{http.MethodGet, "/api/v1/clusters/3/integration-discovery/status", []string{PermClusterRead}},
		{http.MethodGet, "/api/v1/clusters/3/integration-discovery/bindings", []string{PermClusterRead}},
		{http.MethodPost, "/api/v1/clusters/3/integration-discovery/refresh", []string{PermClusterRead, PermWorkloadRead}},
	} {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Permissions: tc.perms})
			c.Next()
		})
		New().Register(router.Group("/api/v1"), &app.Deps{})
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, httptest.NewRequest(tc.method, tc.path, nil))
		if resp.Code != http.StatusForbidden {
			t.Errorf("%s %s perms=%v: got %d, want 403", tc.method, tc.path, tc.perms, resp.Code)
		}
	}
}

// MATRIX_TEST_DATABASE_URL must point to a disposable test database. The test
// creates and drops only its own uniquely named schema; it never touches live
// cluster credentials or observability tables.
func TestIntegrationDiscoverySQLLifecycle(t *testing.T) {
	url := integrationTestDatabaseURL()
	if url == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run PostgreSQL lifecycle integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	pool, cleanup := integrationIsolatedPool(t, ctx, url)
	defer cleanup()
	m := New()
	m.d = &app.Deps{DB: pool}
	var a, b int64
	for _, row := range []struct {
		id   *int64
		name string
	}{{&a, "a"}, {&b, "b"}} {
		if err := pool.QueryRow(ctx, `INSERT INTO cluster_clusters (name,env,kubeconfig) VALUES ($1,'prod','encrypted') RETURNING id`, row.name).Scan(row.id); err != nil {
			t.Fatal(err)
		}
		if err := m.enqueueIntegrationDiscovery(ctx, *row.id); err != nil {
			t.Fatal(err)
		}
	}
	if a == b {
		t.Fatal("same-environment clusters share an id")
	}
	if err := m.enqueueIntegrationDiscovery(ctx, a); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cluster_integration_discovery`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate jobs or lost cluster: count=%d err=%v", count, err)
	}
	first, err := m.claimIntegrationDiscovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClusterID != a {
		t.Fatalf("claim order: %+v", first)
	}
	if err := m.enqueueIntegrationDiscovery(ctx, a); err != nil {
		t.Fatal(err)
	}
	tag, err := pool.Exec(ctx, `UPDATE cluster_integration_discovery SET status='ready' WHERE cluster_id=$1 AND generation=$2`, a, first.Generation)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatal("old worker overwrote an invalidated result")
	}
	if _, err := pool.Exec(ctx, `UPDATE cluster_clusters SET updated_at=now()+interval '1 second' WHERE id=$1`, a); err != nil {
		t.Fatal(err)
	}
	stale, err := m.loadIntegrationDiscovery(ctx, a)
	if err != nil || stale.Status != "pending" || len(stale.Result.Items) != 0 {
		t.Fatalf("stale snapshot exposed before reconciliation: %+v %v", stale, err)
	}
	if err := m.reconcileIntegrationDiscovery(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM cluster_integration_discovery WHERE cluster_id=$1`, a).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("changed cluster not invalidated: %s %v", status, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM cluster_clusters WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cluster_integration_discovery WHERE cluster_id=$1`, b).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted cluster retains discovery: %d %v", count, err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 7,
			Permissions: []string{PermClusterRead, PermWorkloadRead}, EnvScope: []string{"dev"}})
		c.Next()
	})
	m.Register(router.Group("/api/v1"), &app.Deps{DB: pool})
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/clusters/%d/integration-discovery", a), nil))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("same-environment discovery escaped principal scope: %d", resp.Code)
	}
}

func integrationTestDatabaseURL() string { return os.Getenv("MATRIX_TEST_DATABASE_URL") }

func integrationIsolatedPool(t *testing.T, ctx context.Context, url string) (*pgxpool.Pool, func()) {
	t.Helper()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("matrix_discovery_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		admin.Close()
	}
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if _, err := pool.Exec(ctx, migration.SQL); err != nil {
			cleanup()
			t.Fatalf("migration %d: %v", migration.Version, err)
		}
	}
	return pool, cleanup
}
