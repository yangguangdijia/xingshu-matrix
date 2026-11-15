// Package workspace implements the v1.30 企业空间 (workspace) API: workspaces,
// their members and projects (cluster + namespace), and scoped role bindings.
//
// The tables come from core migration 0009_workspace_scope; data scope
// resolution (rbac.Principal.Scopes) reads the same tables, so every write that
// can change a user's scope invalidates the rbac scope cache.
//
// Layering: api.go (HTTP, permissions, audit) → Service (validation, rules) →
// Store (PostgreSQL, PgStore in store.go).
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// Field limits.
const (
	MaxNameLen        = 63
	MaxDescriptionLen = 512
	maxKeywordLen     = 64
)

// ---------------------------------------------------------------- models

// Workspace is one 企业空间.
type Workspace struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	ManagerID        *int64    `json:"manager_id"`
	ManagerName      string    `json:"manager_name"`
	CPULimitMilli    int64     `json:"cpu_limit_milli"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	MemberCount      int64     `json:"member_count"`
	ProjectCount     int64     `json:"project_count"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CreateInput is the POST /workspaces body.
type CreateInput struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	ManagerID        *int64 `json:"manager_id"`
	CPULimitMilli    int64  `json:"cpu_limit_milli"`
	MemoryLimitBytes int64  `json:"memory_limit_bytes"`
}

// UpdateInput is the PUT /workspaces/:id body. Omitted fields stay unchanged;
// manager_id 0 clears the manager.
type UpdateInput struct {
	Name             *string `json:"name"`
	Description      *string `json:"description"`
	ManagerID        *int64  `json:"manager_id"`
	CPULimitMilli    *int64  `json:"cpu_limit_milli"`
	MemoryLimitBytes *int64  `json:"memory_limit_bytes"`
}

