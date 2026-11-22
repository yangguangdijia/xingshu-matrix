package devcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 价值流 PostgreSQL 集成测试（设置 MATRIX_TEST_DATABASE_URL 后运行）。cicd 表按 cicd 迁移的相关列最小化建表。
func TestTracePostgres(t *testing.T) {
	db := devTestDB(t)
	ctx := context.Background()
	// 迁移 0004：旧关联表已更名，新表为价值流结构
	var repo, vs bool
	_ = db.QueryRow(ctx, "SELECT to_regclass('devcenter_repo_links') IS NOT NULL").Scan(&repo)
	_ = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='devcenter_links' AND column_name='item_type')").Scan(&vs)
	if !repo || !vs {
		t.Fatalf("migration 0004: repo_links=%v value_stream=%v", repo, vs)
	}
	for _, q := range []string{
		`CREATE TABLE cicd_applications (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, image_repo TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE cicd_pipelines (id BIGSERIAL PRIMARY KEY, app_id BIGINT NOT NULL, name TEXT NOT NULL)`,
		`CREATE TABLE cicd_runs (id BIGSERIAL PRIMARY KEY, pipeline_id BIGINT NOT NULL, app_id BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'queued',
			triggered_by TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', commit_sha TEXT NOT NULL DEFAULT '', image_tag TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, commits JSONB NOT NULL DEFAULT '[]')`,
		`CREATE TABLE cicd_releases (id BIGSERIAL PRIMARY KEY, app_id BIGINT NOT NULL, env TEXT NOT NULL, run_id BIGINT, image TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'deploy', status TEXT NOT NULL DEFAULT 'running', created_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), finished_at TIMESTAMPTZ)`,
		`CREATE TABLE cicd_webhook_deliveries (id BIGSERIAL PRIMARY KEY, app_id BIGINT NOT NULL, ref TEXT NOT NULL DEFAULT '', commit_sha TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'processing', runs JSONB NOT NULL DEFAULT '[]', received_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO devcenter_requirements(id,title,status,created_by) VALUES (1,'单点登录','doing',1)`,
		`INSERT INTO devcenter_bugs(id,title,status,created_by) VALUES (1,'登录页空指针','fixed',1)`,
		`INSERT INTO cicd_applications(id,name,image_repo) VALUES (1,'web','reg/web')`,
		`INSERT INTO cicd_pipelines(id,app_id,name) VALUES (1,1,'构建镜像')`,
		`INSERT INTO cicd_runs(id,pipeline_id,app_id,status,branch,commit_sha,image_tag,finished_at) VALUES (1,1,1,'succeeded','main','0000aaaa','v0',now() - interval '2 hour')`,
		`INSERT INTO cicd_releases(id,app_id,env,run_id,image,status,finished_at) VALUES (1,1,'prod',1,'reg/web:v0','succeeded',now() - interval '2 hour')`,
		`INSERT INTO cicd_runs(id,pipeline_id,app_id,status,branch,commit_sha,image_tag,finished_at,commits) VALUES
			(2,1,1,'succeeded','feature/REQ-1-sso','aaaa1111bbbb','v1',now(), '[{"id":"aaaa1111bbbb","message":"feat: sso REQ-1"},{"id":"cccc2222","message":"chore: deps"}]')`,
		`INSERT INTO cicd_releases(id,app_id,env,run_id,image,status,finished_at) VALUES (2,1,'prod',2,'reg/web:v1','succeeded',now())`,
		`INSERT INTO cicd_webhook_deliveries(app_id,ref,commit_sha,status) VALUES (1,'refs/heads/bugfix/BUG-1','dddd3333','ignored')`,
		`INSERT INTO cicd_runs(id,pipeline_id,app_id,status,branch,commit_sha) VALUES (3,1,1,'failed','main','dddd3333')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Username: "alice", Permissions: []string{"devcenter:read", "devcenter:manage"}})
		c.Next()
	})
	m := &Module{d: &app.Deps{DB: db}}
	pass := func(c *gin.Context) { c.Next() }
	m.registerTrace(r.Group("/devcenter"), pass, pass)
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
	stageState := func(d map[string]any, key string) string {
		for _, s := range d["stages"].([]any) {
			st := s.(map[string]any)
			if st["key"] == key {
				return st["state"].(string)
			}
		}
		return ""
	}

	// 全量同步：分支名 REQ-1 → run 2 + 提交；run 2 → release 2；特性分支推送 BUG-1 → 提交 dddd3333 → run 3（失败）
	if err := m.syncTraceLinks(ctx, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if err := m.syncTraceLinks(ctx, nil); err != nil { // 幂等
		t.Fatalf("sweep again: %v", err)
	}
	var n int
	_ = db.QueryRow(ctx, "SELECT count(*) FROM devcenter_links WHERE item_type='requirement' AND item_id=1").Scan(&n)
	if n != 3 { // run 2, commit aaaa1111bbbb, release 2
		t.Fatalf("requirement links = %d", n)
	}
	code, d := do("GET", "/devcenter/requirements/1/trace", nil)
	if code != 200 {
		t.Fatalf("trace %d", code)
	}
	for k, want := range map[string]string{"code": "done", "build": "done", "artifact": "done", "release": "done", "env": "done"} {
		if got := stageState(d, k); got != want {
			t.Errorf("requirement stage %s = %s, want %s", k, got, want)
		}
	}
	if envs := d["envs"].([]any); len(envs) != 1 || envs[0].(map[string]any)["env"] != "prod" {
		t.Fatalf("envs %v", d["envs"])
	}
	_, d = do("GET", "/devcenter/bugs/1/trace", nil)
	if stageState(d, "code") != "done" || stageState(d, "build") != "failed" || stageState(d, "release") != "none" {
		t.Fatalf("bug trace %v", d["stages"])
	}

	// 手工关联：重复 409；自动关联不能删除；手工关联可删除
	code, l := do("POST", "/devcenter/bugs/1/links", map[string]any{"kind": "release", "ref": "2"})
	if code != 201 || l["source"] != "manual" || l["env"] != "prod" {
		t.Fatalf("manual link %d %v", code, l)
	}
	if code, _ := do("POST", "/devcenter/bugs/1/links", map[string]any{"kind": "release", "ref": "2"}); code != 409 {
		t.Fatalf("duplicate link want 409, got %d", code)
	}
	var autoID int64
	_ = db.QueryRow(ctx, "SELECT id FROM devcenter_links WHERE source='auto' LIMIT 1").Scan(&autoID)
	if code, _ := do("DELETE", "/devcenter/links/"+strconv.FormatInt(autoID, 10), nil); code != 409 {
		t.Fatalf("delete auto link want 409, got %d", code)
	}
	if code, _ := do("DELETE", "/devcenter/links/"+strconv.FormatInt(int64(l["id"].(float64)), 10), nil); code != 200 {
		t.Fatalf("delete manual link %d", code)
	}
	if code, _ := do("POST", "/devcenter/requirements/999/links", map[string]any{"kind": "run", "ref": "2"}); code != 404 {
		t.Fatalf("link to missing item want 404, got %d", code)
	}

	// 发布包含的条目与发布说明
	code, d = do("GET", "/devcenter/releases/2/items", nil)
	if code != 200 {
		t.Fatalf("release items %d", code)
	}
	if reqs := d["requirements"].([]any); len(reqs) != 1 || reqs[0].(map[string]any)["id"].(float64) != 1 {
		t.Fatalf("release requirements %v", d["requirements"])
	}
	if cs := d["commits"].([]any); len(cs) != 2 {
		t.Fatalf("release commits %v", d["commits"])
	}
	md := d["notes_md"].(string)
	for _, frag := range []string{"### 新功能", "REQ-1 单点登录", "### 其他提交", "chore: deps", "上一次成功发布 #1"} {
		if !strings.Contains(md, frag) {
			t.Errorf("notes missing %q:\n%s", frag, md)
		}
	}
	if code, _ := do("GET", "/devcenter/releases/999/items", nil); code != 404 {
		t.Fatalf("missing release want 404, got %d", code)
	}
}
