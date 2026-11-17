package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func sparkQ(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestParseSparkRequestValidation(t *testing.T) {
	many := make([]string, 51)
	for i := range many {
		many[i] = "p" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
	}
	cases := []struct {
		name string
		q    map[string]string
	}{
		{"no cluster", sparkQ("kind", "pod", "names", "a", "namespace", "ns")},
		{"bad kind", sparkQ("cluster_id", "1", "kind", "svc", "names", "a", "namespace", "ns")},
		{"bad workload kind", sparkQ("cluster_id", "1", "kind", "workload", "workload_kind", "replicasets", "names", "a", "namespace", "ns")},
		{"no names", sparkQ("cluster_id", "1", "kind", "pod", "namespace", "ns")},
		{"too many", sparkQ("cluster_id", "1", "kind", "pod", "namespace", "ns", "names", strings.Join(many, ","))},
		{"no namespace", sparkQ("cluster_id", "1", "kind", "pod", "names", "a")},
		{"bad name", sparkQ("cluster_id", "1", "kind", "pod", "namespace", "ns", "names", `a"}`)},
		{"regex inject", sparkQ("cluster_id", "1", "kind", "pod", "namespace", "ns", "names", ".*")},
		{"bad ns", sparkQ("cluster_id", "1", "kind", "pod", "names", "Bad_NS/a")},
		{"ip count", sparkQ("cluster_id", "1", "kind", "node", "names", "n1,n2", "ips", "10.0.0.1")},
		{"bad ip", sparkQ("cluster_id", "1", "kind", "node", "names", "n1", "ips", "nope")},
	}
	for _, tc := range cases {
		if _, err := parseSparkRequest(tc.q); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func TestParseSparkRequestTargets(t *testing.T) {
	req, err := parseSparkRequest(sparkQ("cluster_id", "3", "kind", "pod", "namespace", "default", "names", "web-1, kube-system/coredns-abc,web-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Targets) != 2 {
		t.Fatalf("dedupe failed: %+v", req.Targets)
	}
	if req.Targets[1].Namespace != "kube-system" || req.Targets[1].Name != "coredns-abc" || req.Targets[1].Key != "kube-system/coredns-abc" {
		t.Fatalf("ns/name not split: %+v", req.Targets[1])
	}
	w, err := parseSparkRequest(sparkQ("cluster_id", "3", "kind", "workload", "namespace", "default", "names", "api"))
	if err != nil || w.WorkloadKind != "deployments" {
		t.Fatalf("workload default kind: %v %+v", err, w)
	}
	n, err := parseSparkRequest(sparkQ("cluster_id", "3", "kind", "node", "names", "Node-A,node-b", "ips", "10.0.0.1,"))
	if err != nil || n.Targets[0].IP != "10.0.0.1" || n.Targets[1].IP != "" {
		t.Fatalf("node parse: %v %+v", err, n)
	}
}

func TestBuildSparkQueriesPassCentralScope(t *testing.T) {
	central := sourceScope{Cluster: clusterIdentity{ID: 7, Name: "prod"}, Central: true, Label: "cluster", Value: "prod-a"}
	reqs := []string{"pod", "workload", "node"}
	for _, kind := range reqs {
		q := sparkQ("cluster_id", "7", "kind", kind, "names", "api,web.v2", "namespace", "default")
		if kind == "node" {
			q["names"] = "node-1,node-2"
		}
		req, err := parseSparkRequest(q)
		if err != nil {
			t.Fatal(err)
		}
		qs := buildSparkQueries(req, central)
		for _, query := range []string{qs.CPU, qs.Mem, `node_uname_info` + joinMatchers(scopeMatcher(central))} {
			if err := scopedPromQL(query, central, false); err != nil {
				t.Errorf("%s: central validator rejected %q: %v", kind, query, err)
			}
			if err := validatePromQL(query); err != nil {
				t.Errorf("%s: %v", kind, err)
			}
		}
		// Dedicated (non-central) queries must not carry a cluster matcher.
		plain := buildSparkQueries(req, sourceScope{})
		if strings.Contains(plain.CPU, "prod-a") {
			t.Errorf("dedicated query leaked central matcher: %s", plain.CPU)
		}
	}
	req, _ := parseSparkRequest(sparkQ("cluster_id", "7", "kind", "workload", "workload_kind", "statefulsets", "namespace", "db", "names", "web.v2"))
	qs := buildSparkQueries(req, sourceScope{})
	if !strings.Contains(qs.CPU, `pod=~"(?:web\\.v2)-[0-9]+"`) || !strings.Contains(qs.CPU, `namespace="db"`) {
		t.Fatalf("unexpected workload selector: %s", qs.CPU)
	}
}

func series(labels map[string]string, pts ...[2]any) promMatrixSeries {
	return promMatrixSeries{Metric: labels, Values: pts}
}

func pt(ts float64, v string) [2]any { return [2]any{ts, v} }

func TestAssembleWorkloadSumsPodsAndPrefersLongestName(t *testing.T) {
	req, err := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "workload", "namespace", "default", "names", "api,api-v2"))
	if err != nil {
		t.Fatal(err)
	}
	cpu := []promMatrixSeries{
		series(map[string]string{"namespace": "default", "pod": "api-7d9f8c-abcde", "container": "app"}, pt(60, "0.1"), pt(120, "0.2")),
		series(map[string]string{"namespace": "default", "pod": "api-7d9f8c-fghij", "container": "app"}, pt(60, "0.3"), pt(120, "NaN")),
		series(map[string]string{"namespace": "default", "pod": "api-v2-5b6c7-zzzzz", "container": "app"}, pt(60, "1")),
		series(map[string]string{"namespace": "other", "pod": "api-7d9f8c-abcde"}, pt(60, "9")),
		series(map[string]string{"namespace": "default", "pod": "apix-7d9f8c-abcde"}, pt(60, "9")),
	}
	items := assembleSparklines(req, cpu, nil, nil)
	if len(items) != 2 || items[0].Name != "api" || items[1].Name != "api-v2" {
		t.Fatalf("items: %+v", items)
	}
	if got := items[0].CPU; len(got) != 2 || got[0][1] != 0.4 || got[1][1] != 0.2 {
		t.Fatalf("api cpu: %+v", got)
	}
	if got := items[1].CPU; len(got) != 1 || got[0][1] != 1 {
		t.Fatalf("api-v2 cpu: %+v", got)
	}
	if items[0].Mem == nil || len(items[0].Mem) != 0 {
		t.Fatalf("mem should be empty array, got %#v", items[0].Mem)
	}
}

func TestAssembleNodeResolvesByIPLabelAndUname(t *testing.T) {
	req, err := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "node", "names", "n1,n2,n3", "ips", "10.0.0.1,,"))
	if err != nil {
		t.Fatal(err)
	}
	cpu := []promMatrixSeries{
		series(map[string]string{"instance": "10.0.0.1:9100", "cpu": "0"}, pt(60, "0.8")),
		series(map[string]string{"instance": "10.0.0.1:9100", "cpu": "1"}, pt(60, "0.6")),
		series(map[string]string{"instance": "x:9100", "node": "n2"}, pt(60, "0.5")),
		series(map[string]string{"instance": "10.0.0.3:9100"}, pt(60, "0.0")),
	}
	mem := []promMatrixSeries{series(map[string]string{"instance": "10.0.0.3:9100"}, pt(60, "42.5"))}
	uname := []promMatrixSeries{series(map[string]string{"instance": "10.0.0.3:9100", "nodename": "n3"}, pt(60, "1"))}
	items := assembleSparklines(req, cpu, mem, uname)
	if v := items[0].CPU[0][1]; v != 30 {
		t.Fatalf("n1 cpu = %v, want 30 (avg idle .7)", v)
	}
	if v := items[1].CPU[0][1]; v != 50 {
		t.Fatalf("n2 cpu = %v", v)
	}
	if v := items[2].CPU[0][1]; v != 100 || items[2].Mem[0][1] != 42.5 {
		t.Fatalf("n3 = %+v", items[2])
	}
}

