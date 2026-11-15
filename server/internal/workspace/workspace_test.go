package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ---------------------------------------------------------------- validation

func TestNormalizeName(t *testing.T) {
	ok := []string{"a", "  研发中心  ", strings.Repeat("空", 63), "Team A_1"}
	for _, s := range ok {
		if _, err := NormalizeName(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{"", "   ", strings.Repeat("x", 64), "a\nb", "a\x00b"}
	for _, s := range bad {
		if _, err := NormalizeName(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if n, _ := NormalizeName("  x  "); n != "x" {
		t.Errorf("not trimmed: %q", n)
	}
}

func TestValidateCreateUpdate(t *testing.T) {
	if _, err := ValidateCreate(CreateInput{Name: "w", CPULimitMilli: -1}); err == nil {
		t.Error("negative cpu accepted")
	}
	if _, err := ValidateCreate(CreateInput{Name: "w", Description: strings.Repeat("d", 513)}); err == nil {
		t.Error("long description accepted")
	}
	zero := int64(0)
	in, err := ValidateCreate(CreateInput{Name: "w", ManagerID: &zero})
	if err != nil || in.ManagerID != nil {
		t.Errorf("manager 0 should be nil: %v %v", in.ManagerID, err)
	}
	empty := " "
	if _, err := ValidateUpdate(UpdateInput{Name: &empty}); err == nil {
		t.Error("empty name update accepted")
	}
	neg := int64(-5)
	if _, err := ValidateUpdate(UpdateInput{MemoryLimitBytes: &neg}); err == nil {
		t.Error("negative memory accepted")
	}
	if _, err := ValidateUpdate(UpdateInput{ManagerID: &neg}); err == nil {
		t.Error("negative manager accepted")
	}
	if _, err := ValidateUpdate(UpdateInput{ManagerID: &zero}); err != nil {
		t.Errorf("manager 0 (clear) rejected: %v", err)
	}
}

func TestValidateDeleteConfirm(t *testing.T) {
	if ValidateDeleteConfirm("研发", "") == nil || ValidateDeleteConfirm("研发", "研发2") == nil {
		t.Error("bad confirm accepted")
	}
	if err := ValidateDeleteConfirm("研发", " 研发 "); err != nil {
		t.Error(err)
	}
}

func TestValidateMember(t *testing.T) {
	u, g, z := int64(3), int64(4), int64(0)
	cases := []struct {
		in MemberInput
		ok bool
	}{
		{MemberInput{UserID: &u, Role: "owner"}, true},
		{MemberInput{GroupID: &g, Role: " Viewer "}, true},
		{MemberInput{UserID: &u, GroupID: &z, Role: "admin"}, true},
		{MemberInput{UserID: &u, GroupID: &g, Role: "member"}, false},
		{MemberInput{Role: "member"}, false},
		{MemberInput{UserID: &u, Role: "root"}, false},
		{MemberInput{UserID: &u, Role: ""}, false},
	}
	for i, c := range cases {
		out, err := ValidateMember(c.in)
		if (err == nil) != c.ok {
			t.Errorf("case %d: err=%v", i, err)
		}
		if err == nil && out.GroupID != nil && *out.GroupID == 0 {
			t.Errorf("case %d: zero group kept", i)
		}
	}
	if out, _ := ValidateMember(MemberInput{GroupID: &g, Role: " Viewer "}); out.Role != "viewer" {
		t.Errorf("role not normalized: %q", out.Role)
	}
}

func TestValidateProject(t *testing.T) {
	ok := []string{"default", "a", "team-a1", strings.Repeat("a", 63)}
	for _, ns := range ok {
		if _, err := ValidateProject(ProjectInput{ClusterID: 1, Namespace: ns}); err != nil {
			t.Errorf("%q: %v", ns, err)
		}
	}
	bad := []string{"", "-a", "a-", "A", "a_b", "a.b", strings.Repeat("a", 64), "中文"}
	for _, ns := range bad {
		if _, err := ValidateProject(ProjectInput{ClusterID: 1, Namespace: ns}); err == nil {
			t.Errorf("%q accepted", ns)
		}
	}
	if _, err := ValidateProject(ProjectInput{ClusterID: 0, Namespace: "a"}); err == nil {
		t.Error("cluster 0 accepted")
	}
}

func TestNormalizeScope(t *testing.T) {
	cases := []struct {
		scope, value string
		ok           bool
		wantScope    string
	}{
		{"", "", true, "global"},
		{"global", "", true, "global"},
		{"global", "x", false, ""},
		{"env", "prod", true, "env"},
		{"env", "", false, ""},
		{"env", "Prod", false, ""},
		{"cluster", "12", true, "cluster"},
		{"cluster", "012", false, ""},
		{"cluster", "-1", false, ""},
		{"cluster", "0", false, ""},
		{"cluster", "abc", false, ""},
		{"workspace", "7", true, "workspace"},
		{"workspace", "", false, ""},
		{"PROJECT", "3/default", true, "project"},
		{"project", "3/Default", false, ""},
		{"project", "3", false, ""},
		{"project", "0/default", false, ""},
		{"project", "3/", false, ""},
		{"tenant", "1", false, ""},
	}
	for _, c := range cases {
		s, _, err := NormalizeScope(c.scope, c.value)
		if (err == nil) != c.ok {
			t.Errorf("%s/%s: err=%v", c.scope, c.value, err)
			continue
		}
		if c.ok && s != c.wantScope {
			t.Errorf("%s: got scope %s", c.scope, s)
		}
		// every accepted value must satisfy the table CHECK and the rbac resolver
		if c.ok {
			_, v, _ := NormalizeScope(c.scope, c.value)
			if (s == rbac.ScopeGlobal) != (v == "") {
				t.Errorf("%s/%s violates CHECK", s, v)
			}
			if s == rbac.ScopeProject {
				if _, ok := rbac.ParseNamespaceRef(v); !ok {
					t.Errorf("%s not parseable by rbac", v)
				}
			}
		}
	}
}

func TestFlexString(t *testing.T) {
	var in BindingInput
	if err := json.Unmarshal([]byte(`{"role_id":1,"user_id":2,"scope":"cluster","scope_value":12}`), &in); err != nil || in.ScopeValue != "12" {
		t.Fatalf("number: %q %v", in.ScopeValue, err)
	}
	if err := json.Unmarshal([]byte(`{"scope_value":"3/default"}`), &in); err != nil || in.ScopeValue != "3/default" {
		t.Fatalf("string: %q %v", in.ScopeValue, err)
	}
	if err := json.Unmarshal([]byte(`{"scope_value":null}`), &in); err != nil || in.ScopeValue != "" {
		t.Fatalf("null: %q %v", in.ScopeValue, err)
	}
	if err := json.Unmarshal([]byte(`{"scope_value":{}}`), &in); err == nil {
		t.Fatal("object accepted")
	}
}

func TestValidateBinding(t *testing.T) {
	u, g := int64(1), int64(2)
	if _, err := ValidateBinding(BindingInput{RoleID: 0, UserID: &u}); err == nil {
		t.Error("role 0 accepted")
	}
	if _, err := ValidateBinding(BindingInput{RoleID: 1, UserID: &u, GroupID: &g}); err == nil {
		t.Error("both subjects accepted")
	}
	out, err := ValidateBinding(BindingInput{RoleID: 1, GroupID: &g, Scope: "Workspace", ScopeValue: " 9 "})
	if err != nil || out.Scope != "workspace" || out.ScopeValue != "9" {
		t.Errorf("normalize: %+v %v", out, err)
	}
}

func TestOwnerRemovalAllowed(t *testing.T) {
	if OwnerRemovalAllowed(true, 0) {
		t.Error("last owner removable")
	}
	if !OwnerRemovalAllowed(true, 1) || !OwnerRemovalAllowed(false, 0) {
		t.Error("allowed case refused")
	}
}

func TestMissingPermissions(t *testing.T) {
	admin := &rbac.Principal{Permissions: []string{"*"}}
	if m := MissingPermissions(admin, []string{"*", "user:manage"}); len(m) != 0 {
		t.Errorf("admin missing %v", m)
	}
	op := &rbac.Principal{Permissions: []string{"workspace:manage", "workload:*", "*:read"}}
	m := MissingPermissions(op, []string{"workload:write", "cluster:read", "*", "user:manage"})
	if strings.Join(m, ",") != "*,user:manage" {
		t.Errorf("missing = %v", m)
	}
}

func TestBuildBindingWhere(t *testing.T) {
	w, args := buildBindingWhere(BindingFilter{})
	if w != "TRUE" || len(args) != 0 {
		t.Errorf("empty: %s %v", w, args)
	}
	w, args = buildBindingWhere(BindingFilter{Scope: "workspace", ScopeValue: "3", UserID: 5, Restricted: true})
	if !strings.Contains(w, "b.scope = $1") || !strings.Contains(w, "b.scope_value = $2") || !strings.Contains(w, "b.user_id = $3") ||
		!strings.Contains(w, "ANY($4::text[])") || !strings.Contains(w, "ANY($5::text[])") || len(args) != 5 {
		t.Errorf("where = %s args=%v", w, args)
	}
}

func TestMapErr(t *testing.T) {
	cases := map[error]int{
		ErrWorkspaceNotFound: 404,
		ErrRoleNotFound:      404,
		ErrClusterNotFound:   404,
		ErrNameTaken:         409,
		ErrMemberExists:      409,
		ErrBindingExists:     409,
		ErrLastOwner:         409,
		&NamespaceTakenError{Workspace: "研发中心"}:       409,
		&NamespaceTakenError{}:                        409,
		errors.Join(errors.New("x"), ErrUserNotFound): 404,
	}
	for in, want := range cases {
		var he *httpx.Error
		if !errors.As(mapErr(in), &he) || he.Status != want {
			t.Errorf("%v → %v", in, mapErr(in))
		}
	}
	if !strings.Contains(mapErr(&NamespaceTakenError{Workspace: "研发中心"}).Error(), "研发中心") {
		t.Error("conflict message lacks workspace name")
	}
}

// ---------------------------------------------------------------- HTTP with fake store

type fakeStore struct {
	ws          map[int64]*Workspace
	members     map[int64]*Member
	projects    map[int64]*Project
	bindings    map[int64]*RoleBinding
	rolePerms   map[int64][]string
	next        int64
	lastFilter  WorkspaceFilter
	lastBinding BindingFilter
}

func newFake() *fakeStore {
	return &fakeStore{ws: map[int64]*Workspace{}, members: map[int64]*Member{}, projects: map[int64]*Project{},
		bindings: map[int64]*RoleBinding{}, rolePerms: map[int64][]string{1: {"workload:read"}, 2: {"*"}}, next: 100}
}

func (f *fakeStore) id() int64 { f.next++; return f.next }

func (f *fakeStore) ListWorkspaces(_ context.Context, flt WorkspaceFilter, _, _ int) ([]Workspace, int64, error) {
	f.lastFilter = flt
	out := []Workspace{}
	for _, w := range f.ws {
		if flt.IDs != nil {
			found := false
			for _, id := range flt.IDs {
				found = found || id == w.ID
			}
			if !found {
				continue
			}
		}
		out = append(out, *w)
	}
	return out, int64(len(out)), nil
}
func (f *fakeStore) GetWorkspace(_ context.Context, id int64) (*Workspace, error) {
	if w, ok := f.ws[id]; ok {
		c := *w
		return &c, nil
	}
	return nil, ErrWorkspaceNotFound
}
func (f *fakeStore) CreateWorkspace(_ context.Context, in CreateInput, owner int64, by string) (*Workspace, error) {
	for _, w := range f.ws {
		if strings.EqualFold(w.Name, in.Name) {
			return nil, ErrNameTaken
		}
	}
	w := &Workspace{ID: f.id(), Name: in.Name, Description: in.Description, CreatedBy: by, CreatedAt: time.Now()}
	f.ws[w.ID] = w
	if owner > 0 {
		o := owner
		m := &Member{ID: f.id(), WorkspaceID: w.ID, UserID: &o, Kind: "user", Role: "owner"}
		f.members[m.ID] = m
	}
	return w, nil
}
func (f *fakeStore) UpdateWorkspace(_ context.Context, id int64, in UpdateInput) (*Workspace, error) {
	w, ok := f.ws[id]
	if !ok {
		return nil, ErrWorkspaceNotFound
	}
	if in.Name != nil {
		w.Name = *in.Name
	}
	return w, nil
}
func (f *fakeStore) DeleteWorkspace(_ context.Context, id int64) error {
	if _, ok := f.ws[id]; !ok {
		return ErrWorkspaceNotFound
	}
	delete(f.ws, id)
	return nil
}
func (f *fakeStore) ListMembers(_ context.Context, ws int64) ([]Member, error) {
	out := []Member{}
	for _, m := range f.members {
		if m.WorkspaceID == ws {
			out = append(out, *m)
		}
	}
	return out, nil
}
func (f *fakeStore) AddMember(_ context.Context, ws int64, in MemberInput, by string) (*Member, error) {
	if _, ok := f.ws[ws]; !ok {
		return nil, ErrWorkspaceNotFound
	}
	m := &Member{ID: f.id(), WorkspaceID: ws, UserID: in.UserID, GroupID: in.GroupID, Role: in.Role, CreatedBy: by}
	f.members[m.ID] = m
	return m, nil
}
func (f *fakeStore) owners(ws, except int64) int64 {
	var n int64
	for _, m := range f.members {
		if m.WorkspaceID == ws && m.ID != except && m.Role == "owner" {
			n++
		}
	}
	return n
}
func (f *fakeStore) UpdateMemberRole(_ context.Context, ws, mid int64, role string) (*Member, error) {
	m, ok := f.members[mid]
	if !ok || m.WorkspaceID != ws {
		return nil, ErrMemberNotFound
	}
	if m.Role == "owner" && role != "owner" && !OwnerRemovalAllowed(true, f.owners(ws, mid)) {
		return nil, ErrLastOwner
	}
	m.Role = role
	return m, nil
}
func (f *fakeStore) DeleteMember(_ context.Context, ws, mid int64) (*Member, error) {
	m, ok := f.members[mid]
	if !ok || m.WorkspaceID != ws {
		return nil, ErrMemberNotFound
	}
	if !OwnerRemovalAllowed(m.Role == "owner", f.owners(ws, mid)) {
		return nil, ErrLastOwner
	}
	delete(f.members, mid)
	return m, nil
}
func (f *fakeStore) ListProjects(_ context.Context, ws int64) ([]Project, error) {
	out := []Project{}
	for _, p := range f.projects {
		if p.WorkspaceID == ws {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (f *fakeStore) AddProject(_ context.Context, ws int64, in ProjectInput, by string) (*Project, error) {
	for _, p := range f.projects {
		if p.ClusterID == in.ClusterID && p.Namespace == in.Namespace {
			return nil, &NamespaceTakenError{Workspace: f.ws[p.WorkspaceID].Name}
		}
	}
	p := &Project{ID: f.id(), WorkspaceID: ws, ClusterID: in.ClusterID, Namespace: in.Namespace, CreatedBy: by}
	f.projects[p.ID] = p
	return p, nil
}
func (f *fakeStore) DeleteProject(_ context.Context, ws, pid int64) (*Project, error) {
	p, ok := f.projects[pid]
	if !ok || p.WorkspaceID != ws {
		return nil, ErrProjectNotFound
	}
	delete(f.projects, pid)
	return p, nil
}
func (f *fakeStore) ListBindings(_ context.Context, flt BindingFilter, _, _ int) ([]RoleBinding, int64, error) {
	f.lastBinding = flt
	out := []RoleBinding{}
	for _, b := range f.bindings {
		out = append(out, *b)
	}
	return out, int64(len(out)), nil
}
func (f *fakeStore) CreateBinding(_ context.Context, in BindingInput, by string) (*RoleBinding, error) {
	for _, b := range f.bindings {
		if b.RoleID == in.RoleID && b.Scope == in.Scope && b.ScopeValue == string(in.ScopeValue) {
			return nil, ErrBindingExists
		}
	}
	b := &RoleBinding{ID: f.id(), RoleID: in.RoleID, UserID: in.UserID, GroupID: in.GroupID, Scope: in.Scope, ScopeValue: string(in.ScopeValue), CreatedBy: by}
	f.bindings[b.ID] = b
	return b, nil
}
func (f *fakeStore) GetBinding(_ context.Context, id int64) (*RoleBinding, error) {
	if b, ok := f.bindings[id]; ok {
		return b, nil
	}
	return nil, ErrBindingNotFound
}
func (f *fakeStore) DeleteBinding(_ context.Context, id int64) (*RoleBinding, error) {
	b, ok := f.bindings[id]
	if !ok {
		return nil, ErrBindingNotFound
	}
	delete(f.bindings, id)
	return b, nil
}
func (f *fakeStore) RolePermissions(_ context.Context, id int64) ([]string, error) {
	p, ok := f.rolePerms[id]
	if !ok {
		return nil, ErrRoleNotFound
	}
	return p, nil
}
func (f *fakeStore) CheckScopeTarget(_ context.Context, scope, value string) error {
	if scope == rbac.ScopeWorkspace {
		id, _ := strconv.ParseInt(value, 10, 64)
		if _, ok := f.ws[id]; !ok {
			return ErrWorkspaceNotFound
		}
	}
	return nil
}

type fakeScopes struct{ invalidated int }

func (s *fakeScopes) InvalidateScopes() { s.invalidated++ }
func (s *fakeScopes) ScopedPrincipal(_ context.Context, p *rbac.Principal) (*rbac.Principal, error) {
	return p, nil
}

func newRouter(st Store, sc ScopeCache, p *rbac.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	g := e.Group("/api/v1")
	g.Use(func(c *gin.Context) {
		if p != nil {
			rbac.SetPrincipal(c, p)
		}
		c.Next()
	})
	RegisterRoutes(g, Deps{Store: st, Scopes: sc})
	return e
}

func do(t *testing.T, e *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

var admin = &rbac.Principal{UserID: 1, Username: "admin", Roles: []string{"admin"}, Permissions: []string{"*"}}

func TestHTTPWorkspaceLifecycle(t *testing.T) {
	st, sc := newFake(), &fakeScopes{}
	e := newRouter(st, sc, admin)

	code, out := do(t, e, "POST", "/workspaces", gin.H{"name": "研发中心", "description": "d"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := int64(out["data"].(map[string]any)["id"].(float64))
	if sc.invalidated == 0 {
		t.Error("creator-owner insert did not invalidate scopes")
	}
	if code, _ := do(t, e, "POST", "/workspaces", gin.H{"name": "研发中心"}); code != 409 {
		t.Errorf("duplicate name: %d", code)
	}
	if code, _ := do(t, e, "POST", "/workspaces", gin.H{"name": strings.Repeat("x", 64)}); code != 400 {
		t.Errorf("long name: %d", code)
	}

	code, out = do(t, e, "GET", "/workspaces", nil)
	data := out["data"].(map[string]any)
	if code != 200 || data["total"].(float64) != 1 || len(data["items"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, out)
	}

	ids := "/workspaces/" + strconv.FormatInt(id, 10)
	// the creator is the only owner: cannot be removed or demoted
	code, out = do(t, e, "GET", ids+"/members", nil)
	items := out["data"].(map[string]any)["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("members: %d %v", code, out)
	}
	owner := strconv.FormatInt(int64(items[0].(map[string]any)["id"].(float64)), 10)
	if code, _ := do(t, e, "DELETE", ids+"/members/"+owner, nil); code != 409 {
		t.Errorf("delete last owner: %d", code)
	}
	if code, _ := do(t, e, "PUT", ids+"/members/"+owner, gin.H{"role": "viewer"}); code != 409 {
		t.Errorf("demote last owner: %d", code)
	}
	if code, _ := do(t, e, "PUT", ids+"/members/"+owner, gin.H{"role": "god"}); code != 400 {
		t.Errorf("bad role: %d", code)
	}
	if code, _ := do(t, e, "POST", ids+"/members", gin.H{"user_id": 2, "group_id": 3, "role": "admin"}); code != 400 {
		t.Errorf("both subjects: %d", code)
	}
	code, out = do(t, e, "POST", ids+"/members", gin.H{"user_id": 2, "role": "owner"})
	if code != 201 {
		t.Fatalf("add member: %d %v", code, out)
	}
	if code, _ := do(t, e, "DELETE", ids+"/members/"+owner, nil); code != 200 {
		t.Errorf("delete owner with another owner: %d", code)
	}

	// projects
	if code, _ := do(t, e, "POST", ids+"/projects", gin.H{"cluster_id": 1, "namespace": "Bad_NS"}); code != 400 {
		t.Errorf("bad namespace: %d", code)
	}
	if code, _ := do(t, e, "POST", ids+"/projects", gin.H{"cluster_id": 1, "namespace": "team-a"}); code != 201 {
		t.Errorf("add project: %d", code)
	}
	code, out = do(t, e, "POST", ids+"/projects", gin.H{"cluster_id": 1, "namespace": "team-a"})
	if code != 409 || !strings.Contains(out["message"].(string), "研发中心") {
		t.Errorf("namespace conflict: %d %v", code, out)
	}

	// delete needs confirm_name
	if code, _ := do(t, e, "DELETE", ids, nil); code != 400 {
		t.Errorf("delete without confirm: %d", code)
	}
	if code, _ := do(t, e, "DELETE", ids, gin.H{"confirm_name": "其他"}); code != 400 {
		t.Errorf("delete wrong confirm: %d", code)
	}
	if code, _ := do(t, e, "DELETE", ids, gin.H{"confirm_name": "研发中心"}); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do(t, e, "GET", ids, nil); code != 404 {
		t.Errorf("get deleted: %d", code)
	}
	if code, _ := do(t, e, "GET", "/workspaces/abc", nil); code != 400 {
		t.Errorf("bad id: %d", code)
	}
}

func TestHTTPPermissions(t *testing.T) {
	st := newFake()
	st.ws[5] = &Workspace{ID: 5, Name: "a"}
	st.ws[6] = &Workspace{ID: 6, Name: "b"}

	// no permission at all
	e := newRouter(st, nil, &rbac.Principal{UserID: 9, Username: "u", Roles: []string{"x"}})
	if code, _ := do(t, e, "GET", "/workspaces", nil); code != 403 {
		t.Errorf("no perm list: %d", code)
	}
	// read-only cannot write
	ro := &rbac.Principal{UserID: 9, Username: "u", Roles: []string{"x"}, Permissions: []string{"workspace:read"}}
	e = newRouter(st, nil, ro)
	if code, _ := do(t, e, "POST", "/workspaces", gin.H{"name": "c"}); code != 403 {
		t.Errorf("read-only create: %d", code)
	}
	if code, _ := do(t, e, "GET", "/workspaces/5", nil); code != 200 {
		t.Errorf("unrestricted reader get: %d", code)
	}
	// restricted member of workspace 5 only
	member := &rbac.Principal{UserID: 9, Username: "u", Permissions: []string{"workspace:read"},
		Scopes: &rbac.DataScope{Restricted: true, Workspaces: map[int64]string{5: "viewer"}}}
	e = newRouter(st, nil, member)
	code, out := do(t, e, "GET", "/workspaces", nil)
	if code != 200 || out["data"].(map[string]any)["total"].(float64) != 1 || len(st.lastFilter.IDs) != 1 || st.lastFilter.IDs[0] != 5 {
		t.Errorf("restricted list: %d %v %v", code, out, st.lastFilter)
	}
	if code, _ := do(t, e, "GET", "/workspaces/6", nil); code != 403 {
		t.Errorf("restricted foreign get: %d", code)
	}
	if code, _ := do(t, e, "GET", "/workspaces/5/members", nil); code != 200 {
		t.Errorf("restricted own members: %d", code)
	}
	if code, _ := do(t, e, "GET", "/role-bindings", nil); code != 200 || !st.lastBinding.Restricted || len(st.lastBinding.Workspaces) != 1 {
		t.Errorf("restricted bindings filter: %d %+v", code, st.lastBinding)
	}
}

func TestHTTPRoleBindings(t *testing.T) {
	st, sc := newFake(), &fakeScopes{}
	st.ws[5] = &Workspace{ID: 5, Name: "a"}
	mgr := &rbac.Principal{UserID: 3, Username: "m", Roles: []string{"ws-admin"}, Permissions: []string{"workspace:manage", "workspace:read", "workload:read"}}
	e := newRouter(st, sc, mgr)

	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 1, "user_id": 4, "scope": "global"}); code != 403 {
		t.Errorf("global without user:manage: %d", code)
	}
	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 2, "user_id": 4, "scope": "workspace", "scope_value": 5}); code != 403 {
		t.Errorf("escalation: %d", code)
	}
	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 1, "user_id": 4, "scope": "workspace", "scope_value": 99}); code != 404 {
		t.Errorf("missing workspace: %d", code)
	}
	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 77, "user_id": 4, "scope": "workspace", "scope_value": 5}); code != 404 {
		t.Errorf("missing role: %d", code)
	}
	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 1, "user_id": 4, "scope": "project", "scope_value": "1/Bad"}); code != 400 {
		t.Errorf("bad project: %d", code)
	}
	code, out := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 1, "user_id": 4, "scope": "workspace", "scope_value": 5})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	if sc.invalidated != 1 {
		t.Errorf("invalidated = %d", sc.invalidated)
	}
	if code, _ := do(t, e, "POST", "/role-bindings", gin.H{"role_id": 1, "user_id": 4, "scope": "workspace", "scope_value": "5"}); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
	bid := strconv.FormatInt(int64(out["data"].(map[string]any)["id"].(float64)), 10)
	if code, _ := do(t, e, "GET", "/role-bindings?role_id=x", nil); code != 400 {
		t.Errorf("bad role_id filter: %d", code)
	}
	if code, _ := do(t, e, "GET", "/role-bindings?scope=tenant", nil); code != 400 {
		t.Errorf("bad scope filter: %d", code)
	}
	if code, _ := do(t, e, "DELETE", "/role-bindings/"+bid, nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do(t, e, "DELETE", "/role-bindings/"+bid, nil); code != 404 {
		t.Errorf("delete again: %d", code)
	}
}

