package devcenter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testAlert() *alertInfo {
	return &alertInfo{
		ID: 42, Alertname: "KubePodCrashLooping", Severity: "critical", Status: "firing", Fingerprint: "abc123",
		Labels:      map[string]string{"cluster": "prod-sh", "namespace": "shop", "pod": "order-service-7c9d8f6b5d-x2k4p", "container": "order", "alertname": "KubePodCrashLooping"},
		Annotations: map[string]string{"summary": "Pod 反复重启", "description": "order-service 在 15 分钟内重启 5 次", "runbook_url": "https://runbooks.example.com/crash"},
		FiredAt:     time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		DSEnv:       "production",
	}
}

func TestDeriveAlertScope(t *testing.T) {
	sc := deriveAlertScope(testAlert())
	if sc.Cluster != "prod-sh" || sc.Namespace != "shop" {
		t.Fatalf("scope %+v", sc)
	}
	// container 标签排在 pod 推导之前；pod 名去掉 Deployment 后缀得到 order-service
	if len(sc.AppCands) != 2 || sc.AppCands[0] != "order" || sc.AppCands[1] != "order-service" || sc.AppLabels[1] != "pod" {
		t.Fatalf("app candidates %v %v", sc.AppCands, sc.AppLabels)
	}
	a := testAlert()
	a.Labels = map[string]string{"app": "Web", "service": "web", "cluster_id": ""}
	a.DSClusterID, a.DSCluster = "7", "c7"
	sc = deriveAlertScope(a)
	if sc.App != "web" || len(sc.AppCands) != 1 || sc.ClusterID != "7" || sc.Cluster != "c7" {
		t.Fatalf("dedupe/datasource fallback failed %+v", sc)
	}
	a.Labels = map[string]string{"pod": "redis-2"}
	if sc = deriveAlertScope(a); sc.App != "redis" {
		t.Fatalf("statefulset ordinal not stripped: %+v", sc)
	}
}

func TestAlertBugTitle(t *testing.T) {
	a := testAlert()
	if got := alertBugTitle(a, deriveAlertScope(a)); got != "[告警] KubePodCrashLooping · prod-sh/shop" {
		t.Fatalf("title %q", got)
	}
	a.Labels = map[string]string{}
	if got := alertBugTitle(a, deriveAlertScope(a)); got != "[告警] KubePodCrashLooping · 未知集群/-" {
		t.Fatalf("fallback title %q", got)
	}
	a.Alertname = strings.Repeat("长", 300)
	if got := alertBugTitle(a, deriveAlertScope(a)); len([]rune(got)) != 200 {
		t.Fatalf("title not truncated: %d", len([]rune(got)))
	}
}

func TestMatchSuspects(t *testing.T) {
	fired := testAlert().FiredAt
	sc := deriveAlertScope(testAlert())
	env := func(name, ns, dep string, target int64) releaseEnv {
		return releaseEnv{Name: name, Namespace: ns, Deployment: dep, TargetID: target}
	}
	targets := map[int64]targetCluster{1: {ID: "1", Name: "prod-sh"}, 2: {ID: "2", Name: "test-bj"}}
	list := []releaseRow{
		{ID: 1, AppID: 10, AppName: "order-service", Env: "prod", Image: "r/order:v2", Status: "succeeded", CreatedAt: fired.Add(-30 * time.Minute), Envs: []releaseEnv{env("prod", "shop", "order-service", 1), env("test", "shop-test", "order-service", 2)}},
		{ID: 2, AppID: 10, AppName: "order-service", Env: "test", Image: "r/order:v2", CreatedAt: fired.Add(-20 * time.Minute), Envs: []releaseEnv{env("prod", "shop", "order-service", 1), env("test", "shop-test", "order-service", 2)}}, // 其他命名空间：排除
		{ID: 3, AppID: 11, AppName: "cart", Env: "prod", Image: "r/cart:v9", CreatedAt: fired.Add(-10 * time.Minute), Envs: []releaseEnv{env("prod", "shop", "cart", 1)}},                                                                  // 同命名空间同集群：弱匹配
		{ID: 4, AppID: 12, AppName: "billing", Env: "prod", Image: "r/billing:v1", CreatedAt: fired.Add(-5 * time.Minute), Envs: []releaseEnv{env("prod", "finance", "billing", 1)}},                                                       // 无关：排除
		{ID: 5, AppID: 13, AppName: "web", Env: "prod", Image: "r/web:v1", CreatedAt: fired.Add(-5 * time.Minute), Envs: []releaseEnv{env("prod", "shop", "web", 2)}},                                                                      // 同命名空间但另一集群：排除
		{ID: 6, AppID: 14, AppName: "legacy", Env: "prod", Image: "r/legacy:v1", CreatedAt: fired.Add(-90 * time.Minute)},                                                                                                                  // 无环境配置、应用名不匹配：排除
	}
	got := matchSuspects(fired, sc, list, targets)
	if len(got) != 2 || got[0].ReleaseID != 1 || got[1].ReleaseID != 3 {
		t.Fatalf("unexpected suspects %+v", got)
	}
	if got[0].Score != 6 || got[0].MinutesBefore != 30 || len(got[0].Reasons) != 3 || !strings.Contains(got[0].Reasons[0], "pod=order-service") {
		t.Fatalf("top suspect %+v", got[0])
	}
	if got[1].Score != 3 {
		t.Fatalf("weak suspect %+v", got[1])
	}
	// 告警没有命名空间、集群时只能靠应用名
	bare := alertScope{AppCands: []string{"legacy"}, AppLabels: []string{"app"}, App: "legacy"}
	got = matchSuspects(fired, bare, list, targets)
	if len(got) != 1 || got[0].ReleaseID != 6 || got[0].Score != 3 {
		t.Fatalf("app-only match %+v", got)
	}
	// 上限 10 条，按得分与时间排序
	many := []releaseRow{}
	for i := 0; i < 15; i++ {
		many = append(many, releaseRow{ID: int64(100 + i), AppName: "legacy", Env: "prod", CreatedAt: fired.Add(-time.Duration(i+1) * time.Minute)})
	}
	got = matchSuspects(fired, bare, many, nil)
	if len(got) != alertBugMaxSuspects || got[0].ReleaseID != 100 {
		t.Fatalf("limit/order %d %+v", len(got), got[0])
	}
}

