package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func draftTestBody(endpoint, typ string) map[string]any {
	return map[string]any{
		"name": "draft", "type": typ, "url": endpoint, "env": "dev",
		"auth_type": "none", "timeout_sec": 2,
		"options": map[string]any{"scope": "cluster", "cluster_id": "1", "cluster_name": "dev-a"},
	}
}

func draftTestRouter(m *Module, deps *app.Deps, p *rbac.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p != nil {
			rbac.SetPrincipal(c, p)
		}
		c.Next()
	})
	m.Register(r.Group("/api/v1"), deps)
	return r
}

func draftTestRequest(t *testing.T, r http.Handler, query string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return draftTestRawRequest(r, query, string(raw))
}

func draftTestRawRequest(r http.Handler, query, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/observability/datasources/test"+query, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func draftTestResult(t *testing.T, w *httptest.ResponseRecorder) struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error"`
} {
	t.Helper()
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			OK        bool   `json:"ok"`
			LatencyMS int64  `json:"latency_ms"`
			Error     string `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal("invalid response JSON")
	}
	if w.Code != http.StatusOK || envelope.Code != 0 {
		t.Fatalf("unexpected draft status/code: %d/%d", w.Code, envelope.Code)
	}
	return envelope.Data
}

func TestDatasourceDraftRouteRequiresManage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      *rbac.Principal
		status int
	}{
		{"anonymous", nil, http.StatusUnauthorized},
		{"reader", &rbac.Principal{Permissions: []string{PermDSRead}}, http.StatusForbidden},
		{"manager", &rbac.Principal{Permissions: []string{PermDSManage}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := draftTestRouter(New(), &app.Deps{}, tc.p)
			w := draftTestRawRequest(r, "", `{}`)
			if w.Code != tc.status {
				t.Fatalf("route status=%d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestDatasourceDraftRejectsInvalidInputsBeforeLookup(t *testing.T) {
	p := &rbac.Principal{Permissions: []string{PermDSManage}}
	db, deps := newDraftProtocolDB(t, nil)
	r := draftTestRouter(New(), deps, p)
	for _, tc := range []struct {
		name   string
		query  string
		change func(map[string]any)
	}{
		{"missing scope", "", nil},
		{"invalid cluster", "?cluster_id=x", nil},
		{"duplicate cluster", "?cluster_id=1&cluster_id=2", nil},
		{"conflicting owner", "?cluster_id=2", nil},
		{"unsupported type", "?cluster_id=1", func(b map[string]any) { b["type"] = "unknown" }},
		{"long name", "?cluster_id=1", func(b map[string]any) { b["name"] = strings.Repeat("x", 65) }},
		{"url userinfo", "?cluster_id=1", func(b map[string]any) { b["url"] = "http://user:credential@svc" }},
		{"url fragment", "?cluster_id=1", func(b map[string]any) { b["url"] = "http://svc/#credential" }},
		{"empty fragment", "?cluster_id=1", func(b map[string]any) { b["url"] = "http://svc/#" }},
		{"url query", "?cluster_id=1", func(b map[string]any) { b["url"] = "http://svc?token=credential" }},
		{"scheme", "?cluster_id=1", func(b map[string]any) { b["url"] = "file:///etc/passwd" }},
		{"port", "?cluster_id=1", func(b map[string]any) { b["url"] = "http://svc:65536" }},
		{"timeout zero", "?cluster_id=1", func(b map[string]any) { b["timeout_sec"] = 0 }},
		{"timeout large", "?cluster_id=1", func(b map[string]any) { b["timeout_sec"] = 301 }},
		{"invalid auth", "?cluster_id=1", func(b map[string]any) { b["auth_type"] = "cookie" }},
		{"basic username", "?cluster_id=1", func(b map[string]any) { b["auth_type"] = "basic" }},
		{"header injection", "?cluster_id=1", func(b map[string]any) { b["secret"] = "credential\r\nX: injected" }},
		{"option type", "?cluster_id=1", func(b map[string]any) { b["options"].(map[string]any)["org_id"] = true }},
		{"owner type", "?cluster_id=1", func(b map[string]any) { b["options"].(map[string]any)["cluster_id"] = false }},
		{"central draft", "?cluster_id=1", func(b map[string]any) { b["options"].(map[string]any)["scope"] = "central" }},
		{"central mapping", "?cluster_id=1", func(b map[string]any) { b["options"].(map[string]any)["cluster_values"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := draftTestBody("http://prometheus.monitoring.svc:9090", "prometheus")
			if tc.change != nil {
				tc.change(body)
			}
			w := draftTestRequest(t, r, tc.query, body)
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "credential\r") {
				t.Fatalf("invalid draft was not safely rejected: status=%d", w.Code)
			}
			if db.writes.Load() != 0 {
				t.Fatal("invalid draft attempted a database mutation")
			}
		})
	}
	for _, raw := range []string{
		`{"secret":false}`,
		`{"options":{"org_id":"credential"},"unknown":"credential"}`,
		`{} {}`,
	} {
		if w := draftTestRawRequest(r, "?cluster_id=1", raw); w.Code != http.StatusBadRequest ||
			strings.Contains(w.Body.String(), "credential\r") {
			t.Fatalf("unsafe malformed-body response: %d", w.Code)
		}
	}
	w := draftTestRawRequest(r, "?cluster_id=1", `{"secret":"`+strings.Repeat("x", maxDraftBody)+`"}`)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status=%d", w.Code)
	}
}

func TestDatasourceDraftHealthEndpointsAndNoWrites(t *testing.T) {
	for typ, path := range map[string]string{
		"prometheus": "/api/v1/status/buildinfo", "loki": "/loki/api/v1/labels",
		"elasticsearch": "/", "alertmanager": "/api/v2/status", "jaeger": "/api/services", "tempo": "/api/echo",
	} {
		t.Run(typ, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != path || r.URL.RawQuery != "" {
					t.Error("health endpoint mismatch")
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.Header.Get("Authorization") != "Bearer draft-test-token" {
					t.Error("draft credential missing")
				}
				if typ == "tempo" {
					io.WriteString(w, "echo")
				} else {
					io.WriteString(w, `{"status":"success","data":{},"private":"must-not-be-returned"}`)
				}
			}))
			defer srv.Close()
			db, deps := newDraftProtocolDB(t, nil)
			p := &rbac.Principal{Permissions: []string{PermDSManage}, EnvScope: []string{"dev"}}
			r := draftTestRouter(New(), deps, p)
			body := draftTestBody(srv.URL, typ)
			body["auth_type"], body["secret"] = "bearer", "draft-test-token"
			resultResponse := draftTestRequest(t, r, "?cluster_id=1", body)
			result := draftTestResult(t, resultResponse)
			if !result.OK || result.Error != "" || result.LatencyMS < 0 || calls.Load() != 1 {
				t.Fatal("health check did not confirm the real local dependency")
			}
			if strings.Contains(resultResponse.Body.String(), "must-not-be-returned") ||
				strings.Contains(resultResponse.Body.String(), "draft-test-token") {
				t.Fatal("health response leaked upstream content or credentials")
			}
			if db.writes.Load() != 0 || db.sourceReads.Load() != 0 {
				t.Fatal("new draft accessed stored datasources or wrote to the database")
			}
		})
	}
}

func TestDatasourceDraftScopeAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int32
	var seenCredential atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seenCredential.Store(r.Header.Get("Authorization"))
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		status int
		want   string
		envs   []string
	}{
		{"omitted preserves", func(b map[string]any) { b["datasource_id"] = 10 }, 200, "Bearer stored-test-token", []string{"dev"}},
		{"blank preserves", func(b map[string]any) { b["datasource_id"], b["secret"] = 10, "" }, 200, "Bearer stored-test-token", []string{"dev"}},
		{"replacement", func(b map[string]any) { b["datasource_id"], b["secret"] = "10", "new-test-token" }, 200, "Bearer new-test-token", []string{"dev"}},
		{"new cannot inherit", nil, 400, "", []string{"dev"}},
		{"cross cluster", func(b map[string]any) { b["datasource_id"] = 20 }, 403, "", []string{"dev"}},
		{"central cannot inherit", func(b map[string]any) { b["datasource_id"] = 30 }, 400, "", []string{"dev"}},
		{"central even with fresh secret", func(b map[string]any) { b["datasource_id"], b["secret"] = 30, "new-test-token" }, 400, "", []string{"dev"}},
		{"wrong type", func(b map[string]any) { b["datasource_id"] = 40 }, 400, "", []string{"dev"}},
		{"unknown datasource", func(b map[string]any) { b["datasource_id"] = 99 }, 404, "", []string{"dev"}},
		{"forbidden environment", nil, 403, "", []string{"prod"}},
		{"body env mismatch", func(b map[string]any) { b["env"] = "prod" }, 400, "", nil},
		{"body name mismatch", func(b map[string]any) { b["options"].(map[string]any)["cluster_name"] = "dev-b" }, 400, "", nil},
		{"name-only owner mismatch", func(b map[string]any) {
			opts := b["options"].(map[string]any)
			delete(opts, "cluster_id")
			opts["cluster_name"] = "dev-b"
		}, 403, "", nil},
		{"changed endpoint", func(b map[string]any) { b["datasource_id"], b["url"] = 10, srv.URL+"/other" }, 400, "", nil},
		{"changed authentication", func(b map[string]any) {
			b["datasource_id"], b["auth_type"], b["username"] = 10, "basic", "test-user"
		}, 400, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			opts := func(id string) map[string]any { return map[string]any{"scope": "cluster", "cluster_id": id} }
			sources := map[int64]*DataSource{
				10: {ID: 10, Type: "prometheus", URL: srv.URL, Env: "dev", AuthType: "bearer", secretPlain: "stored-test-token", Options: opts("1")},
				20: {ID: 20, Type: "prometheus", URL: srv.URL, Env: "dev", AuthType: "bearer", secretPlain: "foreign-test-token", Options: opts("2")},
				30: {ID: 30, Type: "prometheus", URL: srv.URL, AuthType: "bearer", secretPlain: "central-test-token", Options: map[string]any{
					"scope": "central", "cluster_label": "cluster", "cluster_values": map[string]string{"1": "dev-a"}}},
				40: {ID: 40, Type: "tempo", URL: srv.URL, Env: "dev", Options: opts("1")},
			}
			db, deps := newDraftProtocolDB(t, sources)
			p := &rbac.Principal{Permissions: []string{PermDSManage}, EnvScope: tc.envs}
			// The central source is readable to this caller, but cannot seed an own binding.
			if strings.HasPrefix(tc.name, "central") {
				p.EnvScope = nil
			}
			r := draftTestRouter(New(), deps, p)
			body := draftTestBody(srv.URL, "prometheus")
			body["auth_type"] = "bearer"
			if tc.change != nil {
				tc.change(body)
			}
			w := draftTestRequest(t, r, "?cluster_id=1", body)
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d", w.Code, tc.status)
			}
			if tc.status == http.StatusOK {
				if !draftTestResult(t, w).OK || calls.Load() != 1 || seenCredential.Load() != tc.want {
					t.Fatal("stored credential was not preserved or replaced correctly")
				}
			} else if calls.Load() != 0 {
				t.Fatal("invalid scope or credentials reached upstream")
			}
			if db.writes.Load() != 0 || strings.Contains(w.Body.String(), "test-token") {
				t.Fatal("draft wrote to the database or disclosed a credential")
			}
		})
	}
}

