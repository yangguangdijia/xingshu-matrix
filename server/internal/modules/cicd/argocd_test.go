package cicd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	matrixcrypto "github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func TestNormalizeArgoURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"https://argocd.example.com/", "https://argocd.example.com"},
		{"http://127.0.0.1:8080/argocd/", "http://127.0.0.1:8080/argocd"},
		{"HTTPS://Argo.Example.com/root", "https://Argo.Example.com/root"},
	}
	for _, tc := range ok {
		got, err := normalizeArgoURL(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("normalizeArgoURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	bad := []string{
		"",
		"argocd.example.com",
		"ftp://argocd.example.com",
		"https://user:pass@argocd.example.com",
		"https://argocd.example.com/?a=b",
		"https://argocd.example.com/#fragment",
		"https://argocd.example.com/../admin",
		"http://:8080",
		"https://",
	}
	for _, in := range bad {
		if got, err := normalizeArgoURL(in); err == nil {
			t.Fatalf("normalizeArgoURL(%q) accepted: %q", in, got)
		}
	}
}

func TestArgoEndpointEncoding(t *testing.T) {
	got, err := argoEndpoint("https://argocd.example.com/root/", "/api/v1/applications/web app", url.Values{
		"projects": {"team a&b"},
		"selector": {"app.kubernetes.io/name=web"},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/root/api/v1/applications/web app" {
		t.Fatalf("unexpected path: %q (%s)", u.Path, got)
	}
	if q := u.Query(); q.Get("projects") != "team a&b" || q.Get("selector") != "app.kubernetes.io/name=web" {
		t.Fatalf("query not encoded/decoded correctly: %q", u.RawQuery)
	}
	if strings.Contains(got, "//api") {
		t.Fatalf("double slash in endpoint: %s", got)
	}
	if _, err := argoEndpoint("ftp://argocd.example.com", "/api/v1/applications", nil); err == nil {
		t.Fatal("non-http base URL accepted")
	}
	if _, err := argoEndpoint("https://user:pass@argocd.example.com", "/api/v1/applications", nil); err == nil {
		t.Fatal("base URL with credentials accepted")
	}
}

func TestArgoValidationHelpers(t *testing.T) {
	for _, env := range []string{"prod", "dev", "team_a-1", "生产", "Prod", "p"} {
		if !validArgoEnv(env) {
			t.Fatalf("valid env %q rejected", env)
		}
	}
	for _, env := range []string{"", "prod!", "prod test", "../prod"} {
		if validArgoEnv(env) {
			t.Fatalf("invalid env %q accepted", env)
		}
	}
	for _, app := range []string{"web", "my.app-1", "a"} {
		if !validArgoApp(app) {
			t.Fatalf("valid app %q rejected", app)
		}
	}
	for _, app := range []string{"", "../etc", "a/b", "a b", "a..b", strings.Repeat("a", 254)} {
		if validArgoApp(app) {
			t.Fatalf("invalid app %q accepted", app)
		}
	}
	for _, p := range []string{"default", "team-a"} {
		if !validArgoProject(p) {
			t.Fatalf("valid project %q rejected", p)
		}
	}
	for _, p := range []string{"", "bad/project", "a..b"} {
		if validArgoProject(p) {
			t.Fatalf("invalid project %q accepted", p)
		}
	}
	for _, rev := range []string{"v1.2.3", "release/1.0", "abc123", strings.Repeat("a", argoMaxRevision)} {
		if !validArgoRevision(rev) {
			t.Fatalf("valid revision %q rejected", rev)
		}
	}
	for _, rev := range []string{"", " bad", "bad\nrev", "a\x00b", strings.Repeat("a", argoMaxRevision+1)} {
		if validArgoRevision(rev) {
			t.Fatalf("invalid revision %q accepted", rev)
		}
	}
}

func TestValidateArgoConfigInput(t *testing.T) {
	in := argoConfigIn{
		Name: " prod ", URL: "https://argocd.example.com/root/",
		Env: " prod ", Project: "", Token: "s3cret",
	}
	gotURL, err := validateArgoConfigInput(&in, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "https://argocd.example.com/root" || in.URL != gotURL {
		t.Fatalf("url not normalised: %q / %q", gotURL, in.URL)
	}
	if in.Name != "prod" || in.Env != "prod" || in.Project != "default" || in.Token != "s3cret" {
		t.Fatalf("input not normalised: %+v", in)
	}
	masked := argoConfigIn{
		Name: "prod", URL: "https://argocd.example.com", Env: "prod", Token: argoTokenMask,
	}
	if _, err := validateArgoConfigInput(&masked, true); err == nil {
		t.Fatal("token mask accepted on create")
	}
	if _, err := validateArgoConfigInput(&masked, false); err != nil {
		t.Fatalf("token mask rejected on update: %v", err)
	}
	if masked.Token != argoTokenMask {
		t.Fatalf("update mask must be preserved for the update handler: %q", masked.Token)
	}
	base := func() argoConfigIn {
		return argoConfigIn{Name: "prod", URL: "https://argocd.example.com", Env: "prod", Project: "default"}
	}
	bad := []func(*argoConfigIn){
		func(v *argoConfigIn) { v.Name = " " },
		func(v *argoConfigIn) { v.URL = "ftp://argocd.example.com" },
		func(v *argoConfigIn) { v.Env = "bad env" },
		func(v *argoConfigIn) { v.Project = "bad/project" },
		func(v *argoConfigIn) { v.Token = "line1\nline2" },
		func(v *argoConfigIn) { v.Token = strings.Repeat("t", argoMaxToken+1) },
	}
	for i, mutate := range bad {
		v := base()
		mutate(&v)
		if _, err := validateArgoConfigInput(&v, true); err == nil {
			t.Fatalf("bad input %d accepted: %+v", i, v)
		}
	}
}

func TestArgoConfigPublicMasksToken(t *testing.T) {
	cfg := &argoConfig{ID: 1, Name: "prod", URL: "https://argocd.example.com", Env: "prod", Project: "default", tokenEnc: "enc:v1:secret"}
	pub := cfg.public()
	if pub.Token != argoTokenMask {
		t.Fatalf("token not masked: %q", pub.Token)
	}
	if strings.Contains(pub.Token, "secret") {
		t.Fatal("masked token leaks the secret")
	}
	if cfg.tokenEnc != "enc:v1:secret" {
		t.Fatal("public() mutated the source config")
	}
	if (&argoConfig{}).public().Token != "" {
		t.Fatal("config without a token must not report one")
	}
}

func TestArgoTokenEncryption(t *testing.T) {
	cipher := argoTestCipher(t)
	m := &Module{d: &app.Deps{Crypto: cipher}}
	const token = "s3cret-argo-token"
	enc, err := m.encryptArgoToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if enc == "" || strings.Contains(enc, token) {
		t.Fatalf("token was not encrypted: %q", enc)
	}
	cfg := &argoConfig{tokenEnc: enc}
	got, err := m.argoToken(cfg)
	if err != nil || got != token {
		t.Fatalf("decrypted token = %q, %v", got, err)
	}
}

func TestReadArgoBodyLimit(t *testing.T) {
	b, err := readArgoBody(strings.NewReader("12345"), 5)
	if err != nil || string(b) != "12345" {
		t.Fatalf("limit boundary: %q %v", b, err)
	}
	if _, err := readArgoBody(strings.NewReader("123456"), 5); !errors.Is(err, errArgoBodyTooLarge) {
		t.Fatalf("oversized body not rejected: %v", err)
	}
}

func TestArgoSyncPayloadHasNoToken(t *testing.T) {
	b, err := json.Marshal(argoSyncPayload{ConfigID: 7, App: "web", Revision: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("approval payload has unexpected fields: %s", b)
	}
	for _, key := range []string{"config_id", "app", "revision"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("approval payload missing %q: %s", key, b)
		}
	}
	if _, ok := fields["token"]; ok || strings.Contains(strings.ToLower(string(b)), "token") {
		t.Fatalf("approval payload contains a token: %s", b)
	}
}

func TestResolveArgoRevisionAndProject(t *testing.T) {
	app := &argoApplication{}
	app.Metadata.Name = "web"
	app.Spec.Project = "team-a"
	app.Spec.Source = &argoAppSource{TargetRevision: "v1.2.3"}
	sha := strings.Repeat("a", 40)
	app.Status.Sync.Revision = sha

	if rev, err := resolveArgoRevision(app, ""); err != nil || rev != sha {
		t.Fatalf("default revision: %q %v", rev, err)
	}
	if rev, err := resolveArgoRevision(app, sha); err != nil || rev != sha {
		t.Fatalf("requested revision: %q %v", rev, err)
	}
	app.Spec.Source.TargetRevision = ""
	app.Status.Sync.Revision = sha
	if rev, err := resolveArgoRevision(app, ""); err != nil || rev != sha {
		t.Fatalf("status revision fallback: %q %v", rev, err)
	}
	app.Status.Sync.Revision = ""
	if _, err := resolveArgoRevision(app, ""); err == nil {
		t.Fatal("unresolved HEAD must not enter an approval")
	}
	if _, err := resolveArgoRevision(app, "main"); err == nil {
		t.Fatal("mutable branch accepted")
	}
	app.Spec.Source.Chart = "redis"
	if rev, err := resolveArgoRevision(app, "1.2.3"); err != nil || rev != "1.2.3" {
		t.Fatal("exact chart version rejected")
	}
	app.Spec.Source = nil
	app.Spec.Sources = []argoAppSource{{TargetRevision: "src-v2"}}
	if _, err := resolveArgoRevision(app, sha); err == nil {
		t.Fatal("multi-source app must not apply one revision to all sources")
	}
	if _, err := resolveArgoRevision(app, "bad\nrev"); err == nil {
		t.Fatal("invalid revision accepted")
	}

	cfg := &argoConfig{Project: "team-a"}
	if err := validateArgoAppProject(cfg, app); err != nil {
		t.Fatalf("matching project rejected: %v", err)
	}
	cfg.Project = "team-b"
	if err := validateArgoAppProject(cfg, app); err == nil {
		t.Fatal("project mismatch accepted")
	}
	app.Spec.Project = ""
	cfg.Project = "default"
	if err := validateArgoAppProject(cfg, app); err != nil {
		t.Fatalf("default project rejected: %v", err)
	}
}

func argoTestCipher(t *testing.T) *matrixcrypto.Cipher {
	t.Helper()
	c, err := matrixcrypto.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegisterArgoRoutesRegistersApprovalType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := approval.New(nil, nil, nil)
	m := &Module{d: &app.Deps{Approval: svc}}
	r := gin.New()
	m.registerArgoRoutes(r.Group("/cicd"))

	var registered bool
	for _, spec := range svc.Types() {
		if spec.Type != argoApprovalType {
			continue
		}
		registered = true
		if spec.CreatePermission != "cicd:deploy" || spec.Handler == nil {
			t.Fatalf("unexpected approval spec: %+v", spec)
		}
	}
	if !registered {
		t.Fatalf("approval type %q was not registered", argoApprovalType)
	}
	var syncRoute bool
	for _, route := range r.Routes() {
		if route.Method == http.MethodPost && route.Path == "/cicd/argocd/:id/applications/:app/sync" {
			syncRoute = true
		}
	}
	if !syncRoute {
		t.Fatal("argocd sync route was not mounted")
	}
}

func TestBindArgoSyncIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oversized := `{"revision":"` + strings.Repeat("a", 64<<10) + `"}`
	cases := []struct {
		name, body, revision string
		ok                   bool
	}{
		{name: "empty", body: "", ok: true},
		{name: "revision", body: `{"revision":"v1.2.3","reason":"release"}`, revision: "v1.2.3", ok: true},
		{name: "invalid json", body: `{`, ok: false},
		{name: "oversized", body: oversized, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			in, ok := bindArgoSyncIn(c)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (status %d)", ok, tc.ok, w.Code)
			}
			if ok && in.Revision != tc.revision {
				t.Fatalf("revision = %q, want %q", in.Revision, tc.revision)
			}
			if !ok && w.Code != http.StatusBadRequest {
				t.Fatalf("invalid body status = %d, want 400", w.Code)
			}
		})
	}
}

