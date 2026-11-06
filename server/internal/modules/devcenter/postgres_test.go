package devcenter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 真实 PostgreSQL 集成测试：设置 MATRIX_TEST_DATABASE_URL（可丢弃的测试库）后运行，否则跳过。
func devTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run devcenter PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "devcenter_test_" + hex.EncodeToString(b)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal("cannot create test schema")
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = admin.Exec(c, "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open test pool")
	}
	t.Cleanup(pool.Close)
	names, _ := fs.Glob(migFS, "migrations/*.sql")
	for _, n := range names {
		body, _ := migFS.ReadFile(n)
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s: %v", n, err)
		}
	}
	return pool
}

func TestDevcenterFlowPostgres(t *testing.T) {
	db := devTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	user := "alice"
	r.Use(func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Username: user, Permissions: []string{"devcenter:read", "devcenter:manage"}})
		c.Next()
	})
	m := New()
	m.Register(r.Group(""), &app.Deps{DB: db})
	do := func(method, path string, body any) (int, map[string]any) {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var env map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		d, _ := env["data"].(map[string]any)
		return w.Code, d
	}
	code, it := do("POST", "/devcenter/iterations", map[string]any{"title": "迭代 1"})
	if code != 201 {
		t.Fatalf("create iteration %d", code)
	}
	iterID := int64(it["id"].(float64))
	code, rq := do("POST", "/devcenter/requirements", map[string]any{"title": "SSO", "iteration_id": iterID, "assignee": "bob"})
	if code != 201 {
		t.Fatalf("create requirement %d", code)
	}
	id := strconv.FormatInt(int64(rq["id"].(float64)), 10)
	if code, _ := do("PATCH", "/devcenter/requirements/"+id, map[string]any{"status": "doing"}); code != 200 {
		t.Fatalf("patch %d", code)
	}
	_, acts := do("GET", "/devcenter/requirements/"+id+"/activities", nil)
	if n := len(acts["items"].([]any)); n != 3 {
		t.Fatalf("want create+assign+status activities, got %d", n)
	}
	user = "carol"
	if code, _ := do("POST", "/devcenter/requirements/"+id+"/comments", map[string]any{"body": "看一下回调"}); code != 201 {
		t.Fatalf("comment %d", code)
	}
	_, inv := do("GET", "/devcenter/requirements?involved=me", nil)
	if inv["total"].(float64) != 1 {
		t.Fatalf("carol involved via comment: %v", inv["total"])
	}
	if code, _ := do("POST", "/devcenter/requirements/"+id+"/links", map[string]any{"ref_type": "app", "ref_id": 5, "ref_name": "web"}); code != 201 {
		t.Fatalf("link %d", code)
	}
	if code, _ := do("POST", "/devcenter/requirements/"+id+"/links", map[string]any{"ref_type": "app", "ref_id": 5, "ref_name": "web"}); code != 409 {
		t.Fatalf("duplicate link must conflict, got %d", code)
	}
	_, links := do("GET", "/devcenter/links?ref_type=app&ref_id=5", nil)
	if l := links["items"].([]any); len(l) != 1 || l[0].(map[string]any)["item_title"] != "SSO" {
		t.Fatalf("reverse links %v", links)
	}
	if code, _ := do("PATCH", "/devcenter/requirements/"+id, map[string]any{"status": "done"}); code != 200 {
		t.Fatalf("done %d", code)
	}
	code, st := do("GET", "/devcenter/stats?days=30", nil)
	if code != 200 {
		t.Fatalf("stats %d", code)
	}
	rep := st["report"].(map[string]any)
	if rep["throughput"].(map[string]any)["requirements"].(float64) != 1 || rep["cycle_time"].(map[string]any)["requirements"].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("stats report %v", rep)
	}
	// AI：未启用 AI 服务时给出可读原因，发起返回 400；已生成的方案可创建为子任务且不重复创建
	if code, s := do("GET", "/devcenter/ai/status", nil); code != 200 || s["available"] != false || s["reason"] == "" {
		t.Fatalf("ai status %d %v", code, s)
	}
	var taskID int64
	if err := db.QueryRow(context.Background(), `INSERT INTO devcenter_ai_tasks(requirement_id,status,result) VALUES($1,'succeeded','{"summary":"s","subtasks":[{"title":"后端","priority":"P1"},{"title":"前端","priority":"P2"}]}') RETURNING id`, id).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	tid := strconv.FormatInt(taskID, 10)
	if code, r := do("POST", "/devcenter/ai-tasks/"+tid+"/apply", map[string]any{"indexes": []int{0, 1}}); code != 200 || len(r["items"].([]any)) != 2 {
		t.Fatalf("apply %d %v", code, r)
	}
	if code, r := do("POST", "/devcenter/ai-tasks/"+tid+"/apply", map[string]any{"indexes": []int{0}}); code != 200 || len(r["items"].([]any)) != 0 {
		t.Fatalf("re-apply must not duplicate %d %v", code, r)
	}
	if _, kids := do("GET", "/devcenter/requirements?parent_id="+id+"&type=task", nil); kids["total"].(float64) != 2 {
		t.Fatalf("children %v", kids["total"])
	}
	if code, _ := do("GET", "/devcenter/iterations/"+strconv.FormatInt(iterID, 10)+"/burndown", nil); code != 200 {
		t.Fatalf("burndown %d", code)
	}
	if code, _ := do("DELETE", "/devcenter/iterations/"+strconv.FormatInt(iterID, 10), nil); code != 400 {
		t.Fatalf("iteration delete without confirm must fail, got %d", code)
	}
	if code, _ := do("DELETE", "/devcenter/iterations/"+strconv.FormatInt(iterID, 10)+"?confirm="+"%E8%BF%AD%E4%BB%A3%201", nil); code != 200 {
		t.Fatalf("iteration delete with confirm %d", code)
	}
	if code, _ := do("DELETE", "/devcenter/requirements/"+id, nil); code != 200 {
		t.Fatalf("delete %d", code)
	}
	var n int
	_ = db.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM devcenter_comments)+(SELECT count(*) FROM devcenter_activities WHERE kind='requirements' AND item_id=$1)+(SELECT count(*) FROM devcenter_links)+(SELECT count(*) FROM devcenter_requirements WHERE data ? 'parent_id')", id).Scan(&n)
	if n != 0 {
		t.Fatalf("orphans left: %d", n)
	}
}

// 0003 在已有数据的库上执行时，为历史需求 / 缺陷补齐创建与状态记录。
func TestMigrationBackfillPostgres(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MATRIX_TEST_DATABASE_URL to run devcenter PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "devcenter_mig_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	run := func(n string) {
		b, _ := migFS.ReadFile("migrations/" + n)
		if _, err := db.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	run("0001_init.up.sql")
	run("0002_fields.up.sql")
	if _, err := db.Exec(ctx, "INSERT INTO devcenter_requirements(title,status,created_by) VALUES('a','todo',1),('b','done',1); INSERT INTO devcenter_bugs(title,status,created_by) VALUES('c','closed',1)"); err != nil {
		t.Fatal(err)
	}
	run("0003_collab.up.sql")
	var n int
	_ = db.QueryRow(ctx, "SELECT count(*) FROM devcenter_activities").Scan(&n)
	if n != 5 {
		t.Fatalf("want 3 create + 2 status backfill rows, got %d", n)
	}
}
