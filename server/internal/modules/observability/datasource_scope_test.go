package observability

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testScope(id int64, value string) sourceScope {
	return sourceScope{Cluster: clusterIdentity{ID: id, Name: "dev-" + value, Env: "dev"},
		Central: true, Label: "k8s.cluster.name", Value: value}
}

func TestClusterOwnershipRequiresExplicitBinding(t *testing.T) {
	for _, tc := range []struct {
		opts map[string]any
		want string
	}{
		{nil, ""},
		{map[string]any{"cluster_id": "1"}, "cluster"},
		{map[string]any{"cluster_name": "dev-a"}, "cluster"},
		{map[string]any{"scope": "central", "cluster_values": map[string]any{"1": "dev-a"}}, "central"},
	} {
		if got := scopeKind(tc.opts); got != tc.want {
			t.Fatalf("scopeKind(%v)=%q, want %q", tc.opts, got, tc.want)
		}
	}
	if _, err := centralValues(map[string]any{"cluster_values": map[string]any{"1": ""}}); err == nil {
		t.Fatal("empty central mapping accepted")
	}
	if values, err := centralValues(map[string]any{"cluster_values": map[string]string{"1": "dev-a", "2": "dev-b"}}); err != nil || len(values) != 2 {
		t.Fatalf("typed central mappings: %v %v", values, err)
	}
}

func TestScopedSelectorsFailClosed(t *testing.T) {
	a := testScope(1, "a")
	b := testScope(2, "b")
	valid := `sum(rate(container_cpu_usage_seconds_total{k8s.cluster.name="a",namespace="prod"}[5m]))`
	if err := scopedPromQL(valid, a, false); err != nil {
		t.Fatalf("scoped range expression rejected: %v", err)
	}
	if err := scopedPromQL(`up{k8s.cluster.name="a",pod!~"test-.*"}`, a, false); err != nil {
		t.Fatalf("non-cluster matcher rejected: %v", err)
	}
	for _, q := range []string{
		`up`,
		`up{k8s.cluster.name="b"}`,
		`up{k8s.cluster.name=~"a|b"}`,
		`up{k8s.cluster.name="a"} or up`,
		`up{k8s.cluster.name="a",k8s.cluster.name="a"}`,
	} {
		if err := scopedPromQL(q, a, false); err == nil {
			t.Errorf("unsafe PromQL accepted: %s", q)
		}
	}
	if err := scopedPromQL(`up{k8s.cluster.name="a"}`, b, false); err == nil {
		t.Fatal("same-environment cluster B read cluster A")
	}
	if err := scopedPromQL(`{k8s.cluster.name="a"}`, a, true); err != nil {
		t.Fatal(err)
	}
	if err := scopedPromQL(`{namespace="prod"}`, a, true); err == nil {
		t.Fatal("unscoped Prometheus metadata accepted")
	}
	if err := scopedLogQL(`{k8s.cluster.name="a",pod="api"} |= "error"`, a); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`{pod="api"}`, `{k8s.cluster.name="b"}`, `{k8s.cluster.name="a"} or {k8s.cluster.name="b"}`} {
		if err := scopedLogQL(q, a); err == nil {
			t.Errorf("unsafe LogQL accepted: %s", q)
		}
	}
}

func TestCentralAdaptersRejectCrossClusterWithoutUpstreamDisclosure(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/api/v1/query" {
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		io.WriteString(w, `{"status":"success","data":[]}`)
	}))
	defer srv.Close()
	c := newClient(&DataSource{Type: "prometheus", URL: srv.URL})
	c.scope = testScope(1, "a")
	if _, err := c.PromInstant(context.Background(), `up`, ""); err == nil || calls != 0 {
		t.Fatalf("unscoped PromQL reached upstream: %v, calls=%d", err, calls)
	}
	if _, err := c.PromMeta(context.Background(), "/api/v1/labels", nil, "", "", false); err == nil || calls != 0 {
		t.Fatalf("unscoped metadata reached upstream: %v, calls=%d", err, calls)
	}
	if _, err := c.prom(context.Background(), "/api/v1/rules", nil); err == nil || calls != 0 {
		t.Fatalf("central rules reached upstream: %v, calls=%d", err, calls)
	}
	if _, err := c.PromInstant(context.Background(), `up{k8s.cluster.name="a"}`, ""); err != nil || calls != 1 {
		t.Fatalf("scoped query failed: %v, calls=%d", err, calls)
	}
}

