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

func testDS(typ, u string) *DataSource {
	return &DataSource{Name: "t", Type: typ, URL: u, AuthType: "bearer", secretPlain: "tok", TimeoutSec: 5,
		Options: map[string]any{"index": "logs-*"}}
}

func TestPromRangeValidationAndProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/api/v1/query_range" || r.URL.Query().Get("step") != "60" {
			t.Errorf("unexpected %s", r.URL.String())
		}
		io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()
	c := newClient(testDS("prometheus", srv.URL))
	if _, err := c.PromRange(context.Background(), "", "now-1h", "", "60"); err == nil {
		t.Fatal("empty query must fail")
	}
	if _, err := c.PromRange(context.Background(), "up", "now-30d", "", "1s"); err == nil {
		t.Fatal("too many points must fail")
	}
	env, err := c.PromRange(context.Background(), "up", "now-1h", "now", "1m")
	if err != nil || env.Status != "success" {
		t.Fatalf("got %v %v", env, err)
	}
}

func TestLokiNormalize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" || r.URL.Query().Get("direction") != "backward" {
			t.Errorf("unexpected %s", r.URL.String())
		}
		io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
		 {"stream":{"app":"api","pod":"api-1"},"values":[["1700000000000000000","2023 ERROR boom"],["1700000001000000000","ok"]]}]}}`)
	}))
	defer srv.Close()
	r, err := newClient(testDS("loki", srv.URL)).QueryLogs(context.Background(), &LogQuery{Query: `{app="api"}`, From: "now-1h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0].Message != "ok" || r.Rows[1].Level != "error" || r.Rows[1].Service != "api" || r.Rows[1].Host != "api-1" {
		t.Fatalf("bad rows %+v", r.Rows)
	}
}

func TestTranslateQuery(t *testing.T) {
	cases := map[string]string{
		"":                                     "*",
		`level : error and service:api`:        `level:error AND service:api`,
		`msg:"a and b" or not host:x`:          `msg:"a and b" OR NOT host:x`,
		`status:500 AND kubernetes.pod_name:x`: `status:500 AND kubernetes.pod_name:x`,
	}
	for in, want := range cases {
		if got := translateQuery(in); got != want {
			t.Errorf("translateQuery(%q)=%q want %q", in, got, want)
		}
	}
}

func TestESQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/logs-*/_search" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		if !strings.Contains(string(b), `"query":"level:error AND service:api"`) {
			t.Errorf("body %s", b)
		}
		io.WriteString(w, `{"hits":{"total":{"value":7},"hits":[{"_index":"logs-1","_id":"x","_source":
		 {"@timestamp":"2024-01-01T00:00:00Z","message":"failed","log":{"level":"ERROR"},"service":{"name":"api"},"host":{"name":"n1"}}}]}}`)
	}))
	defer srv.Close()
	r, err := newClient(testDS("elasticsearch", srv.URL)).QueryLogs(context.Background(), &LogQuery{Query: "level:error and service:api"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Rows[0]
	if r.Total != 7 || row.Message != "failed" || row.Level != "error" || row.Service != "api" || row.Host != "n1" || row.TS.Year() != 2024 {
		t.Fatalf("bad %+v", r)
	}
}

func TestTempoOTLP(t *testing.T) {
	raw := `{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
	 "scopeSpans":[{"spans":[{"spanId":"AAAAAAAAAAE=","name":"GET /","startTimeUnixNano":"1000000","endTimeUnixNano":"3000000","status":{"code":2}}]}]}]}`
	tr, err := parseOTLPTrace("ABC", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := tr.Spans[0]
	if s.SpanID != "0000000000000001" || s.Service != "api" || s.DurationUs != 2000 || !s.Error {
		t.Fatalf("bad span %+v", s)
	}
	if sum := summarize(*tr); sum.RootService != "api" || sum.DurationMs != 2 {
		t.Fatalf("bad summary %+v", sum)
	}
}

func TestRobotErrcode(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sign") == "" {
			t.Error("dingtalk sign missing")
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"errcode":310000,"errmsg":"sign not match"}`)
	}))
	defer srv.Close()
	ch := &Channel{Type: "dingtalk", Config: ChannelConfig{Webhook: srv.URL + "/robot/send?access_token=x", Secret: "SEC"}}
	title, body := formatAlert(Alert{Alertname: "HighCPU", Severity: "critical", Labels: map[string]string{"instance": "n1"}})
	err := sendChannel(context.Background(), ch, title, body)
	if err == nil || !strings.Contains(err.Error(), "310000") {
		t.Fatalf("want robot error, got %v", err)
	}
	if got["msgtype"] != "markdown" {
		t.Fatalf("payload %v", got)
	}
}
