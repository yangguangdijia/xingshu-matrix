package rbac

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// Role is a named set of permissions.
type Role struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Builtin     bool      `json:"builtin"`
	EnvScope    []string  `json:"env_scope"`
	Permissions []string  `json:"permissions"`
	UserCount   int64     `json:"user_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RoleInput is the create/update payload.
type RoleInput struct {
	Code        string   `json:"code"`
	Name        string   `json:"name" binding:"required,max=64"`
	Description string   `json:"description" binding:"max=512"`
	EnvScope    []string `json:"env_scope"`
	Permissions []string `json:"permissions"`
}

// EnvPolicy is the change policy of one environment.
type EnvPolicy struct {
	Env                string    `json:"env"`
	Name               string    `json:"name"`
	RequireApproval    bool      `json:"require_approval"`
	ApproverPermission string    `json:"approver_permission"`
	UpdatedAt          time.Time `json:"updated_at"`
}

var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_\-]{1,63}$`)

// Service implements RBAC persistence and policy queries.
type Service struct {
	db      store.DB
	Catalog *Catalog
	// OnChange is invoked after any change that can alter a user's effective
	// permissions (auth uses it to drop its principal cache).
	OnChange func()
}

// NewService builds the RBAC service.
func NewService(db store.DB, catalog *Catalog) *Service {
	return &Service{db: db, Catalog: catalog}
}

func (s *Service) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// ListRoles returns all roles with their permissions and member counts.
func (s *Service) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.Query(ctx, `
		SELECT r.id, r.code, r.name, r.description, r.builtin, r.env_scope, r.created_at, r.updated_at,
		       COALESCE((SELECT array_agg(permission ORDER BY permission) FROM role_permissions rp WHERE rp.role_id=r.id), '{}'),
		       (SELECT count(*) FROM user_roles ur WHERE ur.role_id=r.id)
		FROM roles r ORDER BY r.builtin DESC, r.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRole)
}

// GetRole returns one role.
func (s *Service) GetRole(ctx context.Context, id int64) (*Role, error) {
	rows, err := s.db.Query(ctx, `
		SELECT r.id, r.code, r.name, r.description, r.builtin, r.env_scope, r.created_at, r.updated_at,
		       COALESCE((SELECT array_agg(permission ORDER BY permission) FROM role_permissions rp WHERE rp.role_id=r.id), '{}'),
		       (SELECT count(*) FROM user_roles ur WHERE ur.role_id=r.id)
		FROM roles r WHERE r.id=$1`, id)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRole)
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("role not found")
	}
	return &r, err
}

func scanRole(row pgx.CollectableRow) (Role, error) {
	var r Role
	err := row.Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.Builtin, &r.EnvScope, &r.CreatedAt, &r.UpdatedAt, &r.Permissions, &r.UserCount)
	if r.EnvScope == nil {
		r.EnvScope = []string{}
	}
	return r, err
}

func validatePerms(perms []string) ([]string, error) {
	set := map[string]bool{}
	for _, p := range perms {
		if !ValidCode(p) {
			return nil, httpx.Validation(fmt.Sprintf("invalid permission %q (want resource:action)", p))
		}
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// CreateRole inserts a custom role.
func (s *Service) CreateRole(ctx context.Context, in RoleInput) (*Role, error) {
	if !codeRe.MatchString(in.Code) {
		return nil, httpx.Validation("role code must match ^[a-z][a-z0-9_-]{1,63}$")
	}
	perms, err := validatePerms(in.Permissions)
	if err != nil {
		return nil, err
	}
	scope := in.EnvScope
	if scope == nil {
		scope = []string{}
	}
	var id int64
	err = store.InTx(ctx, s.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO roles(code,name,description,env_scope) VALUES($1,$2,$3,$4) RETURNING id`,
			in.Code, in.Name, in.Description, scope).Scan(&id); err != nil {
			return err
		}
		return replacePerms(ctx, tx, id, perms)
	})
	if store.IsUniqueViolation(err) {
		return nil, httpx.Conflict("role code already exists")
	}
	if err != nil {
		return nil, err
	}
	s.changed()
	return s.GetRole(ctx, id)
}

// UpdateRole updates name/description/scope/permissions. Built-in roles keep
// their code; the built-in admin role keeps "*".
func (s *Service) UpdateRole(ctx context.Context, id int64, in RoleInput) (*Role, error) {
	cur, err := s.GetRole(ctx, id)
	if err != nil {
		return nil, err
	}
	perms, err := validatePerms(in.Permissions)
	if err != nil {
		return nil, err
	}
	if cur.Builtin && cur.Code == "admin" {
		perms = []string{"*"}
		in.EnvScope = nil
	}
	scope := in.EnvScope
	if scope == nil {
		scope = []string{}
	}
	err = store.InTx(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE roles SET name=$2, description=$3, env_scope=$4, updated_at=now() WHERE id=$1`,
			id, in.Name, in.Description, scope); err != nil {
			return err
		}
		return replacePerms(ctx, tx, id, perms)
	})
	if err != nil {
		return nil, err
	}
	s.changed()
	return s.GetRole(ctx, id)
}

func replacePerms(ctx context.Context, tx pgx.Tx, roleID int64, perms []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id=$1`, roleID); err != nil {
		return err
	}
	if len(perms) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO role_permissions(role_id, permission) SELECT $1, unnest($2::text[])`, roleID, perms)
	return err
}

// DeleteRole removes a custom role.
func (s *Service) DeleteRole(ctx context.Context, id int64) error {
	cur, err := s.GetRole(ctx, id)
	if err != nil {
		return err
	}
	if cur.Builtin {
		return httpx.Forbidden("built-in roles cannot be deleted")
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM roles WHERE id=$1`, id); err != nil {
		return err
	}
	s.changed()
	return nil
}

