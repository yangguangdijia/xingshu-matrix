package apidoc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

func noop(c *gin.Context) {}

// fakeAuth mimics the real auth middleware: a capturing closure, so it has its
// own identity distinct from other middleware.
func fakeAuth(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("who", tag)
		c.Next()
	}
}

func smallEngine() (*gin.Engine, *gin.RouterGroup) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(gin.Recovery())
	e.GET("/healthz", noop)
	api := e.Group(APIPrefix)
	api.Use(func(c *gin.Context) { c.Next() }) // stands in for the audit middleware
	public := api.Group("")
	public.POST("/auth/login", noop)
	public.GET("/settings/public/branding", noop)
	authed := api.Group("")
	authed.Use(fakeAuth("auth"))
	authed.GET("/auth/me", noop)
	authed.GET("/clusters", rbac.RequirePerm("cluster:read"), noop)
	authed.GET("/clusters/:id", rbac.RequirePerm("cluster:read"), rbac.RequireClusterAccess("id"), noop)
	authed.DELETE("/clusters/:id", rbac.RequirePerm("cluster:write", "cluster:read"), noop)
	authed.POST("/approvals/:id/approve", rbac.RequireAnyPerm("approval:approve", "*:approve"), noop)
	authed.POST("/hosts/:id/exec", rbac.RequireEnvPerm("host:exec", func(*gin.Context) string { return "" }), noop)
	g := authed.Group("/bastion")
	g.Use(rbac.RequirePerm("bastion:read"))
	g.GET("/sessions/:sid/files/*path", noop)
	return e, authed
}

func find(t *testing.T, rs []Route, method, path string) Route {
	t.Helper()
	for _, r := range rs {
		if r.Method == method && r.Path == path {
			return r
		}
	}
	t.Fatalf("route %s %s not found", method, path)
	return Route{}
}

func TestCollectSmallEngine(t *testing.T) {
	e, authed := smallEngine()
	rs := Collect(e, AuthMarkers(authed, e))
	if len(rs) != len(e.Routes()) {
		t.Fatalf("got %d routes, want %d", len(rs), len(e.Routes()))
	}
	for _, r := range rs {
		if r.Access.Inferred {
			t.Fatalf("%s %s: chain not read (gin internals changed?)", r.Method, r.Path)
		}
	}
	if r := find(t, rs, "POST", "/api/v1/auth/login"); !r.Access.Public || r.Tag != "auth" {
		t.Fatalf("login: %+v", r)
	}
	if r := find(t, rs, "GET", "/healthz"); !r.Access.Public || r.Tag != "system" {
		t.Fatalf("healthz: %+v", r)
	}
	if r := find(t, rs, "GET", "/api/v1/auth/me"); r.Access.Public || len(r.Access.Permissions) != 0 {
		t.Fatalf("me: %+v", r)
	}
	r := find(t, rs, "GET", "/api/v1/clusters/{id}")
	if r.Access.Public || strings.Join(r.Access.Permissions, ",") != "cluster:read" || strings.Join(r.Access.DataScopes, ",") != "cluster" {
		t.Fatalf("cluster get: %+v", r)
	}
	if len(r.Params) != 1 || r.Params[0] != "id" || r.Tag != "clusters" || r.OperationID != "getClustersById" {
		t.Fatalf("cluster get meta: %+v", r)
	}
	if r := find(t, rs, "DELETE", "/api/v1/clusters/{id}"); strings.Join(r.Access.Permissions, ",") != "cluster:write,cluster:read" {
		t.Fatalf("cluster delete: %+v", r)
	}
	if r := find(t, rs, "POST", "/api/v1/approvals/{id}/approve"); len(r.Access.AnyOf) != 1 || len(r.Access.AnyOf[0]) != 2 {
		t.Fatalf("approve: %+v", r)
	}
	if r := find(t, rs, "POST", "/api/v1/hosts/{id}/exec"); !r.Access.EnvScoped || r.Access.Permissions[0] != "host:exec" {
		t.Fatalf("exec: %+v", r)
	}
	r = find(t, rs, "GET", "/api/v1/bastion/sessions/{sid}/files/{path}")
	if strings.Join(r.Access.Permissions, ",") != "bastion:read" || strings.Join(r.Params, ",") != "sid,path" || r.Tag != "bastion" {
		t.Fatalf("group middleware: %+v", r)
	}
}

