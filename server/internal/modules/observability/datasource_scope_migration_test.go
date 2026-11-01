package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

func TestLegacyDatasourceScopeMigrationIntent(t *testing.T) {
	migration := legacyDatasourceScopeMigration(t)
	if migration.Version != 3 || migration.Name != "legacy_datasource_scope" {
		t.Fatalf("unexpected migration identity: %d_%s", migration.Version, migration.Name)
	}

	sql := strings.ToLower(migration.SQL)
	for _, required := range []string{
		"jsonb_typeof(ds.options) = 'object'",
		"ds.url ~* '^https?://[^[:space:]/?#]+'",
		"ds.url !~ '[[:space:]]'",
		"nullif(btrim(ds.options->>'scope'), '') is null",
		"nullif(btrim(ds.options->>'cluster_id'), '') is null",
		"nullif(btrim(ds.options->>'cluster_name'), '') is null",
		"'scope', 'cluster'",
		"'cluster_id', cl.id::text",
		"'cluster_name', cl.name",
		"other_cluster.env = ds.env",
		"rtrim(sibling.url, '/') = rtrim(ds.url, '/')",
		"'cluster_values'",
		"'shared'",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("migration is missing safety condition %q", required)
		}
	}
	if strings.Contains(sql, "secret") {
		t.Fatal("migration must not read or write datasource secrets")
	}
}