func TestArgoSyncHandlerRejectsInvalidPayload(t *testing.T) {
	h := &argoSyncHandler{m: &Module{}}
	payloads := []string{
		`not-json`,
		`{}`,
		`{"config_id":0,"app":"web","revision":"v1"}`,
		`{"config_id":1,"app":"../web","revision":"v1"}`,
		`{"config_id":1,"app":"web","revision":""}`,
	}
	for _, payload := range payloads {
		ticket := &approval.Ticket{Payload: json.RawMessage(payload)}
		if _, err := h.OnApproved(context.Background(), ticket); err == nil {
			t.Fatalf("invalid payload accepted: %s", payload)
		}
	}
	if _, err := h.OnApproved(context.Background(), nil); err == nil {
		t.Fatal("nil ticket accepted")
	}
}

func TestArgoSyncHandlerAuthorizeFallback(t *testing.T) {
	h := &argoSyncHandler{m: &Module{}}
	ticket := &approval.Ticket{ApplicantID: 7, ApplicantName: "dev"}
	cfg := &argoConfig{Env: "prod"}
	withPerms := func(perms, scope []string) context.Context {
		return rbac.WithPrincipal(context.Background(), &rbac.Principal{
			UserID: 7, Username: "dev", Permissions: perms, EnvScope: scope,
		})
	}
	if err := h.authorize(withPerms([]string{"cicd:deploy"}, []string{"prod"}), ticket, cfg); err != nil {
		t.Fatalf("authorized applicant rejected: %v", err)
	}
	if err := h.authorize(withPerms([]string{"cicd:read"}, []string{"prod"}), ticket, cfg); err == nil {
		t.Fatal("missing deploy permission accepted")
	}
	if err := h.authorize(withPerms([]string{"cicd:deploy"}, []string{"dev"}), ticket, cfg); err == nil {
		t.Fatal("out-of-scope environment accepted")
	}
	if err := h.authorize(context.Background(), ticket, cfg); err == nil {
		t.Fatal("missing RBAC accepted")
	}
}

