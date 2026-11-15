package workspace

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// PgStore implements Store on PostgreSQL (tables from core migration 0009).
type PgStore struct {
	DB store.DB
}

var _ Store = (*PgStore)(nil)

// pgErr returns the PostgreSQL error code and constraint name of err.
func pgErr(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

// fkErr maps a foreign key violation to the domain error of the missing row.
func fkErr(err error) error {
	code, cons := pgErr(err)
	if code != "23503" {
		return err
	}
	switch {
	case strings.Contains(cons, "workspace_id"):
		return ErrWorkspaceNotFound
	case strings.Contains(cons, "manager_id"), strings.Contains(cons, "user_id"):
		return ErrUserNotFound
	case strings.Contains(cons, "group_id"):
		return ErrGroupNotFound
	case strings.Contains(cons, "role_id"):
		return ErrRoleNotFound
	}
	return err
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// ---------------------------------------------------------------- workspaces

const wsSelect = `SELECT w.id, w.name, w.description, w.manager_id, COALESCE(u.username, ''),
	w.cpu_limit_milli, w.memory_limit_bytes,
	(SELECT count(*) FROM workspace_members m WHERE m.workspace_id = w.id),
	(SELECT count(*) FROM workspace_projects p WHERE p.workspace_id = w.id),
	w.created_by, w.created_at, w.updated_at
	FROM workspaces w LEFT JOIN users u ON u.id = w.manager_id`

func scanWorkspace(row pgx.Row) (*Workspace, error) {
	var w Workspace
	if err := row.Scan(&w.ID, &w.Name, &w.Description, &w.ManagerID, &w.ManagerName,
		&w.CPULimitMilli, &w.MemoryLimitBytes, &w.MemberCount, &w.ProjectCount,
		&w.CreatedBy, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

// ListWorkspaces implements Store.
func (s *PgStore) ListWorkspaces(ctx context.Context, f WorkspaceFilter, limit, offset int) ([]Workspace, int64, error) {
	where := []string{"TRUE"}
	args := []any{}
	if f.Keyword != "" {
		args = append(args, likePattern(f.Keyword))
		where = append(where, fmt.Sprintf(`(w.name ILIKE $%d ESCAPE '\' OR w.description ILIKE $%d ESCAPE '\')`, len(args), len(args)))
	}
	if f.IDs != nil {
		args = append(args, f.IDs)
		where = append(where, fmt.Sprintf("w.id = ANY($%d::bigint[])", len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM workspaces w WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.DB.Query(ctx, wsSelect+` WHERE `+cond+
		fmt.Sprintf(` ORDER BY lower(w.name), w.id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *w)
	}
	return out, total, rows.Err()
}

// GetWorkspace implements Store.
func (s *PgStore) GetWorkspace(ctx context.Context, id int64) (*Workspace, error) {
	w, err := scanWorkspace(s.DB.QueryRow(ctx, wsSelect+` WHERE w.id = $1`, id))
	if store.IsNoRows(err) {
		return nil, ErrWorkspaceNotFound
	}
	return w, err
}

func wsWriteErr(err error) error {
	if code, cons := pgErr(err); code == "23505" && strings.Contains(cons, "workspaces_name") {
		return ErrNameTaken
	}
	return fkErr(err)
}

// CreateWorkspace implements Store.
func (s *PgStore) CreateWorkspace(ctx context.Context, in CreateInput, ownerUserID int64, by string) (*Workspace, error) {
	var id int64
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (name, description, manager_id, cpu_limit_milli, memory_limit_bytes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			in.Name, in.Description, in.ManagerID, in.CPULimitMilli, in.MemoryLimitBytes, by).Scan(&id); err != nil {
			return err
		}
		if ownerUserID > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role, created_by) VALUES ($1, $2, 'owner', $3)`,
				id, ownerUserID, by); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, wsWriteErr(err)
	}
	return s.GetWorkspace(ctx, id)
}

// UpdateWorkspace implements Store.
func (s *PgStore) UpdateWorkspace(ctx context.Context, id int64, in UpdateInput) (*Workspace, error) {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.Description != nil {
		add("description", *in.Description)
	}
	if in.ManagerID != nil {
		if *in.ManagerID == 0 {
			sets = append(sets, "manager_id = NULL")
		} else {
			add("manager_id", *in.ManagerID)
		}
	}
	if in.CPULimitMilli != nil {
		add("cpu_limit_milli", *in.CPULimitMilli)
	}
	if in.MemoryLimitBytes != nil {
		add("memory_limit_bytes", *in.MemoryLimitBytes)
	}
	tag, err := s.DB.Exec(ctx, `UPDATE workspaces SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		return nil, wsWriteErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrWorkspaceNotFound
	}
	return s.GetWorkspace(ctx, id)
}

// DeleteWorkspace implements Store.
func (s *PgStore) DeleteWorkspace(ctx context.Context, id int64) error {
	return store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE scope = 'workspace' AND scope_value = $1`, strconv.FormatInt(id, 10)); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrWorkspaceNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------- members

const memberSelect = `SELECT m.id, m.workspace_id, m.user_id, m.group_id,
	COALESCE(u.username, g.name, ''), COALESCE(u.display_name, ''), m.role, m.created_by, m.created_at
	FROM workspace_members m
	LEFT JOIN users u ON u.id = m.user_id
	LEFT JOIN user_groups g ON g.id = m.group_id`

func scanMember(row pgx.Row) (*Member, error) {
	var m Member
	if err := row.Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.GroupID, &m.Name, &m.DisplayName, &m.Role, &m.CreatedBy, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.Kind, m.Username = "user", m.Name
	if m.GroupID != nil {
		m.Kind, m.Username, m.GroupName = "group", "", m.Name
	}
	return &m, nil
}

func (s *PgStore) getMember(ctx context.Context, db store.DB, wsID, mid int64) (*Member, error) {
	m, err := scanMember(db.QueryRow(ctx, memberSelect+` WHERE m.workspace_id = $1 AND m.id = $2`, wsID, mid))
	if store.IsNoRows(err) {
		return nil, ErrMemberNotFound
	}
	return m, err
}

// ListMembers implements Store.
func (s *PgStore) ListMembers(ctx context.Context, wsID int64) ([]Member, error) {
	rows, err := s.DB.Query(ctx, memberSelect+` WHERE m.workspace_id = $1
		ORDER BY CASE m.role WHEN 'owner' THEN 1 WHEN 'admin' THEN 2 WHEN 'member' THEN 3 ELSE 4 END, m.id`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// AddMember implements Store.
func (s *PgStore) AddMember(ctx context.Context, wsID int64, in MemberInput, by string) (*Member, error) {
	var id int64
	err := s.DB.QueryRow(ctx, `INSERT INTO workspace_members (workspace_id, user_id, group_id, role, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, wsID, in.UserID, in.GroupID, in.Role, by).Scan(&id)
	if err != nil {
		if code, cons := pgErr(err); code == "23505" && strings.HasPrefix(cons, "workspace_members") {
			return nil, ErrMemberExists
		}
		return nil, fkErr(err)
	}
	return s.getMember(ctx, s.DB, wsID, id)
}

// lockOwnerChange locks the workspace row and reports whether member mid may
// stop being an owner. It returns the member's current role.
func lockOwnerChange(ctx context.Context, tx pgx.Tx, wsID, mid int64) (string, bool, error) {
	var x int64
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id = $1 FOR UPDATE`, wsID).Scan(&x); err != nil {
		if store.IsNoRows(err) {
			return "", false, ErrWorkspaceNotFound
		}
		return "", false, err
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM workspace_members WHERE workspace_id = $1 AND id = $2`, wsID, mid).Scan(&role); err != nil {
		if store.IsNoRows(err) {
			return "", false, ErrMemberNotFound
		}
		return "", false, err
	}
	var others int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'owner' AND id <> $2`, wsID, mid).Scan(&others); err != nil {
		return "", false, err
	}
	return role, OwnerRemovalAllowed(role == rbac.WorkspaceOwner, others), nil
}

// UpdateMemberRole implements Store.
func (s *PgStore) UpdateMemberRole(ctx context.Context, wsID, mid int64, role string) (*Member, error) {
	var out *Member
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		cur, ok, err := lockOwnerChange(ctx, tx, wsID, mid)
		if err != nil {
			return err
		}
		if cur == rbac.WorkspaceOwner && role != rbac.WorkspaceOwner && !ok {
			return ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND id = $2`, wsID, mid, role); err != nil {
			return err
		}
		out, err = s.getMember(ctx, tx, wsID, mid)
		return err
	})
	return out, err
}

// DeleteMember implements Store.
func (s *PgStore) DeleteMember(ctx context.Context, wsID, mid int64) (*Member, error) {
	var out *Member
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		_, ok, err := lockOwnerChange(ctx, tx, wsID, mid)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLastOwner
		}
		if out, err = s.getMember(ctx, tx, wsID, mid); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1 AND id = $2`, wsID, mid)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------- projects

func (s *PgStore) clusterTable(ctx context.Context) (bool, error) {
	var ok bool
	err := s.DB.QueryRow(ctx, `SELECT to_regclass('cluster_clusters') IS NOT NULL`).Scan(&ok)
	return ok, err
}

// clusterExists reports whether cluster id exists; true when the cluster
// module is not installed (nothing to check against).
func (s *PgStore) clusterExists(ctx context.Context, id int64) (bool, error) {
	has, err := s.clusterTable(ctx)
	if err != nil || !has {
		return true, err
	}
	var ok bool
	err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cluster_clusters WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

func (s *PgStore) projectSelect(ctx context.Context) (string, error) {
	has, err := s.clusterTable(ctx)
	if err != nil {
		return "", err
	}
	if !has {
		return `SELECT p.id, p.workspace_id, p.cluster_id, '', '', FALSE, p.namespace, p.created_by, p.created_at
			FROM workspace_projects p`, nil
	}
	return `SELECT p.id, p.workspace_id, p.cluster_id, COALESCE(c.name, ''), COALESCE(c.env, ''), c.id IS NOT NULL,
		p.namespace, p.created_by, p.created_at
		FROM workspace_projects p LEFT JOIN cluster_clusters c ON c.id = p.cluster_id`, nil
}

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.ClusterID, &p.ClusterName, &p.ClusterEnv, &p.ClusterExists,
		&p.Namespace, &p.CreatedBy, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) getProject(ctx context.Context, wsID, pid int64) (*Project, error) {
	q, err := s.projectSelect(ctx)
	if err != nil {
		return nil, err
	}
	p, err := scanProject(s.DB.QueryRow(ctx, q+` WHERE p.workspace_id = $1 AND p.id = $2`, wsID, pid))
	if store.IsNoRows(err) {
		return nil, ErrProjectNotFound
	}
	return p, err
}

// ListProjects implements Store.
func (s *PgStore) ListProjects(ctx context.Context, wsID int64) ([]Project, error) {
	q, err := s.projectSelect(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, q+` WHERE p.workspace_id = $1 ORDER BY p.cluster_id, p.namespace`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// AddProject implements Store.
func (s *PgStore) AddProject(ctx context.Context, wsID int64, in ProjectInput, by string) (*Project, error) {
	ok, err := s.clusterExists(ctx, in.ClusterID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrClusterNotFound
	}
	var id int64
	err = s.DB.QueryRow(ctx, `INSERT INTO workspace_projects (workspace_id, cluster_id, namespace, created_by)
		VALUES ($1, $2, $3, $4) RETURNING id`, wsID, in.ClusterID, in.Namespace, by).Scan(&id)
	if err != nil {
		if code, _ := pgErr(err); code == "23505" {
			var name string
			_ = s.DB.QueryRow(ctx, `SELECT w.name FROM workspace_projects p JOIN workspaces w ON w.id = p.workspace_id
				WHERE p.cluster_id = $1 AND p.namespace = $2`, in.ClusterID, in.Namespace).Scan(&name)
			return nil, &NamespaceTakenError{Workspace: name}
		}
		return nil, fkErr(err)
	}
	return s.getProject(ctx, wsID, id)
}

// DeleteProject implements Store.
func (s *PgStore) DeleteProject(ctx context.Context, wsID, pid int64) (*Project, error) {
	p, err := s.getProject(ctx, wsID, pid)
	if err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM workspace_projects WHERE workspace_id = $1 AND id = $2`, wsID, pid)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrProjectNotFound
	}
	return p, nil
}

// ---------------------------------------------------------------- role bindings

const bindingSelect = `SELECT b.id, b.role_id, r.code, r.name, b.user_id, b.group_id,
	COALESCE(u.username, g.name, ''), b.scope, b.scope_value, b.created_by, b.created_at
	FROM role_bindings b
	JOIN roles r ON r.id = b.role_id
	LEFT JOIN users u ON u.id = b.user_id
	LEFT JOIN user_groups g ON g.id = b.group_id`

func scanBinding(row pgx.Row) (*RoleBinding, error) {
	var b RoleBinding
	if err := row.Scan(&b.ID, &b.RoleID, &b.RoleCode, &b.RoleName, &b.UserID, &b.GroupID,
		&b.SubjectName, &b.Scope, &b.ScopeValue, &b.CreatedBy, &b.CreatedAt); err != nil {
		return nil, err
	}
	b.Kind = "user"
	if b.GroupID != nil {
		b.Kind = "group"
	}
	return &b, nil
}

// bindingWhere builds the WHERE clause of ListBindings (exported for tests via buildBindingWhere).
func buildBindingWhere(f BindingFilter) (string, []any) {
	where := []string{"TRUE"}
	args := []any{}
	add := func(frag string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(frag, len(args)))
	}
	if f.Scope != "" {
		add("b.scope = $%d", f.Scope)
	}
	if f.ScopeValue != "" {
		add("b.scope_value = $%d", f.ScopeValue)
	}
	if f.RoleID > 0 {
		add("b.role_id = $%d", f.RoleID)
	}
	if f.UserID > 0 {
		add("b.user_id = $%d", f.UserID)
	}
	if f.GroupID > 0 {
		add("b.group_id = $%d", f.GroupID)
	}
	if f.Restricted {
		ws, pr := f.Workspaces, f.Projects
		if ws == nil {
			ws = []string{}
		}
		if pr == nil {
			pr = []string{}
		}
		args = append(args, ws, pr)
		where = append(where, fmt.Sprintf("((b.scope = 'workspace' AND b.scope_value = ANY($%d::text[])) OR (b.scope = 'project' AND b.scope_value = ANY($%d::text[])))",
			len(args)-1, len(args)))
	}
	return strings.Join(where, " AND "), args
}

// ListBindings implements Store.
func (s *PgStore) ListBindings(ctx context.Context, f BindingFilter, limit, offset int) ([]RoleBinding, int64, error) {
	cond, args := buildBindingWhere(f)
	var total int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM role_bindings b WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.DB.Query(ctx, bindingSelect+` WHERE `+cond+
		fmt.Sprintf(` ORDER BY b.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []RoleBinding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *b)
	}
	return out, total, rows.Err()
}

// GetBinding implements Store.
func (s *PgStore) GetBinding(ctx context.Context, id int64) (*RoleBinding, error) {
	b, err := scanBinding(s.DB.QueryRow(ctx, bindingSelect+` WHERE b.id = $1`, id))
	if store.IsNoRows(err) {
		return nil, ErrBindingNotFound
	}
	return b, err
}

// CreateBinding implements Store.
func (s *PgStore) CreateBinding(ctx context.Context, in BindingInput, by string) (*RoleBinding, error) {
	var id int64
	err := s.DB.QueryRow(ctx, `INSERT INTO role_bindings (role_id, user_id, group_id, scope, scope_value, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		in.RoleID, in.UserID, in.GroupID, in.Scope, string(in.ScopeValue), by).Scan(&id)
	if err != nil {
		if code, _ := pgErr(err); code == "23505" {
			return nil, ErrBindingExists
		}
		return nil, fkErr(err)
	}
	return s.GetBinding(ctx, id)
}

// DeleteBinding implements Store.
func (s *PgStore) DeleteBinding(ctx context.Context, id int64) (*RoleBinding, error) {
	b, err := s.GetBinding(ctx, id)
	if err != nil {
		return nil, err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM role_bindings WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrBindingNotFound
	}
	return b, nil
}

// RolePermissions implements Store.
func (s *PgStore) RolePermissions(ctx context.Context, roleID int64) ([]string, error) {
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM roles WHERE id = $1)`, roleID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrRoleNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT permission FROM role_permissions WHERE role_id = $1 ORDER BY permission`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CheckScopeTarget implements Store.
func (s *PgStore) CheckScopeTarget(ctx context.Context, scope, value string) error {
	switch scope {
	case rbac.ScopeWorkspace:
		id, _ := strconv.ParseInt(value, 10, 64)
		var ok bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`, id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrWorkspaceNotFound
		}
	case rbac.ScopeCluster, rbac.ScopeProject:
		c, _, _ := strings.Cut(value, "/")
		id, _ := strconv.ParseInt(c, 10, 64)
		ok, err := s.clusterExists(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrClusterNotFound
		}
	}
	return nil
}
