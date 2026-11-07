// Package apidoc generates an OpenAPI 3.0 document from the live gin route
// table. Required permissions are inferred from the rbac guard registry: every
// middleware built by rbac.RequirePerm / RequireAnyPerm / RequireEnvPerm /
// Require*Access is recorded by closure identity, and the generator looks each
// route's handler chain up in that registry. Nothing is executed.
package apidoc

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unsafe"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// APIPrefix is the versioned API root; tags are taken from the segment after it.
const APIPrefix = "/api/v1"

// Access is what a route requires from the caller.
type Access struct {
	Public      bool       `json:"public"`
	Permissions []string   `json:"permissions,omitempty"` // all required
	AnyOf       [][]string `json:"any_of,omitempty"`      // at least one of each group
	EnvScoped   bool       `json:"env_scoped,omitempty"`  // environment scope also checked
	DataScopes  []string   `json:"data_scopes,omitempty"` // cluster / namespace / workspace
	// Inferred is true when the handler chain could not be read and the
	// public/permission information comes from the path whitelist only.
	Inferred bool `json:"inferred,omitempty"`
}

// Route is one documented endpoint.
type Route struct {
	Method      string `json:"method"`
	GinPath     string `json:"gin_path"`
	Path        string `json:"path"` // OpenAPI form: {id}
	Tag         string `json:"tag"`
	OperationID string `json:"operation_id"`
	Handler     string `json:"handler"`
	Params      []string `json:"params,omitempty"`
	Access      Access `json:"access"`
}

// publicWhitelist is used only when handler chains cannot be read (gin
// internals changed): paths (relative to APIPrefix) known to be unauthenticated.
var publicWhitelist = []string{
	"/health", "/version", "/auth/login", "/auth/refresh", "/auth/captcha",
	"/settings/public", "/sso/", "/recovery/",
}

// RouteInput is the raw material for one route; exported for tests that build
// synthetic route tables without an engine.
type RouteInput struct {
	Method  string
	Path    string
	Handler string
	Chain   gin.HandlersChain // nil when unknown
}

// Collect reads e's route table. authMarkers are the handlers that only the
// authenticated group carries (see AuthMarkers); a route whose chain lacks any
// of them is public.
func Collect(e *gin.Engine, authMarkers []gin.HandlerFunc) []Route {
	chains, err := readChains(e)
	in := make([]RouteInput, 0, len(chains))
	for _, ri := range e.Routes() {
		r := RouteInput{Method: ri.Method, Path: ri.Path, Handler: ri.Handler}
		if err == nil {
			r.Chain = chains[ri.Method+" "+ri.Path]
		}
		in = append(in, r)
	}
	return Build(in, authMarkers)
}

// AuthMarkers returns the handlers present on authed but not on the engine's
// global middleware (the auth middleware, password-change gate and any
// API-group middleware).
func AuthMarkers(authed *gin.RouterGroup, e *gin.Engine) []gin.HandlerFunc {
	global := map[uintptr]bool{}
	for _, h := range e.Handlers {
		global[rbac.HandlerKey(h)] = true
	}
	var out []gin.HandlerFunc
	for _, h := range authed.Handlers {
		if !global[rbac.HandlerKey(h)] {
			out = append(out, h)
		}
	}
	return out
}

// Build turns raw route inputs into documented routes, sorted by path then
// method, with unique operation ids.
func Build(in []RouteInput, authMarkers []gin.HandlerFunc) []Route {
	markers := make([]uintptr, 0, len(authMarkers))
	for _, h := range authMarkers {
		markers = append(markers, rbac.HandlerKey(h))
	}
	out := make([]Route, 0, len(in))
	for _, ri := range in {
		r := Route{
			Method:  strings.ToUpper(ri.Method),
			GinPath: ri.Path,
			Handler: ri.Handler,
		}
		r.Path, r.Params = openAPIPath(ri.Path)
		r.Tag = tagOf(ri.Path)
		r.Access = accessOf(ri, markers)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return methodRank(out[i].Method) < methodRank(out[j].Method)
	})
	used := map[string]bool{}
	for i := range out {
		base := operationID(out[i].Method, out[i].GinPath)
		id := base
		for n := 2; used[id]; n++ {
			id = fmt.Sprintf("%s_%d", base, n)
		}
		used[id] = true
		out[i].OperationID = id
	}
	return out
}

func methodRank(m string) int {
	for i, x := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if m == x {
			return i
		}
	}
	return 99
}