// ---------------------------------------------------------------- PostgreSQL

// TestPgStore needs a migrated PostgreSQL (MATRIX_TEST_DSN) with core
// migration 0009 and at least one user and role; skipped otherwise.
func TestPgStore(t *testing.T) {
	dsn := os.Getenv("MATRIX_TEST_DSN")
	if dsn == "" {
		t.Skip("MATRIX_TEST_DSN not set; skipping PostgreSQL workspace test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer pool.Close()
	var userID, roleID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM roles ORDER BY id LIMIT 1`).Scan(&roleID); err != nil {
		t.Skipf("no role: %v", err)
	}
	s := &PgStore{DB: pool}
	name := "test-ws-" + time.Now().Format("150405.000000")

	w, err := s.CreateWorkspace(ctx, CreateInput{Name: name}, userID, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM role_bindings WHERE scope='workspace' AND scope_value=$1`, strconv.FormatInt(w.ID, 10))
	defer pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, w.ID)
	if w.MemberCount != 1 {
		t.Errorf("member_count = %d", w.MemberCount)
	}
	if _, err := s.CreateWorkspace(ctx, CreateInput{Name: strings.ToUpper(name)}, 0, "test"); !errors.Is(err, ErrNameTaken) {
		t.Errorf("dup name: %v", err)
	}
	ws, total, err := s.ListWorkspaces(ctx, WorkspaceFilter{Keyword: name[:12], IDs: []int64{w.ID}}, 10, 0)
	if err != nil || total != 1 || len(ws) != 1 || ws[0].ManagerName != "" {
		t.Errorf("list: %v %d %v", ws, total, err)
	}
	if _, total, _ := s.ListWorkspaces(ctx, WorkspaceFilter{Keyword: "100%_none"}, 10, 0); total != 0 {
		t.Errorf("escaped keyword matched %d", total)
	}
	mgr, desc, zero := userID, "说明", int64(0)
	if w2, err := s.UpdateWorkspace(ctx, w.ID, UpdateInput{Description: &desc, ManagerID: &mgr}); err != nil || w2.ManagerID == nil || w2.Description != desc {
		t.Errorf("update: %+v %v", w2, err)
	}
	if w2, err := s.UpdateWorkspace(ctx, w.ID, UpdateInput{ManagerID: &zero}); err != nil || w2.ManagerID != nil {
		t.Errorf("clear manager: %+v %v", w2, err)
	}
	if _, err := s.UpdateWorkspace(ctx, 1<<60, UpdateInput{Description: &desc}); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("update missing: %v", err)
	}
	ms, err := s.ListMembers(ctx, w.ID)
	if err != nil || len(ms) != 1 {
		t.Fatalf("members: %v %v", ms, err)
	}
	if _, err := s.DeleteMember(ctx, w.ID, ms[0].ID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("last owner: %v", err)
	}
	if _, err := s.UpdateMemberRole(ctx, w.ID, ms[0].ID, "admin"); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demote last owner: %v", err)
	}
	if m, err := s.UpdateMemberRole(ctx, w.ID, ms[0].ID, "owner"); err != nil || m.Role != "owner" || m.Kind != "user" {
		t.Errorf("same role: %+v %v", m, err)
	}
	uid := userID
	if _, err := s.AddMember(ctx, w.ID, MemberInput{UserID: &uid, Role: "viewer"}, "test"); !errors.Is(err, ErrMemberExists) {
		t.Errorf("dup member: %v", err)
	}
	bad := int64(1 << 60)
	if _, err := s.AddMember(ctx, w.ID, MemberInput{UserID: &bad, Role: "viewer"}, "test"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("missing user: %v", err)
	}

	ns := "ws-test-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10)
	var cid int64 = 1
	_ = pool.QueryRow(ctx, `SELECT id FROM cluster_clusters ORDER BY id LIMIT 1`).Scan(&cid)
	p, err := s.AddProject(ctx, w.ID, ProjectInput{ClusterID: cid, Namespace: ns}, "test")
	if err != nil && !errors.Is(err, ErrClusterNotFound) {
		t.Fatal(err)
	}
	if err == nil {
		if _, err := s.AddProject(ctx, w.ID, ProjectInput{ClusterID: cid, Namespace: ns}, "test"); err == nil {
			t.Error("namespace conflict not detected")
		} else {
			var nt *NamespaceTakenError
			if !errors.As(err, &nt) || nt.Workspace != name {
				t.Errorf("conflict: %v", err)
			}
		}
		if ps, err := s.ListProjects(ctx, w.ID); err != nil || len(ps) != 1 || !ps[0].ClusterExists {
			t.Errorf("projects: %+v %v", ps, err)
		}
		if _, err := s.DeleteProject(ctx, w.ID, p.ID); err != nil {
			t.Error(err)
		}
	}

	b, err := s.CreateBinding(ctx, BindingInput{RoleID: roleID, UserID: &uid, Scope: "workspace", ScopeValue: FlexString(strconv.FormatInt(w.ID, 10))}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, BindingInput{RoleID: roleID, UserID: &uid, Scope: "workspace", ScopeValue: FlexString(strconv.FormatInt(w.ID, 10))}, "test"); !errors.Is(err, ErrBindingExists) {
		t.Errorf("dup binding: %v", err)
	}
	items, total, err := s.ListBindings(ctx, BindingFilter{Scope: "workspace", ScopeValue: strconv.FormatInt(w.ID, 10)}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != b.ID {
		t.Errorf("list bindings: %v %d %v", items, total, err)
	}
	if _, total, err := s.ListBindings(ctx, BindingFilter{Restricted: true, Workspaces: []string{"0"}}, 10, 0); err != nil || total != 0 {
		t.Errorf("restricted bindings: %d %v", total, err)
	}
	if _, total, err := s.ListBindings(ctx, BindingFilter{Restricted: true, Workspaces: []string{strconv.FormatInt(w.ID, 10)}}, 10, 0); err != nil || total != 1 {
		t.Errorf("restricted own bindings: %d %v", total, err)
	}
	if perms, err := s.RolePermissions(ctx, roleID); err != nil || perms == nil {
		t.Errorf("role perms: %v %v", perms, err)
	}
	if _, err := s.RolePermissions(ctx, 1<<60); !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("missing role: %v", err)
	}
	if err := s.CheckScopeTarget(ctx, "workspace", "999999999"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("scope target: %v", err)
	}
	if err := s.DeleteWorkspace(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBinding(ctx, b.ID); !errors.Is(err, ErrBindingNotFound) {
		t.Errorf("workspace binding not removed with workspace: %v", err)
	}
}