func TestDatasourceDraftFailureTimeoutAndRedirectAreSanitized(t *testing.T) {
	for _, mode := range []string{"failure", "invalid-json", "timeout", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var leakedCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				leakedCalls.Add(1)
				io.WriteString(w, `{}`)
			}))
			defer target.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "failure":
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"error":"secret-upstream-credential"}`)
				case "invalid-json":
					io.WriteString(w, "secret-upstream-credential")
				case "timeout":
					<-r.Context().Done()
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
				}
			}))
			defer srv.Close()
			db, deps := newDraftProtocolDB(t, nil)
			r := draftTestRouter(New(), deps, &rbac.Principal{Permissions: []string{PermDSManage}})
			body := draftTestBody(srv.URL, "prometheus")
			body["timeout_sec"], body["auth_type"], body["secret"] = 1, "bearer", "draft-test-token"
			start := time.Now()
			w := draftTestRequest(t, r, "?cluster_id=1", body)
			result := draftTestResult(t, w)
			if result.OK || result.Error == "" || strings.Contains(w.Body.String(), "credential") ||
				strings.Contains(w.Body.String(), "draft-test-token") || strings.Contains(w.Body.String(), srv.URL) {
				t.Fatal("failure result was not sanitized")
			}
			if time.Since(start) > 3*time.Second || leakedCalls.Load() != 0 || db.writes.Load() != 0 {
				t.Fatal("health check exceeded its budget, followed a redirect or wrote data")
			}
		})
	}
}

func TestDatasourceHasSecretMetadataKeepsMask(t *testing.T) {
	db, deps := newDraftProtocolDB(t, map[int64]*DataSource{
		10: {ID: 10, Type: "prometheus", AuthType: "bearer", secretPlain: "stored-test-token", Env: "dev"},
		11: {ID: 11, Type: "prometheus", Env: "dev"},
	})
	m := New()
	m.d = deps
	for _, id := range []int64{10, 11} {
		ds, err := m.loadDS(context.Background(), &rbac.Principal{}, id)
		if err != nil {
			t.Fatal("load datasource through protocol fixture")
		}
		if ds.HasSecret != (id == 10) {
			t.Fatal("incorrect has_secret metadata")
		}
		if id == 10 && ds.Secret != matrixcrypto.Mask("stored-test-token") {
			t.Fatal("existing secret mask changed")
		}
		raw, err := json.Marshal(ds)
		if err != nil || bytes.Contains(raw, []byte("stored-test-token")) {
			t.Fatal("datasource JSON exposed plaintext")
		}
	}
	if db.writes.Load() != 0 {
		t.Fatal("metadata read wrote data")
	}
}

// This protocol fixture runs the production pgx/authorization/handler path.
// It accepts only known SELECTs; it is not proof of a real PostgreSQL deployment.
type draftProtocolDB struct {
	writes      atomic.Int32
	sourceReads atomic.Int32
}

func newDraftProtocolDB(t *testing.T, sources map[int64]*DataSource) (*draftProtocolDB, *app.Deps) {
	t.Helper()
	cipher, err := matrixcrypto.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	encrypted := map[int64]string{}
	for id, ds := range sources {
		if ds.secretPlain != "" {
			encrypted[id], err = cipher.Encrypt(ds.secretPlain)
			if err != nil {
				t.Fatal("encrypt fixture credential")
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	db := &draftProtocolDB{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				for key, value := range map[string]string{"server_version": "16.0", "client_encoding": "UTF8", "standard_conforming_strings": "on"} {
					backend.Send(&pgproto3.ParameterStatus{Name: key, Value: value})
				}
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if backend.Flush() != nil {
					return
				}
				for {
					msg, err := backend.Receive()
					if err != nil {
						return
					}
					switch msg := msg.(type) {
					case *pgproto3.Query:
						db.respond(t, backend, msg.String, sources, encrypted)
						backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
						if backend.Flush() != nil {
							return
						}
					case *pgproto3.Terminate:
						return
					default:
						t.Error("unexpected protocol message")
						return
					}
				}
			}()
		}
	}()
	config, err := pgxpool.ParseConfig("postgres://draft@" + listener.Addr().String() + "/draft?sslmode=disable")
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		listener.Close()
		wg.Wait()
	})
	return db, &app.Deps{DB: pool, Crypto: cipher}
}

var draftFixtureID = regexp.MustCompile(`WHERE id\s*=\s*'?(\d+)'?`)
var draftFixtureName = regexp.MustCompile(`WHERE name\s*=\s*'([^']*)'`)