func TestArgoClientRequests(t *testing.T) {
	const token = "s3cr3t-argo-token"
	cipher := argoTestCipher(t)
	enc, err := cipher.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}

	const commit = "0123456789abcdef0123456789abcdef01234567"
	const appJSON = `{"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"team-a","source":{"repoURL":"https://git.example/app.git","targetRevision":"v1.2.3"}},"status":{"sync":{"status":"OutOfSync","revision":"` + commit + `"},"health":{"status":"Healthy"}}}`
	const otherJSON = `{"metadata":{"name":"other"},"spec":{"project":"team-b","source":{"targetRevision":"v9"}}}`
	const listJSON = `{"items":[` + appJSON + `,` + otherJSON + `]}`

	var mu sync.Mutex
	var listQuery string
	var syncBody map[string]any
	var synced bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/argocd")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"bad token ` + token + `"}`))
			return
		}
		switch path {
		case "/api/v1/session/userinfo":
			_, _ = w.Write([]byte(`{"loggedIn":true,"username":"deployer"}`))
		case "/api/v1/applications":
			mu.Lock()
			listQuery = r.URL.RawQuery
			mu.Unlock()
			_, _ = w.Write([]byte(listJSON))
		case "/api/v1/applications/web":
			_, _ = w.Write([]byte(appJSON))
		case "/api/v1/applications/bad":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid token ` + token + `"}`))
		case "/api/v1/applications/web/sync":
			if r.Method != http.MethodPost {
				t.Errorf("sync method = %s", r.Method)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode sync body: %v", err)
			}
			mu.Lock()
			syncBody = body
			synced = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"application":{"metadata":{"name":"web"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := &Module{http: srv.Client(), d: &app.Deps{Crypto: cipher}}
	cfg := &argoConfig{
		ID: 7, Name: "prod", URL: srv.URL + "/argocd/", Env: "prod", Project: "team-a", tokenEnc: enc,
	}
	ctx := context.Background()

	items, err := m.argoListApps(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(string(items[0]), `"name":"web"`) {
		t.Fatalf("project filtering failed: %v", items)
	}
	mu.Lock()
	q := listQuery
	mu.Unlock()
	values, err := url.ParseQuery(q)
	if err != nil || values.Get("projects") != "team-a" {
		t.Fatalf("list query = %q, %v", q, err)
	}

	raw, parsed, err := m.argoGetApplication(ctx, cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || parsed.projectName() != "team-a" {
		t.Fatalf("unexpected application: %s / %+v", string(raw), parsed)
	}
	if err := validateArgoAppProject(cfg, parsed); err != nil {
		t.Fatal(err)
	}
	rev, err := resolveArgoRevision(parsed, "")
	if err != nil || rev != commit {
		t.Fatalf("resolved revision = %q, %v", rev, err)
	}

	info, err := m.argoUserinfo(ctx, cfg)
	if err != nil || !info.LoggedIn || info.Username != "deployer" {
		t.Fatalf("userinfo = %+v, %v", info, err)
	}

	if err := m.argoSyncApp(ctx, cfg, "web", rev); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body, didSync := syncBody, synced
	mu.Unlock()
	if !didSync || body == nil {
		t.Fatal("sync endpoint was not called")
	}
	if body["name"] != "web" || body["revision"] != commit {
		t.Fatalf("unexpected sync body: %+v", body)
	}
	if prune, ok := body["prune"].(bool); !ok || prune {
		t.Fatalf("sync prune must be false: %+v", body)
	}
	strategy, _ := body["strategy"].(map[string]any)
	apply, _ := strategy["apply"].(map[string]any)
	if force, ok := apply["force"].(bool); !ok || force {
		t.Fatalf("sync force must be false: %+v", body)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("sync body leaked a token: %+v", body)
	}

	// Upstream errors must not echo the response body (which contains the
	// token in this test) into the client-facing error.
	_, _, err = m.argoGetApplication(ctx, cfg, "bad")
	if err == nil {
		t.Fatal("expected an upstream error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("raw error leaked the token: %v", err)
	}
	mapped := argoHTTPError(err)
	if mapped == nil || strings.Contains(mapped.Error(), token) {
		t.Fatalf("mapped error leaked the token: %v", mapped)
	}

	cfgOther := *cfg
	cfgOther.Project = "team-b"
	if err := validateArgoAppProject(&cfgOther, parsed); err == nil {
		t.Fatal("project mismatch accepted")
	}
}

func TestArgoClientVerifiesTLSByDefault(t *testing.T) {
	const token = "tls-token"
	cipher := argoTestCipher(t)
	enc, err := cipher.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"loggedIn":true,"username":"deployer"}`))
	}))
	defer srv.Close()

	m := &Module{http: &http.Client{Timeout: 2 * time.Second}, d: &app.Deps{Crypto: cipher}}
	cfg := &argoConfig{ID: 1, Name: "prod", URL: srv.URL, Env: "prod", Project: "default", tokenEnc: enc}
	if _, err := m.argoUserinfo(context.Background(), cfg); err == nil {
		t.Fatal("self-signed TLS certificate was accepted")
	}
}