func accessOf(ri RouteInput, markers []uintptr) Access {
	var a Access
	if ri.Chain == nil {
		a.Inferred = true
		a.Public = whitelisted(ri.Path)
		return a
	}
	have := make(map[uintptr]bool, len(ri.Chain))
	for _, h := range ri.Chain {
		have[rbac.HandlerKey(h)] = true
	}
	a.Public = len(markers) == 0 && whitelisted(ri.Path)
	if len(markers) > 0 {
		a.Public = false
		for _, m := range markers {
			if !have[m] {
				a.Public = true
				break
			}
		}
	}
	seenPerm, seenScope := map[string]bool{}, map[string]bool{}
	for _, h := range ri.Chain {
		g, ok := rbac.LookupGuard(h)
		if !ok {
			continue
		}
		switch g.Kind {
		case "all", "env":
			for _, p := range g.Perms {
				if !seenPerm[p] {
					seenPerm[p] = true
					a.Permissions = append(a.Permissions, p)
				}
			}
			if g.Kind == "env" {
				a.EnvScoped = true
			}
		case "any":
			if len(g.Perms) > 0 {
				a.AnyOf = append(a.AnyOf, g.Perms)
			}
		case "scope":
			if g.Scope != "" && !seenScope[g.Scope] {
				seenScope[g.Scope] = true
				a.DataScopes = append(a.DataScopes, g.Scope)
			}
		}
	}
	return a
}

func whitelisted(p string) bool {
	if !strings.HasPrefix(p, APIPrefix+"/") {
		return true // /healthz, /metrics and other non-API endpoints
	}
	rel := strings.TrimPrefix(p, APIPrefix)
	for _, w := range publicWhitelist {
		if rel == w || (strings.HasSuffix(w, "/") && strings.HasPrefix(rel, w)) {
			return true
		}
	}
	return false
}

// openAPIPath converts gin's :id and *rest segments to {id} / {rest}.
func openAPIPath(p string) (string, []string) {
	segs := strings.Split(p, "/")
	var params []string
	for i, s := range segs {
		if len(s) > 1 && (s[0] == ':' || s[0] == '*') {
			params = append(params, s[1:])
			segs[i] = "{" + s[1:] + "}"
		}
	}
	return strings.Join(segs, "/"), params
}

func tagOf(p string) string {
	if !strings.HasPrefix(p, APIPrefix+"/") {
		return "system"
	}
	rest := strings.TrimPrefix(p, APIPrefix+"/")
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" || seg[0] == ':' || seg[0] == '*' {
		return "system"
	}
	return seg
}

func operationID(method, p string) string {
	rel := strings.TrimPrefix(p, APIPrefix)
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, s := range strings.Split(rel, "/") {
		if s == "" {
			continue
		}
		prefix := ""
		if s[0] == ':' || s[0] == '*' {
			prefix, s = "By", s[1:]
		}
		b.WriteString(prefix)
		for _, w := range strings.FieldsFunc(s, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) {
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return b.String()
}

// readChains walks gin's radix trees (unexported) and returns the full handler
// chain of every route keyed by "METHOD path". Any shape mismatch (a future gin
// version) returns an error and the caller falls back to the path whitelist.
func readChains(e *gin.Engine) (out map[string]gin.HandlersChain, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("apidoc: read gin trees: %v", r)
		}
	}()
	ev := reflect.ValueOf(e).Elem()
	trees := ev.FieldByName("trees")
	if !trees.IsValid() || trees.Kind() != reflect.Slice {
		return nil, fmt.Errorf("apidoc: gin engine has no trees slice")
	}
	out = map[string]gin.HandlersChain{}
	chainType := reflect.TypeOf(gin.HandlersChain(nil))
	var walk func(method string, n reflect.Value, depth int) error
	walk = func(method string, n reflect.Value, depth int) error {
		if depth > 512 {
			return fmt.Errorf("apidoc: tree too deep")
		}
		if n.Kind() != reflect.Pointer || n.IsNil() {
			return nil
		}
		n = n.Elem()
		hf, fp, ch := n.FieldByName("handlers"), n.FieldByName("fullPath"), n.FieldByName("children")
		if !hf.IsValid() || hf.Type() != chainType || !fp.IsValid() || fp.Kind() != reflect.String || !ch.IsValid() || ch.Kind() != reflect.Slice {
			return fmt.Errorf("apidoc: unexpected gin node layout")
		}
		if hf.Len() > 0 {
			chain := *(*gin.HandlersChain)(unsafe.Pointer(hf.UnsafeAddr()))
			out[method+" "+fp.String()] = append(gin.HandlersChain(nil), chain...)
		}
		for i := 0; i < ch.Len(); i++ {
			if err := walk(method, ch.Index(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < trees.Len(); i++ {
		t := trees.Index(i)
		m, root := t.FieldByName("method"), t.FieldByName("root")
		if !m.IsValid() || m.Kind() != reflect.String || !root.IsValid() {
			return nil, fmt.Errorf("apidoc: unexpected gin method tree layout")
		}
		if err := walk(m.String(), root, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}