// Member is a user or user group in a workspace.
type Member struct {
	ID          int64     `json:"id"`
	WorkspaceID int64     `json:"workspace_id"`
	Kind        string    `json:"kind"` // user | group
	UserID      *int64    `json:"user_id"`
	GroupID     *int64    `json:"group_id"`
	Name        string    `json:"name"` // username or group name
	DisplayName string    `json:"display_name"`
	Username    string    `json:"username,omitempty"`   // = name for user members
	GroupName   string    `json:"group_name,omitempty"` // = name for group members
	Role        string    `json:"role"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// MemberInput is the POST /workspaces/:id/members body.
type MemberInput struct {
	UserID  *int64 `json:"user_id"`
	GroupID *int64 `json:"group_id"`
	Role    string `json:"role"`
}

// Project is a namespace of a cluster assigned to a workspace.
type Project struct {
	ID            int64     `json:"id"`
	WorkspaceID   int64     `json:"workspace_id"`
	ClusterID     int64     `json:"cluster_id"`
	ClusterName   string    `json:"cluster_name"`
	ClusterEnv    string    `json:"cluster_env"`
	ClusterExists bool      `json:"cluster_exists"`
	Namespace     string    `json:"namespace"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// ProjectInput is the POST /workspaces/:id/projects body.
type ProjectInput struct {
	ClusterID int64  `json:"cluster_id"`
	Namespace string `json:"namespace"`
}

// RoleBinding is one scoped role binding.
type RoleBinding struct {
	ID          int64     `json:"id"`
	RoleID      int64     `json:"role_id"`
	RoleCode    string    `json:"role_code"`
	RoleName    string    `json:"role_name"`
	Kind        string    `json:"kind"` // user | group
	UserID      *int64    `json:"user_id"`
	GroupID     *int64    `json:"group_id"`
	SubjectName string    `json:"subject_name"`
	Scope       string    `json:"scope"`
	ScopeValue  string    `json:"scope_value"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// BindingInput is the POST /role-bindings body. scope_value accepts a JSON
// string or number (cluster / workspace ids).
type BindingInput struct {
	RoleID     int64      `json:"role_id"`
	UserID     *int64     `json:"user_id"`
	GroupID    *int64     `json:"group_id"`
	Scope      string     `json:"scope"`
	ScopeValue FlexString `json:"scope_value"`
}

// FlexString unmarshals from a JSON string, number or null.
type FlexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = FlexString(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("scope_value must be a string or number")
	}
	*f = FlexString(n.String())
	return nil
}

// WorkspaceFilter narrows GET /workspaces. IDs nil means every workspace.
type WorkspaceFilter struct {
	Keyword string
	IDs     []int64
}

// BindingFilter narrows GET /role-bindings.
type BindingFilter struct {
	Scope      string
	ScopeValue string
	RoleID     int64
	UserID     int64
	GroupID    int64
	// Restricted limits rows to workspace bindings in Workspaces and project
	// bindings in Projects (scope-only callers).
	Restricted bool
	Workspaces []string
	Projects   []string
}

// ---------------------------------------------------------------- errors

// Domain errors returned by Store; mapErr turns them into HTTP errors.
var (
	ErrWorkspaceNotFound = errors.New("workspace not found")
	ErrMemberNotFound    = errors.New("member not found")
	ErrProjectNotFound   = errors.New("project not found")
	ErrBindingNotFound   = errors.New("role binding not found")
	ErrUserNotFound      = errors.New("user not found")
	ErrGroupNotFound     = errors.New("group not found")
	ErrRoleNotFound      = errors.New("role not found")
	ErrClusterNotFound   = errors.New("cluster not found")
	ErrNameTaken         = errors.New("workspace name taken")
	ErrMemberExists      = errors.New("member exists")
	ErrBindingExists     = errors.New("role binding exists")
	ErrLastOwner         = errors.New("last owner")
)

// NamespaceTakenError reports that a namespace already belongs to a workspace.
type NamespaceTakenError struct{ Workspace string }

func (e *NamespaceTakenError) Error() string { return "namespace already assigned to " + e.Workspace }

// mapErr translates domain errors to API errors.
func mapErr(err error) error {
	var nt *NamespaceTakenError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &nt):
		if nt.Workspace == "" {
			return httpx.Conflict("该命名空间已归属其他企业空间")
		}
		return httpx.Conflict("该命名空间已归属企业空间「" + nt.Workspace + "」").WithDetails(map[string]string{"workspace": nt.Workspace})
	case errors.Is(err, ErrWorkspaceNotFound):
		return httpx.NotFound("企业空间不存在")
	case errors.Is(err, ErrMemberNotFound):
		return httpx.NotFound("成员不存在")
	case errors.Is(err, ErrProjectNotFound):
		return httpx.NotFound("项目不存在")
	case errors.Is(err, ErrBindingNotFound):
		return httpx.NotFound("角色绑定不存在")
	case errors.Is(err, ErrUserNotFound):
		return httpx.NotFound("用户不存在")
	case errors.Is(err, ErrGroupNotFound):
		return httpx.NotFound("用户组不存在")
	case errors.Is(err, ErrRoleNotFound):
		return httpx.NotFound("角色不存在")
	case errors.Is(err, ErrClusterNotFound):
		return httpx.NotFound("集群不存在")
	case errors.Is(err, ErrNameTaken):
		return httpx.Conflict("企业空间名称已存在")
	case errors.Is(err, ErrMemberExists):
		return httpx.Conflict("该用户或用户组已是空间成员")
	case errors.Is(err, ErrBindingExists):
		return httpx.Conflict("相同的角色绑定已存在")
	case errors.Is(err, ErrLastOwner):
		return httpx.Conflict("不能移除或降级企业空间的最后一个所有者")
	}
	return err
}

// ---------------------------------------------------------------- validation

// NormalizeName trims and validates a workspace name (1–63 characters, no
// control characters).
func NormalizeName(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return "", httpx.Validation("请填写企业空间名称")
	}
	if n > MaxNameLen || !utf8.ValidString(s) {
		return "", httpx.Validation("企业空间名称最多 63 个字符")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", httpx.Validation("企业空间名称不能包含控制字符")
		}
	}
	return s, nil
}

func validDescription(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxDescriptionLen {
		return "", httpx.Validation("描述最多 512 个字符")
	}
	return s, nil
}

func validLimits(cpu, mem int64) error {
	if cpu < 0 || mem < 0 {
		return httpx.Validation("配额不能为负数（0 表示不限制）")
	}
	return nil
}

// ValidateCreate normalizes a create request.
func ValidateCreate(in CreateInput) (CreateInput, error) {
	var err error
	if in.Name, err = NormalizeName(in.Name); err != nil {
		return in, err
	}
	if in.Description, err = validDescription(in.Description); err != nil {
		return in, err
	}
	if in.ManagerID != nil && *in.ManagerID <= 0 {
		in.ManagerID = nil
	}
	return in, validLimits(in.CPULimitMilli, in.MemoryLimitBytes)
}

// ValidateUpdate normalizes an update request.
func ValidateUpdate(in UpdateInput) (UpdateInput, error) {
	if in.Name != nil {
		n, err := NormalizeName(*in.Name)
		if err != nil {
			return in, err
		}
		in.Name = &n
	}
	if in.Description != nil {
		d, err := validDescription(*in.Description)
		if err != nil {
			return in, err
		}
		in.Description = &d
	}
	if in.ManagerID != nil && *in.ManagerID < 0 {
		return in, httpx.Validation("manager_id 无效")
	}
	var cpu, mem int64
	if in.CPULimitMilli != nil {
		cpu = *in.CPULimitMilli
	}
	if in.MemoryLimitBytes != nil {
		mem = *in.MemoryLimitBytes
	}
	return in, validLimits(cpu, mem)
}

// ValidateDeleteConfirm checks the confirm_name of DELETE /workspaces/:id.
func ValidateDeleteConfirm(name, confirm string) error {
	confirm = strings.TrimSpace(confirm)
	if confirm == "" {
		return httpx.Validation("请输入企业空间名称以确认删除")
	}
	if confirm != name {
		return httpx.Validation("输入的名称与企业空间名称不一致")
	}
	return nil
}

// validSubject checks the user_id | group_id pair (exactly one, positive).
func validSubject(userID, groupID *int64) error {
	u := userID != nil && *userID != 0
	g := groupID != nil && *groupID != 0
	if u == g {
		return httpx.Validation("请在用户和用户组中选择且只选择一个")
	}
	if (u && *userID < 0) || (g && *groupID < 0) {
		return httpx.Validation("用户或用户组编号无效")
	}
	return nil
}

func normSubject(userID, groupID *int64) (*int64, *int64) {
	if userID != nil && *userID == 0 {
		userID = nil
	}
	if groupID != nil && *groupID == 0 {
		groupID = nil
	}
	return userID, groupID
}

// ValidateMember normalizes a member request (role whitelist owner|admin|member|viewer).
func ValidateMember(in MemberInput) (MemberInput, error) {
	if err := validSubject(in.UserID, in.GroupID); err != nil {
		return in, err
	}
	in.UserID, in.GroupID = normSubject(in.UserID, in.GroupID)
	r, err := ValidateRole(in.Role)
	in.Role = r
	return in, err
}

// ValidateRole checks a workspace member role.
func ValidateRole(role string) (string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if !rbac.ValidWorkspaceRole(role) {
		return role, httpx.Validation("空间角色无效，可选：owner、admin、member、viewer")
	}
	return role, nil
}

// ValidateProject normalizes a project request (DNS-1123 label namespace).
func ValidateProject(in ProjectInput) (ProjectInput, error) {
	in.Namespace = strings.TrimSpace(in.Namespace)
	if in.ClusterID <= 0 {
		return in, httpx.Validation("请选择集群")
	}
	if !rbac.ValidNamespace(in.Namespace) {
		return in, httpx.Validation("命名空间名称无效：只能包含小写字母、数字和连字符，以字母或数字开头和结尾，最多 63 个字符")
	}
	return in, nil
}

var envCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// parsePositiveID parses a canonical positive int64 ("12", not "012" or "+12").
func parsePositiveID(s string) (int64, bool) {
	if s == "" || s[0] == '0' || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// NormalizeScope validates scope / scope_value with the same rules as the
// role_bindings CHECK constraints and the rbac resolver:
//
//	global → ''; env → environment code; cluster → cluster id;
//	workspace → workspace id; project → '<cluster id>/<namespace>'.
func NormalizeScope(scope, value string) (string, string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	value = strings.TrimSpace(value)
	if scope == "" {
		scope = rbac.ScopeGlobal
	}
	if !rbac.ValidScopeType(scope) {
		return scope, value, httpx.Validation("授权范围无效，可选：global、env、cluster、workspace、project")
	}
	switch scope {
	case rbac.ScopeGlobal:
		if value != "" {
			return scope, value, httpx.Validation("全局范围不能指定范围值")
		}
	case rbac.ScopeEnv:
		if !envCodeRe.MatchString(value) {
			return scope, value, httpx.Validation("请填写有效的环境编码")
		}
	case rbac.ScopeCluster:
		if _, ok := parsePositiveID(value); !ok {
			return scope, value, httpx.Validation("请选择集群")
		}
	case rbac.ScopeWorkspace:
		if _, ok := parsePositiveID(value); !ok {
			return scope, value, httpx.Validation("请选择企业空间")
		}
	case rbac.ScopeProject:
		c, ns, ok := strings.Cut(value, "/")
		if !ok {
			return scope, value, httpx.Validation("项目范围格式应为「集群编号/命名空间」")
		}
		if _, ok := parsePositiveID(c); !ok {
			return scope, value, httpx.Validation("项目范围中的集群编号无效")
		}
		if !rbac.ValidNamespace(ns) {
			return scope, value, httpx.Validation("项目范围中的命名空间名称无效")
		}
	}
	return scope, value, nil
}

// ValidateBinding normalizes a role binding request.
func ValidateBinding(in BindingInput) (BindingInput, error) {
	if in.RoleID <= 0 {
		return in, httpx.Validation("请选择角色")
	}
	if err := validSubject(in.UserID, in.GroupID); err != nil {
		return in, err
	}
	in.UserID, in.GroupID = normSubject(in.UserID, in.GroupID)
	s, v, err := NormalizeScope(in.Scope, string(in.ScopeValue))
	in.Scope, in.ScopeValue = s, FlexString(v)
	return in, err
}

// OwnerRemovalAllowed reports whether a member may lose its owner role:
// otherOwners is the number of owners in the workspace besides this member.
func OwnerRemovalAllowed(isOwner bool, otherOwners int64) bool {
	return !isOwner || otherOwners > 0
}

// ---------------------------------------------------------------- store

// Store is the persistence surface (PgStore in production).
type Store interface {
	ListWorkspaces(ctx context.Context, f WorkspaceFilter, limit, offset int) ([]Workspace, int64, error)
	GetWorkspace(ctx context.Context, id int64) (*Workspace, error)
	// CreateWorkspace inserts the workspace and, when ownerUserID > 0, that
	// user as its first owner, in one transaction.
	CreateWorkspace(ctx context.Context, in CreateInput, ownerUserID int64, by string) (*Workspace, error)
	UpdateWorkspace(ctx context.Context, id int64, in UpdateInput) (*Workspace, error)
	// DeleteWorkspace deletes the workspace (members / projects cascade) and
	// its workspace-scoped role bindings.
	DeleteWorkspace(ctx context.Context, id int64) error

	ListMembers(ctx context.Context, workspaceID int64) ([]Member, error)
	AddMember(ctx context.Context, workspaceID int64, in MemberInput, by string) (*Member, error)
	// UpdateMemberRole and DeleteMember return ErrLastOwner when the change
	// would leave the workspace without an owner.
	UpdateMemberRole(ctx context.Context, workspaceID, memberID int64, role string) (*Member, error)
	DeleteMember(ctx context.Context, workspaceID, memberID int64) (*Member, error)

	ListProjects(ctx context.Context, workspaceID int64) ([]Project, error)
	AddProject(ctx context.Context, workspaceID int64, in ProjectInput, by string) (*Project, error)
	DeleteProject(ctx context.Context, workspaceID, projectID int64) (*Project, error)

	ListBindings(ctx context.Context, f BindingFilter, limit, offset int) ([]RoleBinding, int64, error)
	CreateBinding(ctx context.Context, in BindingInput, by string) (*RoleBinding, error)
	GetBinding(ctx context.Context, id int64) (*RoleBinding, error)
	DeleteBinding(ctx context.Context, id int64) (*RoleBinding, error)
	// RolePermissions returns the permission codes of a role (ErrRoleNotFound).
	RolePermissions(ctx context.Context, roleID int64) ([]string, error)
	// CheckScopeTarget verifies that the workspace / cluster a binding points
	// at exists (clusters only when the cluster module is installed).
	CheckScopeTarget(ctx context.Context, scope, value string) error
}

// ScopeCache is the rbac scope cache (implemented by *rbac.Service).
type ScopeCache interface {
	InvalidateScopes()
	ScopedPrincipal(ctx context.Context, base *rbac.Principal) (*rbac.Principal, error)
}

// Service applies validation and business rules on top of Store.
type Service struct {
	Store  Store
	Scopes ScopeCache // optional
}

func (s *Service) invalidate() {
	if s.Scopes != nil {
		s.Scopes.InvalidateScopes()
	}
}

// ListWorkspaces lists the workspaces visible to p.
func (s *Service) ListWorkspaces(ctx context.Context, p *rbac.Principal, keyword string, limit, offset int) ([]Workspace, int64, error) {
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > maxKeywordLen {
		return nil, 0, httpx.Validation("搜索关键字过长")
	}
	f := WorkspaceFilter{Keyword: keyword, IDs: VisibleWorkspaceIDs(p)}
	if f.IDs != nil && len(f.IDs) == 0 {
		return []Workspace{}, 0, nil
	}
	return s.Store.ListWorkspaces(ctx, f, limit, offset)
}

// VisibleWorkspaceIDs returns nil when p sees every workspace, otherwise the
// ids of p's memberships (possibly empty).
func VisibleWorkspaceIDs(p *rbac.Principal) []int64 {
	if p == nil {
		return []int64{}
	}
	if p.Can("workspace:manage") || (!p.Restricted() && p.Can("workspace:read")) {
		return nil
	}
	ids := []int64{}
	if p.Scopes != nil {
		for id := range p.Scopes.Workspaces {
			ids = append(ids, id)
		}
	}
	return ids
}

// GetWorkspace returns one workspace.
func (s *Service) GetWorkspace(ctx context.Context, id int64) (*Workspace, error) {
	w, err := s.Store.GetWorkspace(ctx, id)
	return w, mapErr(err)
}

// CreateWorkspace creates a workspace; its manager (or else the creator) becomes the first owner.
func (s *Service) CreateWorkspace(ctx context.Context, p *rbac.Principal, in CreateInput) (*Workspace, error) {
	in, err := ValidateCreate(in)
	if err != nil {
		return nil, err
	}
	var owner int64
	if in.ManagerID != nil {
		owner = *in.ManagerID
	} else if p != nil {
		owner = p.UserID
	}
	w, err := s.Store.CreateWorkspace(ctx, in, owner, username(p))
	if err != nil {
		return nil, mapErr(err)
	}
	if owner > 0 {
		s.invalidate()
	}
	return w, nil
}

// UpdateWorkspace changes name / description / manager / limits.
func (s *Service) UpdateWorkspace(ctx context.Context, id int64, in UpdateInput) (*Workspace, error) {
	in, err := ValidateUpdate(in)
	if err != nil {
		return nil, err
	}
	w, err := s.Store.UpdateWorkspace(ctx, id, in)
	return w, mapErr(err)
}

// DeleteWorkspace deletes a workspace after checking confirm_name.
func (s *Service) DeleteWorkspace(ctx context.Context, id int64, confirmName string) (*Workspace, error) {
	w, err := s.Store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := ValidateDeleteConfirm(w.Name, confirmName); err != nil {
		return nil, err
	}
	if err := s.Store.DeleteWorkspace(ctx, id); err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return w, nil
}

// ListMembers lists a workspace's members.
func (s *Service) ListMembers(ctx context.Context, wsID int64) ([]Member, error) {
	if _, err := s.Store.GetWorkspace(ctx, wsID); err != nil {
		return nil, mapErr(err)
	}
	m, err := s.Store.ListMembers(ctx, wsID)
	return m, mapErr(err)
}

// AddMember adds a user or group to a workspace.
func (s *Service) AddMember(ctx context.Context, p *rbac.Principal, wsID int64, in MemberInput) (*Member, error) {
	in, err := ValidateMember(in)
	if err != nil {
		return nil, err
	}
	m, err := s.Store.AddMember(ctx, wsID, in, username(p))
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return m, nil
}

// UpdateMember changes a member's role.
func (s *Service) UpdateMember(ctx context.Context, wsID, mid int64, role string) (*Member, error) {
	role, err := ValidateRole(role)
	if err != nil {
		return nil, err
	}
	m, err := s.Store.UpdateMemberRole(ctx, wsID, mid, role)
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return m, nil
}

// DeleteMember removes a member.
func (s *Service) DeleteMember(ctx context.Context, wsID, mid int64) (*Member, error) {
	m, err := s.Store.DeleteMember(ctx, wsID, mid)
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return m, nil
}

// ListProjects lists a workspace's projects.
func (s *Service) ListProjects(ctx context.Context, wsID int64) ([]Project, error) {
	if _, err := s.Store.GetWorkspace(ctx, wsID); err != nil {
		return nil, mapErr(err)
	}
	ps, err := s.Store.ListProjects(ctx, wsID)
	return ps, mapErr(err)
}

// AddProject assigns cluster/namespace to a workspace.
func (s *Service) AddProject(ctx context.Context, p *rbac.Principal, wsID int64, in ProjectInput) (*Project, error) {
	in, err := ValidateProject(in)
	if err != nil {
		return nil, err
	}
	pr, err := s.Store.AddProject(ctx, wsID, in, username(p))
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return pr, nil
}

// DeleteProject removes a project from a workspace (the namespace itself is untouched).
func (s *Service) DeleteProject(ctx context.Context, wsID, pid int64) (*Project, error) {
	pr, err := s.Store.DeleteProject(ctx, wsID, pid)
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return pr, nil
}

// ListBindings lists role bindings visible to p.
func (s *Service) ListBindings(ctx context.Context, p *rbac.Principal, f BindingFilter, limit, offset int) ([]RoleBinding, int64, error) {
	if f.Scope != "" {
		f.Scope = strings.ToLower(strings.TrimSpace(f.Scope))
		if !rbac.ValidScopeType(f.Scope) {
			return nil, 0, httpx.Validation("授权范围无效，可选：global、env、cluster、workspace、project")
		}
	}
	if len(f.ScopeValue) > 200 {
		return nil, 0, httpx.Validation("范围值过长")
	}
	if p == nil {
		return []RoleBinding{}, 0, nil
	}
	if !p.Can("workspace:manage") && p.Restricted() {
		f.Restricted = true
		f.Workspaces = []string{}
		for id := range p.Scopes.Workspaces {
			f.Workspaces = append(f.Workspaces, strconv.FormatInt(id, 10))
		}
		_, projects := p.VisibleScope()
		f.Projects = []string{}
		for _, n := range projects {
			f.Projects = append(f.Projects, n.String())
		}
	}
	items, total, err := s.Store.ListBindings(ctx, f, limit, offset)
	return items, total, mapErr(err)
}

// CreateBinding creates a scoped role binding. Callers can never grant a role
// carrying permissions they do not hold themselves, and global / env bindings
// (which widen platform-wide permissions like user role assignment) also
// require user:manage.
func (s *Service) CreateBinding(ctx context.Context, p *rbac.Principal, in BindingInput) (*RoleBinding, error) {
	in, err := ValidateBinding(in)
	if err != nil {
		return nil, err
	}
	if (in.Scope == rbac.ScopeGlobal || in.Scope == rbac.ScopeEnv) && !p.Can("user:manage") {
		return nil, httpx.Forbidden("全局或环境范围的角色绑定还需要「管理用户」权限").WithDetails(map[string]string{"required": "user:manage"})
	}
	perms, err := s.Store.RolePermissions(ctx, in.RoleID)
	if err != nil {
		return nil, mapErr(err)
	}
	if missing := MissingPermissions(p, perms); len(missing) > 0 {
		return nil, httpx.Forbidden("不能授予超出你自身权限的角色").WithDetails(map[string]any{"missing": missing})
	}
	if err := s.Store.CheckScopeTarget(ctx, in.Scope, string(in.ScopeValue)); err != nil {
		return nil, mapErr(err)
	}
	b, err := s.Store.CreateBinding(ctx, in, username(p))
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return b, nil
}

// DeleteBinding deletes a role binding; global / env bindings also require user:manage.
func (s *Service) DeleteBinding(ctx context.Context, p *rbac.Principal, id int64) (*RoleBinding, error) {
	b, err := s.Store.GetBinding(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	if (b.Scope == rbac.ScopeGlobal || b.Scope == rbac.ScopeEnv) && !p.Can("user:manage") {
		return nil, httpx.Forbidden("全局或环境范围的角色绑定还需要「管理用户」权限").WithDetails(map[string]string{"required": "user:manage"})
	}
	b, err = s.Store.DeleteBinding(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	s.invalidate()
	return b, nil
}

// MissingPermissions returns the role permissions p does not hold. A wildcard
// grant in the role ("*", "res:*", "*:act") is held only when p holds the same
// or a wider wildcard.
func MissingPermissions(p *rbac.Principal, rolePerms []string) []string {
	var missing []string
	for _, perm := range rolePerms {
		held := p != nil && rbac.Match(p.Permissions, perm)
		if perm == "*" && p != nil {
			held = false
			for _, g := range p.Permissions {
				if g == "*" {
					held = true
				}
			}
		}
		if !held {
			missing = append(missing, perm)
		}
	}
	return missing
}

func username(p *rbac.Principal) string {
	if p == nil {
		return ""
	}
	return p.Username
}
