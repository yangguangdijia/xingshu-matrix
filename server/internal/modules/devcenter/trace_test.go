package devcenter

import (
	"bytes"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func TestParseItemKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []ItemKey
	}{
		{"", nil},
		{"REQ-12 登录", []ItemKey{{"requirement", 12}}},
		{"fix: [BUG-3] 空指针, refs #req-12", []ItemKey{{"bug", 3}, {"requirement", 12}}},
		{"feature/REQ-12-login", []ItemKey{{"requirement", 12}}},
		{"fix_bug-7", []ItemKey{{"bug", 7}}},
		{"Req-5 and REQ-5 again, req-005", []ItemKey{{"requirement", 5}}},
		{"XREQ-1 REQ-2abc REQ-3_x 9REQ-4", nil},
		{"REQ-0 BUG-", nil},
		{"REQ-99999999999999999999", nil}, // 超过 18 位不匹配
		{"(REQ-8)修复，BUG-9：回归", []ItemKey{{"requirement", 8}, {"bug", 9}}},
		{"merge branch 'feature/BUG-21' into main", []ItemKey{{"bug", 21}}},
	}
	for _, c := range cases {
		if got := ParseItemKeys(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseItemKeys(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	k := ItemKey{Type: "bug", ID: 3}
	if k.String() != "BUG-3" || k.Kind() != "bugs" || (ItemKey{Type: "requirement", ID: 1}).Kind() != "requirements" {
		t.Fatal("ItemKey helpers")
	}
	if itemTypeOfKind("requirements") != "requirement" || itemTypeOfKind("bugs") != "bug" || itemTypeOfKind("tests") != "" {
		t.Fatal("itemTypeOfKind")
	}
}

func TestTraceLinkInputCheck(t *testing.T) {
	ok := []traceLinkInput{
		{Kind: "commit", Ref: " ABCDEF1 "},
		{Kind: "MR", Ref: "!12"},
		{Kind: "run", Ref: "42"},
		{Kind: "release", Ref: "7", URL: "https://git.example.com/x"},
		{Kind: "alert", Ref: "fp-abc"},
	}
	for _, in := range ok {
		in := in
		if err := in.check(); err != nil {
			t.Errorf("%+v rejected: %v", in, err)
		}
	}
	in := traceLinkInput{Kind: "commit", Ref: "ABCDEF1"}
	_ = in.check()
	if in.Ref != "abcdef1" {
		t.Fatalf("sha must be lower-cased, got %q", in.Ref)
	}
	mr := traceLinkInput{Kind: "mr", Ref: "#5"}
	_ = mr.check()
	if mr.Ref != "5" {
		t.Fatalf("mr ref normalized, got %q", mr.Ref)
	}
	bad := []traceLinkInput{
		{Kind: "app", Ref: "1"},
		{Kind: "commit", Ref: "xyz"},
		{Kind: "commit", Ref: "abc12"},
		{Kind: "run", Ref: "0"},
		{Kind: "run", Ref: "1;drop"},
		{Kind: "release", Ref: "-1"},
		{Kind: "alert", Ref: "a b"},
		{Kind: "run", Ref: "1", URL: "javascript:alert(1)"},
		{Kind: "run", Ref: "1", Title: strings.Repeat("长", 201)},
	}
	for _, in := range bad {
		in := in
		if in.check() == nil {
			t.Errorf("%+v must be rejected", in)
		}
	}
}

func tptr(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func i64(v int64) *int64 { return &v }

func TestBuildTraceStages(t *testing.T) {
	empty := buildTraceStages(nil, nil, nil)
	if len(empty) != 5 {
		t.Fatalf("want 5 stages, got %d", len(empty))
	}
	for i, st := range empty {
		if st.Key != TraceStageKeys[i] || st.State != "none" || st.Items == nil || len(st.Items) != 0 {
			t.Fatalf("empty stage %d: %+v", i, st)
		}
	}
	links := []TraceLink{
		{ID: 1, Kind: "commit", Ref: "abc1234", OccurredAt: tptr("2026-11-20T10:00:00Z"), Source: "auto"},
		{ID: 2, Kind: "run", Ref: "10", Status: "failed", OccurredAt: tptr("2026-11-20T10:05:00Z")},
		{ID: 3, Kind: "run", Ref: "11", Status: "succeeded", OccurredAt: tptr("2026-11-20T11:00:00Z")},
		{ID: 4, Kind: "release", Ref: "5", Status: "running", Env: "prod", AppID: i64(1), OccurredAt: tptr("2026-11-20T12:00:00Z")},
		{ID: 5, Kind: "alert", Ref: "9", Status: "firing", OccurredAt: tptr("2026-11-20T13:00:00Z")},
	}
	arts := []traceArtifact{{RunID: 11, AppID: 1, Image: "reg/app:v1", Status: "succeeded", At: tptr("2026-11-20T11:00:00Z")}}
	envs := []TraceEnv{{Env: "test", AppID: 1, AppName: "web", ReleaseID: 4, Image: "reg/app:v1", At: tptr("2026-11-20T11:30:00Z")}}
	st := buildTraceStages(links, arts, envs)
	want := map[string]string{"code": "done", "build": "done", "artifact": "done", "release": "running", "env": "failed"}
	for _, s := range st {
		if s.State != want[s.Key] {
			t.Errorf("stage %s state %s, want %s", s.Key, s.State, want[s.Key])
		}
	}
	if st[1].Items[0].Ref != "11" || len(st[1].Items) != 2 {
		t.Fatalf("build items must be newest first: %+v", st[1].Items)
	}
	if st[4].Items[0].Kind != "env" || st[4].Items[1].Kind != "alert" {
		t.Fatalf("env stage items: %+v", st[4].Items)
	}
	// 告警已恢复时环境阶段为 done
	links[4].Status = "resolved"
	if s := buildTraceStages(links, arts, envs)[4]; s.State != "done" {
		t.Fatalf("resolved alert keeps env done, got %s", s.State)
	}
	// 只有构建失败
	s := buildTraceStages([]TraceLink{{ID: 1, Kind: "run", Ref: "1", Status: "cancelled"}}, nil, nil)
	if s[1].State != "failed" || s[0].State != "none" || s[4].State != "none" {
		t.Fatalf("failed build: %+v", s)
	}
	if traceState("run", "queued") != "running" || traceState("release", "succeeded") != "done" || traceState("alert", "open") != "failed" {
		t.Fatal("traceState mapping")
	}
}

func TestLinksFromRunAndDelivery(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	raw := []byte(`[{"id":"AAAA1111","message":"feat: 单点登录 REQ-12\n\n详细说明","author":{"name":"alice"},"url":"https://git/x/aaaa"},
		{"sha":"bbbb2222","message":"fix BUG-3 和 [BUG-3]","author":"bob"},{"sha":"cccc3333","message":"chore: deps"}]`)
	cs := parseTraceCommits(raw)
	if len(cs) != 3 || cs[0].sha() != "aaaa1111" || cs[0].Author != "alice" || cs[1].Author != "bob" {
		t.Fatalf("parse commits: %+v", cs)
	}
	if parseTraceCommits([]byte(`{}`)) != nil || parseTraceCommits(nil) != nil {
		t.Fatal("bad commits json must yield nil")
	}
	r := traceRun{ID: 77, AppID: 2, Status: "succeeded", Branch: "feature/REQ-12-sso", SHA: "AAAA1111", By: "ci", Pipeline: "构建镜像", At: at, Commits: cs}
	got := linksFromRun(r)
	type k struct{ key, kind, ref string }
	have := map[k]pendingTraceLink{}
	for _, p := range got {
		have[k{p.Key.String(), p.Kind, p.Ref}] = p
	}
	for _, w := range []k{{"REQ-12", "run", "77"}, {"REQ-12", "commit", "aaaa1111"}, {"BUG-3", "commit", "bbbb2222"}, {"BUG-3", "run", "77"}} {
		if _, ok := have[w]; !ok {
			t.Errorf("missing %v in %+v", w, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("want 4 deduped links, got %d: %+v", len(got), got)
	}
	if p := have[k{"REQ-12", "commit", "aaaa1111"}]; p.Title != "feat: 单点登录 REQ-12" || p.URL != "https://git/x/aaaa" || p.Author == "" {
		t.Fatalf("commit link fields: %+v", p)
	}
	if p := have[k{"REQ-12", "run", "77"}]; p.Title != "构建镜像 #77" || p.Status != "succeeded" || p.AppID != 2 {
		t.Fatalf("run link fields: %+v", p)
	}
	if len(linksFromRun(traceRun{ID: 1, Branch: "main", SHA: "abc"})) != 0 {
		t.Fatal("no keys, no links")
	}
	d := traceDelivery{AppID: 3, Ref: "refs/heads/bugfix/BUG-8", SHA: "DDDD4444", At: at}
	dl := linksFromDelivery(d)
	if len(dl) != 1 || dl[0].Key != (ItemKey{"bug", 8}) || dl[0].Kind != "commit" || dl[0].Ref != "dddd4444" {
		t.Fatalf("delivery links: %+v", dl)
	}
	if len(linksFromDelivery(traceDelivery{Ref: "refs/heads/REQ-1", SHA: "0000000000"})) != 0 {
		t.Fatal("zero sha (branch delete) must be ignored")
	}
	if traceFirstLine("  a\nb") != "a" || len([]rune(traceFirstLine(strings.Repeat("字", 300)))) != 200 {
		t.Fatal("traceFirstLine")
	}
}

func TestReleaseNotes(t *testing.T) {
	prev := int64(8)
	rel := traceRelease{ID: 9, AppName: "web", Env: "prod", Image: "reg/web:v2", CreatedAt: time.Date(2026, 11, 22, 2, 0, 0, 0, time.UTC), PreviousReleaseID: &prev}
	md := buildReleaseNotes(rel, []traceRelItem{{ID: 12, Title: "单点登录\n第二行", Status: "done"}}, []traceRelItem{{ID: 3, Title: "空指针", Status: "fixed"}},
		[]traceRelCommit{{SHA: "cccc3333dddd", Title: "chore: deps", Author: "bob"}, {SHA: "eeee5555"}})
	for _, frag := range []string{"## web prod 发布说明", "2026-11-22 10:00", "`reg/web:v2`", "上一次成功发布 #8",
		"### 新功能", "- REQ-12 单点登录 第二行（已完成）", "### 缺陷修复", "- BUG-3 空指针（已修复）", "### 其他提交", "- `cccc3333` chore: deps — bob", "`eeee5555` （未记录提交说明）"} {
		if !strings.Contains(md, frag) {
			t.Errorf("notes missing %q:\n%s", frag, md)
		}
	}
	empty := buildReleaseNotes(traceRelease{ID: 1, Kind: "rollback"}, nil, nil, nil)
	if !strings.Contains(empty, "发布 #1 发布说明") || !strings.Contains(empty, "回滚") || !strings.Contains(empty, "首次成功发布") || !strings.Contains(empty, "没有识别到") || strings.Contains(empty, "### ") {
		t.Fatalf("empty notes:\n%s", empty)
	}
}

func TestTraceGuard(t *testing.T) {
	g := &traceSyncGuard{last: map[string]time.Time{}}
	now := time.Now()
	if !g.allow("bug", 1, now) || g.allow("bug", 1, now.Add(10*time.Second)) {
		t.Fatal("second call within interval must be throttled")
	}
	if !g.allow("requirement", 1, now) {
		t.Fatal("different item must not be throttled")
	}
	if !g.allow("bug", 1, now.Add(traceLazyInterval+time.Second)) {
		t.Fatal("after interval must sync again")
	}
	g.reset("bug", 1)
	if !g.allow("bug", 1, now.Add(traceLazyInterval+2*time.Second)) {
		t.Fatal("reset must allow")
	}
}

// 不连数据库的处理器校验：参数错误在访问数据库前返回 400。
func TestTraceHandlersValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Username: "alice", Permissions: []string{"devcenter:read", "devcenter:manage"}})
		c.Next()
	})
	m := &Module{d: &app.Deps{}}
	pass := func(c *gin.Context) { c.Next() }
	m.registerTrace(r.Group("/devcenter"), pass, pass)
	do := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/devcenter/requirements/abc/trace", ""},
		{"GET", "/devcenter/bugs/0/trace", ""},
		{"POST", "/devcenter/requirements/1/links", "not json"},
		{"POST", "/devcenter/requirements/1/links", `{"kind":"app","ref":"1"}`},
		{"POST", "/devcenter/bugs/1/links", `{"kind":"commit","ref":"zz"}`},
		{"POST", "/devcenter/bugs/1/links", `{"kind":"run","ref":"1","app_id":"x"}`},
		{"DELETE", "/devcenter/links/x", ""},
		{"GET", "/devcenter/releases/-1/items", ""},
	} {
		if code := do(c.method, c.path, c.body); code != 400 {
			t.Errorf("%s %s %s: want 400, got %d", c.method, c.path, c.body, code)
		}
	}
}

// 整个模块注册（含 module.go 的路由）不能与价值流路由冲突（gin 冲突时 panic）。
func TestRegisterNoRouteConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("route registration panicked: %v", p)
		}
	}()
	New().Register(gin.New().Group(""), &app.Deps{})
}