func TestRegisterServesCachedDocument(t *testing.T) {
	e, authed := smallEngine()
	Register(authed, e)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: got %d, want 401", w.Code)
	}
	var first string
	for i := 0; i < 2; i++ {
		req = httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer x")
		w = httptest.NewRecorder()
		e.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("authed: %d %s", w.Code, w.Body.String())
		}
		if i == 0 {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Fatal("document not cached")
		}
	}
	var doc Document
	if err := json.Unmarshal([]byte(first), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("openapi = %q", doc.OpenAPI)
	}
	op, ok := doc.Paths["/api/v1/clusters/{id}"]["get"]
	if !ok || op.Permissions[0] != "cluster:read" || len(op.Parameters) != 1 || op.Parameters[0].In != "path" {
		t.Fatalf("op: %+v", op)
	}
	if login := doc.Paths["/api/v1/auth/login"]["post"]; !login.Access.Public || len(login.Security) != 0 {
		t.Fatalf("login: %+v", login)
	}
	if self, ok := doc.Paths["/api/v1/openapi.json"]["get"]; !ok || self.Access.Public {
		t.Fatalf("openapi.json itself should be listed and require login: %+v", self)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json?envelope=1", nil)
	req.Header.Set("Authorization", "Bearer x")
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	var env struct {
		Code int      `json:"code"`
		Data Document `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Code != 0 || len(env.Data.Paths) != len(doc.Paths) {
		t.Fatalf("envelope: %v %s", err, w.Body.String())
	}
}

// Building the production engine needs a database and every module's deps, so
// the scale test drives the generator with >400 synthetic routes on a real gin
// engine instead.
func TestManySyntheticRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	api := e.Group(APIPrefix)
	pub := api.Group("")
	authed := api.Group("")
	authed.Use(fakeAuth("a"))
	pub.POST("/auth/login", noop)
	want := 1
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	for m := 0; m < 30; m++ {
		mod := fmt.Sprintf("mod%02d", m)
		g := authed.Group("/" + mod)
		for k := 0; k < 3; k++ {
			res := fmt.Sprintf("res-%d", k)
			for _, meth := range methods {
				perm := mod + ":" + strings.ToLower(meth)
				g.Handle(meth, "/"+res+"/:id", rbac.RequirePerm(perm), noop)
				want++
			}
			g.GET("/"+res, rbac.RequireAnyPerm(mod+":read", "*:read"), noop)
			want++
		}
	}
	rs := Collect(e, AuthMarkers(authed, e))
	if len(rs) != want || want <= 400 {
		t.Fatalf("got %d routes, want %d (>400)", len(rs), want)
	}
	ids := map[string]bool{}
	tags := map[string]bool{}
	for _, r := range rs {
		if ids[r.OperationID] {
			t.Fatalf("duplicate operationId %s", r.OperationID)
		}
		ids[r.OperationID] = true
		tags[r.Tag] = true
		if r.Access.Inferred {
			t.Fatalf("chain missing for %s %s", r.Method, r.Path)
		}
		if strings.Contains(r.Path, ":") {
			t.Fatalf("gin param left in %s", r.Path)
		}
		if r.Path == "/api/v1/auth/login" {
			if !r.Access.Public {
				t.Fatal("login should be public")
			}
			continue
		}
		if r.Access.Public {
			t.Fatalf("%s %s should require login", r.Method, r.Path)
		}
		if strings.HasSuffix(r.Path, "{id}") {
			want := r.Tag + ":" + strings.ToLower(r.Method)
			if len(r.Access.Permissions) != 1 || r.Access.Permissions[0] != want {
				t.Fatalf("%s %s perms %v want %s", r.Method, r.Path, r.Access.Permissions, want)
			}
		} else if len(r.Access.AnyOf) != 1 {
			t.Fatalf("%s %s any-of %v", r.Method, r.Path, r.Access.AnyOf)
		}
	}
	if len(tags) != 31 {
		t.Fatalf("tags = %d, want 31", len(tags))
	}
	doc := Generate(rs, "test")
	n := 0
	for _, ops := range doc.Paths {
		n += len(ops)
	}
	if n != want {
		t.Fatalf("document has %d operations, want %d", n, want)
	}
}

func TestOperationIDDedupAndHelpers(t *testing.T) {
	in := []RouteInput{
		{Method: "GET", Path: "/api/v1/a-b/c"},
		{Method: "GET", Path: "/api/v1/a/b-c"},
		{Method: "GET", Path: "/api/v1/a/b/c"},
	}
	rs := Build(in, nil)
	seen := map[string]bool{}
	for _, r := range rs {
		if seen[r.OperationID] {
			t.Fatalf("dup %s", r.OperationID)
		}
		seen[r.OperationID] = true
		if !r.Access.Inferred {
			t.Fatal("nil chain must be marked inferred")
		}
	}
	if !seen["getABC"] || !seen["getABC_2"] {
		t.Fatalf("ids: %v", seen)
	}
	if p, ps := openAPIPath("/x/:id/*rest"); p != "/x/{id}/{rest}" || len(ps) != 2 {
		t.Fatalf("path %s %v", p, ps)
	}
	if !whitelisted("/api/v1/auth/login") || whitelisted("/api/v1/users") || !whitelisted("/healthz") {
		t.Fatal("whitelist")
	}
}