func TestCentralLogAndTraceOwnership(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/api/services" {
			io.WriteString(w, `{"data":["api"]}`)
			return
		}
		if r.URL.Path == "/api/traces" {
			io.WriteString(w, `{"data":[{"traceID":"aabbccdd","processes":{"p1":{"serviceName":"api","tags":[{"key":"k8s.cluster.name","value":"b"}]}},"spans":[{"spanID":"11","processID":"p1","operationName":"GET /"}]}]}`)
			return
		}
		if r.URL.Path == "/api/traces/aabbccdd" {
			io.WriteString(w, `{"data":[{"traceID":"aabbccdd","processes":{"p1":{"serviceName":"api","tags":[{"key":"k8s.cluster.name","value":"b"}]}},"spans":[{"spanID":"11","processID":"p1","operationName":"GET /"}]}]}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	jaeger := newClient(&DataSource{Type: "jaeger", URL: srv.URL})
	jaeger.scope = testScope(1, "a")
	if _, err := jaeger.GetTrace(context.Background(), "aabbccdd"); err == nil || !strings.Contains(err.Error(), "current cluster") {
		t.Fatalf("foreign trace was returned: %v", err)
	}
	services, err := jaeger.TraceServices(context.Background())
	if err != nil || len(services) != 0 || calls != 3 {
		t.Fatalf("foreign service was returned: %v, %v, calls=%d", services, err, calls)
	}
	trace, err := parseOTLPTrace("aabbccdd", []byte(`{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}},{"key":"k8s.cluster.name","value":{"stringValue":"a"}}]},"scopeSpans":[{"spans":[{"spanId":"ESI=","name":"GET","startTimeUnixNano":"1000000","endTimeUnixNano":"2000000"}]}]}]}`))
	if err != nil || !jaeger.traceMatches(trace) || trace.Spans[0].Tags["k8s.cluster.name"] != "a" {
		t.Fatalf("Tempo resource tag lost: %+v, %v", trace, err)
	}
	trace.Spans[0].Tags["k8s.cluster.name"] = "b"
	if jaeger.traceMatches(trace) {
		t.Fatal("cross-cluster Tempo trace accepted")
	}
}

func TestCentralElasticsearchAddsTermFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		query, _ := json.Marshal(body["query"])
		if !strings.Contains(string(query), `"k8s.cluster.name":"a"`) {
			t.Errorf("central ES query missing cluster term: %s", query)
		}
		io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer srv.Close()
	c := newClient(&DataSource{Type: "elasticsearch", URL: srv.URL, Options: map[string]any{"index": "logs-*"}})
	c.scope = testScope(1, "a")
	if _, err := c.QueryLogs(context.Background(), &LogQuery{Query: "service:api"}); err != nil {
		t.Fatal(err)
	}
}

func TestCentralSilenceMatchersAndDeletion(t *testing.T) {
	deletes := 0
	createdWithScope := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/silences":
			io.WriteString(w, `[{"id":"aaaaaaaa","matchers":[{"name":"k8s.cluster.name","value":"a","isRegex":false}]},{"id":"bbbbbbbb","matchers":[{"name":"k8s.cluster.name","value":"b","isRegex":false}]}]`)
		case "GET /api/v2/silence/bbbbbbbb":
			io.WriteString(w, `{"matchers":[{"name":"k8s.cluster.name","value":"b","isRegex":false}]}`)
		case "DELETE /api/v2/silence/bbbbbbbb":
			deletes++
		case "POST /api/v2/silences":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, entry := range body["matchers"].([]any) {
				matcher := entry.(map[string]any)
				if matcher["name"] == "k8s.cluster.name" && matcher["value"] == "a" {
					createdWithScope = true
				}
			}
			io.WriteString(w, `{"silenceID":"aaaaaaaa"}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	c := newClient(&DataSource{Type: "alertmanager", URL: srv.URL})
	c.scope = testScope(1, "a")
	items, err := c.ListSilences(context.Background(), nil)
	if err != nil || len(items.([]any)) != 1 {
		t.Fatalf("foreign silence visible: %v, %v", items, err)
	}
	if err := c.DeleteSilence(context.Background(), "bbbbbbbb"); err == nil || deletes != 0 {
		t.Fatalf("foreign silence deleted: %v, deletes=%d", err, deletes)
	}
	in := &SilenceInput{Comment: "maintenance"}
	in.Matchers = append(in.Matchers, struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual *bool  `json:"isEqual,omitempty"`
	}{Name: "service", Value: "api"})
	if _, err := c.CreateSilence(context.Background(), in, "tester"); err != nil || !createdWithScope {
		t.Fatalf("central silence missing cluster matcher: %v, created=%v", err, createdWithScope)
	}
}

func TestAlertSQLScopesSameEnvironmentClusters(t *testing.T) {
	args := []any{}
	sql := alertSourceSQL([]alertSource{{ID: 3, Scope: sourceScope{Cluster: clusterIdentity{ID: 1, Name: "a", Env: "dev"}}},
		{ID: 4, Scope: testScope(1, "a")}}, &args)
	if !strings.Contains(sql, "a.labels->>'cluster_id'") || !strings.Contains(sql, "a.labels->>") || len(args) != 8 {
		t.Fatalf("alert scope SQL incomplete: %s %#v", sql, args)
	}
	if (sourceScope{Cluster: clusterIdentity{ID: 2, Name: "b"}}).checkLabels(map[string]string{"cluster_id": "1"}) {
		t.Fatal("same environment alert crossed cluster boundary")
	}
}