func TestLegacyDatasourceScopeMigrationAndResolution(t *testing.T) {
	pool := newDatasourceScopeTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migration := legacyDatasourceScopeMigration(t)
	createDatasourceScopeTestSchema(t, ctx, pool)

	devA := insertScopeTestCluster(t, ctx, pool, "dev-a", "dev")
	devB := insertScopeTestCluster(t, ctx, pool, "dev-b", "dev")
	prod := insertScopeTestCluster(t, ctx, pool, "prod-a", "prod")
	insertScopeTestCluster(t, ctx, pool, "qa-a", "qa")
	insertScopeTestCluster(t, ctx, pool, "empty-env", "")
	insertScopeTestCluster(t, ctx, pool, "stage-a", "stage")

	cipher, err := matrixcrypto.New(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-datasource-token"
	encryptedSecret, err := cipher.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}

	insertScopeTestDatasource(t, ctx, pool, "ambiguous-dev", "loki", "dev", "http://loki.dev", encryptedSecret, `{}`)
	insertScopeTestDatasource(t, ctx, pool, "safe-prod", "prometheus", "prod", "http://prometheus.prod",
		"", `{"index":"metrics-*","scope":" "}`)
	insertScopeTestDatasource(t, ctx, pool, "shared-mapping", "tempo", "prod", "http://tempo.prod", "",
		fmt.Sprintf(`{"cluster_label":"cluster","cluster_values":{"%d":"prod-a"}}`, prod))
	insertScopeTestDatasource(t, ctx, pool, "shared-flag", "jaeger", "prod", "http://jaeger.prod", "", `{"shared":true}`)
	insertScopeTestDatasource(t, ctx, pool, "invalid-options", "elasticsearch", "prod", "http://es.prod", "", `null`)
	insertScopeTestDatasource(t, ctx, pool, "explicit-owner", "alertmanager", "prod", "http://am.prod", "",
		fmt.Sprintf(`{"cluster_id":"%d","cluster_name":"prod-a"}`, prod))
	insertScopeTestDatasource(t, ctx, pool, "unknown-scope", "loki", "prod", "http://loki.prod", "", `{"scope":"unknown"}`)
	insertScopeTestDatasource(t, ctx, pool, "empty-env", "tempo", "", "http://tempo.empty", "", `{}`)
	insertScopeTestDatasource(t, ctx, pool, "qa-source-a", "prometheus", "qa", "http://shared.qa", "", `{}`)
	insertScopeTestDatasource(t, ctx, pool, "qa-source-b", "loki", "qa", "http://shared.qa", "", `{}`)
	insertScopeTestDatasource(t, ctx, pool, "shared-url-a", "prometheus", "stage", "http://shared.stage/", "", `{}`)
	insertScopeTestDatasource(t, ctx, pool, "shared-url-b", "loki", "stage", "http://shared.stage", "", `{}`)
	insertScopeTestDatasource(t, ctx, pool, "invalid-url", "tempo", "stage", "not a URL", "", `{}`)

	unchanged := map[string][]byte{}
	for _, name := range []string{
		"ambiguous-dev",
		"shared-mapping",
		"shared-flag",
		"invalid-options",
		"explicit-owner",
		"unknown-scope",
		"empty-env",
		"qa-source-a",
		"qa-source-b",
		"shared-url-a",
		"shared-url-b",
		"invalid-url",
	} {
		var options []byte
		if err := pool.QueryRow(ctx, `SELECT options FROM obs_datasources WHERE name=$1`, name).Scan(&options); err != nil {
			t.Fatal(err)
		}
		unchanged[name] = append([]byte(nil), options...)
	}

	testFS := fstest.MapFS{
		"0003_legacy_datasource_scope.up.sql": &fstest.MapFile{Data: []byte(migration.SQL)},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, pool, logger, store.MigrationSource{Module: "observability", FS: testFS}); err != nil {
		t.Fatalf("apply scope migration: %v", err)
	}
	if err := store.Migrate(ctx, pool, logger, store.MigrationSource{Module: "observability", FS: testFS}); err != nil {
		t.Fatalf("repeat scope migration: %v", err)
	}
	tag, err := pool.Exec(ctx, migration.SQL)
	if err != nil {
		t.Fatalf("repeat migration SQL: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("migration SQL was not idempotent: second run changed %d rows", tag.RowsAffected())
	}

	assertOptions := func(name string, wantScope string, wantClusterID int64, wantClusterName string) map[string]any {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT options FROM obs_datasources WHERE name=$1`, name).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var opts map[string]any
		if string(raw) == "null" {
			if wantScope != "" {
				t.Fatalf("%s options unexpectedly null", name)
			}
			return nil
		}
		if err := json.Unmarshal(raw, &opts); err != nil {
			t.Fatalf("%s options are invalid JSON", name)
		}
		if got := optionString(opts, "scope"); got != wantScope {
			t.Errorf("%s scope = %q, want %q", name, got, wantScope)
		}
		if wantClusterID > 0 {
			if got := optionString(opts, "cluster_id"); got != strconv.FormatInt(wantClusterID, 10) {
				t.Errorf("%s cluster_id = %q, want %d", name, got, wantClusterID)
			}
			if got := optionString(opts, "cluster_name"); got != wantClusterName {
				t.Errorf("%s cluster_name = %q, want %q", name, got, wantClusterName)
			}
		}
		return opts
	}

	safeOptions := assertOptions("safe-prod", "cluster", prod, "prod-a")
	if safeOptions["index"] != "metrics-*" {
		t.Fatalf("migration discarded existing datasource options")
	}
	for _, name := range []string{
		"ambiguous-dev",
		"shared-mapping",
		"shared-flag",
		"explicit-owner",
		"empty-env",
		"qa-source-a",
		"qa-source-b",
		"shared-url-a",
		"shared-url-b",
		"invalid-url",
	} {
		assertOptions(name, "", 0, "")
	}
	assertOptions("unknown-scope", "unknown", 0, "")
	for name, before := range unchanged {
		var after []byte
		if err := pool.QueryRow(ctx, `SELECT options FROM obs_datasources WHERE name=$1`, name).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Errorf("migration changed ineligible datasource %q options", name)
		}
	}
	var preservedSecret string
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM obs_datasources WHERE name='ambiguous-dev'`).Scan(&preservedSecret); err != nil {
		t.Fatal(err)
	}
	if preservedSecret != encryptedSecret {
		t.Fatal("migration changed an encrypted datasource secret")
	}

	m := New()
	m.d = &app.Deps{DB: pool, Crypto: cipher}
	principal := &rbac.Principal{UserID: 1}
	selected, scope, err := m.selectSource(ctx, principal, "prometheus", strconv.FormatInt(prod, 10))
	if err != nil {
		t.Fatalf("select migrated source: %v", err)
	}
	if selected.Name != "safe-prod" || scope.Cluster.ID != prod || scope.Central {
		t.Fatalf("wrong source selected for prod cluster: source=%q scope=%+v", selected.Name, scope)
	}
	for _, id := range []int64{devA, devB} {
		_, _, err := m.selectSource(ctx, principal, "loki", strconv.FormatInt(id, 10))
		var apiErr *httpx.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Errorf("unowned source was reused for cluster %d: %v", id, err)
		}
	}

	gin.SetMode(gin.TestMode)
	resolveRouter := gin.New()
	resolveRouter.GET("/resolve", func(c *gin.Context) {
		rbac.SetPrincipal(c, principal)
		m.resolveDS(c)
	})
	resolveResponse := httptest.NewRecorder()
	resolveRouter.ServeHTTP(resolveResponse, httptest.NewRequest(http.MethodGet,
		"/resolve?type=prometheus&cluster_id="+strconv.FormatInt(prod, 10), nil))
	if resolveResponse.Code != http.StatusOK {
		t.Fatalf("resolveDS status %d", resolveResponse.Code)
	}
	var resolved struct {
		Code int `json:"code"`
		Data struct {
			Datasource DataSource `json:"datasource"`
			ClusterID  int64      `json:"cluster_id"`
			Scope      string     `json:"scope"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resolveResponse.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Code != 0 || resolved.Data.Datasource.Name != "safe-prod" ||
		resolved.Data.ClusterID != prod || resolved.Data.Scope != "cluster" {
		t.Fatalf("resolveDS returned the wrong cluster source: code=%d name=%q cluster=%d scope=%q",
			resolved.Code, resolved.Data.Datasource.Name, resolved.Data.ClusterID, resolved.Data.Scope)
	}

	listRouter := gin.New()
	listRouter.GET("/datasources", func(c *gin.Context) {
		rbac.SetPrincipal(c, principal)
		m.listDS(c)
	})
	listResponse := httptest.NewRecorder()
	listRouter.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/datasources", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("listDS status %d", listResponse.Code)
	}
	if bytes.Contains(listResponse.Body.Bytes(), []byte(secret)) {
		t.Fatal("datasource list exposed a secret")
	}
	var listed struct {
		Code int          `json:"code"`
		Data []DataSource `json:"data"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	foundSafe := false
	foundSecret := false
	for _, ds := range listed.Data {
		switch ds.Name {
		case "safe-prod":
			foundSafe = optionString(ds.Options, "scope") == "cluster" &&
				optionString(ds.Options, "cluster_id") == strconv.FormatInt(prod, 10)
		case "ambiguous-dev":
			foundSecret = ds.Secret == matrixcrypto.Mask(secret)
		}
	}
	if !foundSafe {
		t.Fatal("datasource list did not return migrated cluster ownership")
	}
	if !foundSecret {
		t.Fatal("datasource list did not mask and preserve the legacy datasource secret")
	}
}

func legacyDatasourceScopeMigration(t *testing.T) store.Migration {
	t.Helper()
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version == 3 {
			return migration
		}
	}
	t.Fatal("observability migration 0003_legacy_datasource_scope is missing")
	return store.Migration{}
}

func newDatasourceScopeTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run PostgreSQL datasource scope tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("open disposable test database")
	}
	schema := "matrix_obs_scope_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal("create isolated datasource scope schema")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal("parse disposable test database configuration")
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatal("open isolated datasource scope schema")
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("drop isolated datasource scope schema")
		}
		admin.Close()
	})
	return pool
}

func createDatasourceScopeTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE TABLE cluster_clusters (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		env TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal("create isolated cluster table")
	}
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version >= 3 {
			break
		}
		if _, err := pool.Exec(ctx, migration.SQL); err != nil {
			t.Fatalf("create observability test schema for migration %d", migration.Version)
		}
	}
}

func insertScopeTestCluster(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, env string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO cluster_clusters(name,env) VALUES ($1,$2) RETURNING id`, name, env).Scan(&id); err != nil {
		t.Fatal("insert isolated cluster")
	}
	return id
}

func insertScopeTestDatasource(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name, typ, env, url, secret, options string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO obs_datasources(name,type,url,env,secret_enc,options)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb)`, name, typ, url, env, secret, options); err != nil {
		t.Fatal("insert isolated datasource")
	}
}