func TestBuildAlertBugInput(t *testing.T) {
	a := testAlert()
	sc := deriveAlertScope(a)
	sus := []SuspectRelease{{ReleaseID: 1, AppName: "order-service", Env: "prod", Image: "r/order:v2", Status: "succeeded", MinutesBefore: 90, Reasons: []string{"命名空间相同（shop）"}}}
	in := buildAlertBugInput(a, sc, sus, alertBugTitle(a, sc))
	if err := in.check("bugs", true); err != nil {
		t.Fatalf("generated input rejected: %v", err)
	}
	if *in.Priority != "P1" {
		t.Fatalf("priority %s", *in.Priority)
	}
	var d struct {
		Source   map[string]any   `json:"source"`
		Suspects []SuspectRelease `json:"suspect_releases"`
		Severity string           `json:"severity"`
		Env      string           `json:"env"`
	}
	if err := json.Unmarshal(in.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Source["alert_id"].(float64) != 42 || d.Source["cluster"] != "prod-sh" || d.Source["app"] != "order" || d.Source["alert_url"] != "#/observe/events/42" {
		t.Fatalf("source %+v", d.Source)
	}
	if len(d.Suspects) != 1 || d.Severity != "critical" || d.Env != "prod" {
		t.Fatalf("data %+v", d)
	}
	desc := *in.Description
	for _, frag := range []string{"## 告警摘要", "Pod 反复重启", "namespace=shop", "#/observe/events/42", "发布 #1 order-service · prod · r/order:v2", "告警前 1 小时 30 分钟", "https://runbooks.example.com/crash"} {
		if !strings.Contains(desc, frag) {
			t.Fatalf("description misses %q:\n%s", frag, desc)
		}
	}
	// 无可疑发布时给出说明；非 https 处理手册不写入
	a.Annotations["runbook_url"] = "javascript:alert(1)"
	desc = buildAlertBugDescription(a, sc, nil)
	if !strings.Contains(desc, "未找到同应用") || strings.Contains(desc, "javascript:") {
		t.Fatalf("empty suspects description:\n%s", desc)
	}
}

func TestAlertBugMappings(t *testing.T) {
	cases := map[string][2]string{"critical": {"critical", "P1"}, "warning": {"major", "P2"}, "info": {"minor", "P3"}, "": {"major", "P2"}}
	for in, want := range cases {
		if bugSeverity(in) != want[0] || bugPriority(in) != want[1] {
			t.Fatalf("%q -> %s %s", in, bugSeverity(in), bugPriority(in))
		}
	}
	if bugEnv("Production") != "prod" || bugEnv("staging") != "pre" || bugEnv("x") != "" {
		t.Fatal("env mapping")
	}
	if s := truncBytes(strings.Repeat("告", 10), 7); !strings.HasPrefix(s, "告告") || strings.HasPrefix(s, "告告告") {
		t.Fatalf("utf-8 safe truncation %q", s)
	}
	if humanMinutes(5) != "5 分钟" || humanMinutes(120) != "2 小时" {
		t.Fatal("humanMinutes")
	}
	m := jsonStringMap([]byte(`{"a":"x","n":3,"z":null}`))
	if m["a"] != "x" || m["n"] != "3" || len(m) != 2 {
		t.Fatalf("jsonStringMap %v", m)
	}
}

func TestBugFromAlertValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := New()
	r := gin.New()
	g := r.Group("/devcenter")
	noop := func(c *gin.Context) {}
	// 与 module.go 中已有的 /bugs/:id 系列路由共存（gin 静态段优先）
	g.POST("/bugs", noop)
	g.GET("/bugs/:id", noop)
	g.POST("/bugs/:id/comments", noop)
	g.POST("/bugs/:id/links", noop)
	m.registerAlertBug(g, noop, noop)
	for _, body := range []string{`{`, `{}`, `{"alert_id":0}`, `{"alert_id":"x"}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/devcenter/bugs/from-alert", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %s -> %d %s", body, w.Code, w.Body.String())
		}
	}
}