func TestAuthorizeSpark(t *testing.T) {
	podReq, _ := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "pod", "names", "team-a/web,team-b/web"))
	nodeReq, _ := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "node", "names", "n1"))
	admin := &rbac.Principal{Permissions: []string{"*"}}
	if err := authorizeSpark(admin, podReq); err != nil {
		t.Fatalf("admin pod: %v", err)
	}
	if err := authorizeSpark(admin, nodeReq); err != nil {
		t.Fatalf("admin node: %v", err)
	}
	reader := &rbac.Principal{Permissions: []string{"workload:read"}}
	if err := authorizeSpark(reader, nodeReq); err == nil {
		t.Fatal("node trend without node:read must be forbidden")
	}
	if err := authorizeSpark(nil, podReq); err == nil {
		t.Fatal("nil principal must fail")
	}
}

func TestSparkWindowAlignedToMinute(t *testing.T) {
	start, end := sparkWindowAt(time.Date(2026, 11, 17, 10, 30, 42, 0, time.UTC))
	if end.Second() != 0 || end.Sub(start) != 30*time.Minute {
		t.Fatalf("window %v..%v", start, end)
	}
}

func TestFetchSparklinesAgainstFakePrometheus(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		if r.URL.Query().Get("step") != "60" {
			t.Errorf("step = %q", r.URL.Query().Get("step"))
		}
		val := "0.25"
		if strings.HasPrefix(q, "container_memory") {
			val = "1048576"
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{
			map[string]any{"metric": map[string]string{"namespace": "default", "pod": "web-0"}, "values": [][2]any{{1700000000, val}}},
		}}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	cl := newClient(&DataSource{Type: "prometheus", URL: srv.URL, TimeoutSec: 5})
	req, _ := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "pod", "namespace", "default", "names", "web-0,web-1"))
	start, end := sparkWindowAt(time.Now())
	items, err := fetchSparklines(context.Background(), cl, req, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %v", queries)
	}
	if items[0].CPU[0][1] != 0.25 || items[0].Mem[0][1] != 1048576 || items[0].CPU[0][0] != 1700000000 {
		t.Fatalf("web-0 = %+v", items[0])
	}
	if len(items[1].CPU) != 0 || items[1].CPU == nil {
		t.Fatalf("web-1 must be an empty array: %#v", items[1].CPU)
	}
}

func TestFetchSparklinesUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cl := newClient(&DataSource{Type: "prometheus", URL: srv.URL, TimeoutSec: 5})
	req, _ := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "pod", "namespace", "default", "names", "web-0"))
	start, end := sparkWindowAt(time.Now())
	if _, err := fetchSparklines(context.Background(), cl, req, start, end); err == nil {
		t.Fatal("expected upstream error")
	}
}

func TestEmptySparkItemsAndUnits(t *testing.T) {
	req, _ := parseSparkRequest(sparkQ("cluster_id", "1", "kind", "node", "names", "n1"))
	items := emptySparkItems(req)
	b, _ := json.Marshal(items)
	if string(b) != `[{"name":"n1","cpu":[],"mem":[]}]` {
		t.Fatalf("json = %s", b)
	}
	if sparkUnits("node").CPU != "percent" || sparkUnits("pod").Mem != "bytes" {
		t.Fatal("units")
	}
}