// UserAccess resolves a user's role codes, permissions and env scope (nil = all).
func (s *Service) UserAccess(ctx context.Context, userID int64) (roles, perms, envScope []string, err error) {
	rows, err := s.db.Query(ctx, `
		SELECT r.code, r.env_scope,
		       COALESCE((SELECT array_agg(permission) FROM role_permissions rp WHERE rp.role_id=r.id), '{}')
		FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=$1 ORDER BY r.code`, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	permSet := map[string]bool{}
	scopeSet := map[string]bool{}
	unscoped := false
	for rows.Next() {
		var code string
		var scope, ps []string
		if err := rows.Scan(&code, &scope, &ps); err != nil {
			return nil, nil, nil, err
		}
		roles = append(roles, code)
		for _, p := range ps {
			permSet[p] = true
		}
		if len(scope) == 0 {
			unscoped = true
		}
		for _, e := range scope {
			scopeSet[e] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	for p := range permSet {
		perms = append(perms, p)
	}
	sort.Strings(perms)
	if roles == nil {
		roles = []string{}
	}
	if perms == nil {
		perms = []string{}
	}
	if !unscoped {
		envScope = []string{}
		for e := range scopeSet {
			envScope = append(envScope, e)
		}
		sort.Strings(envScope)
	}
	return roles, perms, envScope, nil
}

// SetUserRoles replaces a user's roles (runs on db, which may be a transaction).
func (s *Service) SetUserRoles(ctx context.Context, db store.DB, userID int64, roleIDs []int64) error {
	if _, err := db.Exec(ctx, `DELETE FROM user_roles WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if len(roleIDs) > 0 {
		tag, err := db.Exec(ctx, `INSERT INTO user_roles(user_id, role_id) SELECT $1, id FROM roles WHERE id = ANY($2::bigint[])`, userID, roleIDs)
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != len(uniq(roleIDs)) {
			return httpx.Validation("one or more role ids do not exist")
		}
	}
	s.changed()
	return nil
}

func uniq(ids []int64) map[int64]bool {
	m := map[int64]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// ListEnvPolicies returns all environment policies.
func (s *Service) ListEnvPolicies(ctx context.Context) ([]EnvPolicy, error) {
	rows, err := s.db.Query(ctx, `SELECT env, name, require_approval, approver_permission, updated_at FROM env_policies ORDER BY env`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EnvPolicy])
}

// GetEnvPolicy returns the policy of env, or (nil, nil) when none is defined.
func (s *Service) GetEnvPolicy(ctx context.Context, env string) (*EnvPolicy, error) {
	var p EnvPolicy
	err := s.db.QueryRow(ctx, `SELECT env, name, require_approval, approver_permission, updated_at FROM env_policies WHERE env=$1`, env).
		Scan(&p.Env, &p.Name, &p.RequireApproval, &p.ApproverPermission, &p.UpdatedAt)
	if store.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertEnvPolicy creates or updates an environment policy.
func (s *Service) UpsertEnvPolicy(ctx context.Context, p EnvPolicy) (*EnvPolicy, error) {
	if !codeRe.MatchString(p.Env) {
		return nil, httpx.Validation("env must match ^[a-z][a-z0-9_-]{1,63}$")
	}
	if p.ApproverPermission == "" {
		p.ApproverPermission = "approval:approve"
	}
	if !ValidCode(p.ApproverPermission) {
		return nil, httpx.Validation("invalid approver_permission")
	}
	if p.Name == "" {
		p.Name = p.Env
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO env_policies(env, name, require_approval, approver_permission) VALUES ($1,$2,$3,$4)
		ON CONFLICT (env) DO UPDATE SET name=EXCLUDED.name, require_approval=EXCLUDED.require_approval,
		  approver_permission=EXCLUDED.approver_permission, updated_at=now()`,
		p.Env, p.Name, p.RequireApproval, p.ApproverPermission)
	if err != nil {
		return nil, err
	}
	return s.GetEnvPolicy(ctx, p.Env)
}

// RequiresApproval reports whether write operations on env must go through an
// approval ticket. Unknown environments do not require approval.
func (s *Service) RequiresApproval(ctx context.Context, env string) (bool, error) {
	if env == "" {
		return false, nil
	}
	p, err := s.GetEnvPolicy(ctx, env)
	if err != nil || p == nil {
		return false, err
	}
	return p.RequireApproval, nil
}