func (db *draftProtocolDB) respond(t *testing.T, backend *pgproto3.Backend, query string, sources map[int64]*DataSource, encrypted map[int64]string) {
	t.Helper()
	if !strings.HasPrefix(query, "SELECT ") {
		db.writes.Add(1)
		t.Error("draft attempted a database mutation")
		backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "25006", Message: "read only fixture"})
		return
	}
	id := int64(0)
	if match := draftFixtureID.FindStringSubmatch(query); len(match) == 2 {
		id, _ = strconv.ParseInt(match[1], 10, 64)
	}
	var fields []pgproto3.FieldDescription
	var values []string
	column := func(name string, oid uint32) {
		fields = append(fields, pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: oid, DataTypeSize: -1, TypeModifier: -1})
	}
	switch {
	case strings.Contains(query, " FROM cluster_clusters WHERE "):
		for _, cl := range []clusterIdentity{{ID: 1, Name: "dev-a", Env: "dev"}, {ID: 2, Name: "dev-b", Env: "dev"}} {
			match := draftFixtureName.FindStringSubmatch(query)
			if id == cl.ID || (len(match) == 2 && match[1] == cl.Name) {
				values = []string{strconv.FormatInt(cl.ID, 10), cl.Name, cl.Env}
				break
			}
		}
		column("id", 20)
		column("name", 25)
		column("env", 25)
	case strings.Contains(query, " FROM obs_datasources WHERE "):
		db.sourceReads.Add(1)
		for _, name := range []string{"id", "name", "type", "url", "env", "auth_type", "username", "secret_enc",
			"tls_skip_verify", "timeout_sec", "options", "has_webhook", "created_by", "created_at", "updated_at"} {
			oid := uint32(25)
			switch name {
			case "id":
				oid = 20
			case "timeout_sec":
				oid = 23
			case "tls_skip_verify", "has_webhook":
				oid = 16
			case "options":
				oid = 3802
			case "created_at", "updated_at":
				oid = 1184
			}
			column(name, oid)
		}
		if ds := sources[id]; ds != nil {
			opts, _ := json.Marshal(ds.Options)
			values = []string{strconv.FormatInt(ds.ID, 10), ds.Name, ds.Type, ds.URL, ds.Env, ds.AuthType, ds.Username,
				encrypted[id], "f", "30", string(opts), "f", "fixture", "2026-10-29 00:00:00+00", "2026-10-29 00:00:00+00"}
		}
	default:
		t.Error("unexpected draft SELECT")
		backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "0A000", Message: "unsupported fixture query"})
		return
	}
	backend.Send(&pgproto3.RowDescription{Fields: fields})
	count := 0
	if values != nil {
		row := make([][]byte, len(values))
		for i, value := range values {
			row[i] = []byte(value)
		}
		backend.Send(&pgproto3.DataRow{Values: row})
		count = 1
	}
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(fmt.Sprintf("SELECT %d", count))})
}
