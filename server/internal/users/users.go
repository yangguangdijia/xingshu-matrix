// Package users implements user administration (CRUD, status, password reset,
// unlock, role assignment).
package users

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// RoleRef is a compact role reference.
type RoleRef struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// User is the admin view of a user.
type User struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"display_name"`
	Email              string     `json:"email"`
	Phone              string     `json:"phone"`
	Status             string     `json:"status"`
	IsBuiltin          bool       `json:"is_builtin"`
	MustChangePassword bool       `json:"must_change_password"`
	Locked             bool       `json:"locked"`
	LockedUntil        *time.Time `json:"locked_until"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	LastLoginIP        string     `json:"last_login_ip"`
	PasswordChangedAt  *time.Time `json:"password_changed_at"`
	Roles              []RoleRef  `json:"roles"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// Sessions is what users needs from auth.
type Sessions interface {
	RevokeAllSessions(ctx context.Context, userID int64) error
	InvalidateUser(userID int64)
}

// Service implements user administration.
type Service struct {
	DB       store.DB
	RBAC     *rbac.Service
	Sessions Sessions
}

var usernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{2,63}$`)

const userSelect = `SELECT u.id, u.username, u.display_name, u.email, u.phone, u.status, u.is_builtin, u.must_change_password,
	u.locked_until, u.last_login_at, u.last_login_ip, u.password_changed_at, u.created_at, u.updated_at,
	COALESCE((SELECT json_agg(json_build_object('id', r.id, 'code', r.code, 'name', r.name) ORDER BY r.id)
	          FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=u.id), '[]'::json)
	FROM users u`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Phone, &u.Status, &u.IsBuiltin, &u.MustChangePassword,
		&u.LockedUntil, &u.LastLoginAt, &u.LastLoginIP, &u.PasswordChangedAt, &u.CreatedAt, &u.UpdatedAt, &u.Roles)
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("user not found")
	}
	if err != nil {
		return nil, err
	}
	u.Locked = u.LockedUntil != nil && u.LockedUntil.After(time.Now())
	if u.Roles == nil {
		u.Roles = []RoleRef{}
	}
	return &u, nil
}

// Get returns one user.
func (s *Service) Get(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, userSelect+` WHERE u.id=$1`, id))
}

// ListFilter narrows List.
type ListFilter struct {
	Keyword string
	Status  string
	RoleID  int64
}

// List returns a page of users.
func (s *Service) List(ctx context.Context, f ListFilter, p httpx.Page) ([]User, int64, error) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Keyword != "" {
		add(`(u.username ILIKE '%%' || $%[1]d::text || '%%' OR u.display_name ILIKE '%%' || $%[1]d::text || '%%' OR u.email ILIKE '%%' || $%[1]d::text || '%%')`, f.Keyword)
	}
	if f.Status != "" {
		add(`u.status = $%d`, f.Status)
	}
	if f.RoleID > 0 {
		add(`EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id=u.id AND ur.role_id=$%d)`, f.RoleID)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM users u`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, p.Limit(), p.Offset())
	rows, err := s.DB.Query(ctx, fmt.Sprintf(`%s%s ORDER BY u.id LIMIT $%d OFFSET $%d`, userSelect, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// CreateInput creates a user. Password empty = generate one (returned once).
type CreateInput struct {
	Username           string  `json:"username" binding:"required"`
	DisplayName        string  `json:"display_name" binding:"max=64"`
	Email              string  `json:"email" binding:"max=128"`
	Phone              string  `json:"phone" binding:"max=32"`
	Password           string  `json:"password" binding:"max=72"`
	RoleIDs            []int64 `json:"role_ids"`
	MustChangePassword *bool   `json:"must_change_password"`
}

// UpdateInput updates profile fields; RoleIDs nil keeps roles.
type UpdateInput struct {
	DisplayName string   `json:"display_name" binding:"max=64"`
	Email       string   `json:"email" binding:"max=128"`
	Phone       string   `json:"phone" binding:"max=32"`
	RoleIDs     *[]int64 `json:"role_ids"`
}

func validateProfile(email string) error {
	if email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			return httpx.Validation("invalid email")
		}
	}
	return nil
}

// Create inserts a user and returns it plus the generated password (if any).
func (s *Service) Create(ctx context.Context, in CreateInput) (*User, string, error) {
	in.Username = strings.TrimSpace(in.Username)
	if !usernameRe.MatchString(in.Username) {
		return nil, "", httpx.Validation("用户名需以字母开头，3-64 位字母/数字/._-")
	}
	if err := validateProfile(in.Email); err != nil {
		return nil, "", err
	}
	generated := ""
	if in.Password == "" {
		generated = GeneratePassword(16)
		in.Password = generated
	} else if err := auth.ValidatePassword(in.Password, in.Username); err != nil {
		return nil, "", err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, "", err
	}
	mustChange := in.MustChangePassword == nil || *in.MustChangePassword
	if generated != "" {
		mustChange = true
	}
	var id int64
	err = store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO users(username, display_name, email, phone, password_hash, must_change_password)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			in.Username, in.DisplayName, in.Email, in.Phone, hash, mustChange).Scan(&id); err != nil {
			return err
		}
		return s.RBAC.SetUserRoles(ctx, tx, id, in.RoleIDs)
	})
	if store.IsUniqueViolation(err) {
		return nil, "", httpx.Conflict("用户名已存在")
	}
	if err != nil {
		return nil, "", err
	}
	u, err := s.Get(ctx, id)
	return u, generated, err
}

// Update changes profile fields and optionally roles.
func (s *Service) Update(ctx context.Context, id int64, in UpdateInput) (*User, error) {
	if err := validateProfile(in.Email); err != nil {
		return nil, err
	}
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET display_name=$2, email=$3, phone=$4, updated_at=now() WHERE id=$1`,
			id, in.DisplayName, in.Email, in.Phone)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("user not found")
		}
		if in.RoleIDs != nil {
			if err := s.RBAC.SetUserRoles(ctx, tx, id, *in.RoleIDs); err != nil {
				return err
			}
			return ensureAdminRemains(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Sessions.InvalidateUser(id)
	return s.Get(ctx, id)
}

// SetRoles replaces a user's roles.
func (s *Service) SetRoles(ctx context.Context, id int64, roleIDs []int64) (*User, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if err := s.RBAC.SetUserRoles(ctx, tx, id, roleIDs); err != nil {
			return err
		}
		return ensureAdminRemains(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	s.Sessions.InvalidateUser(id)
	return s.Get(ctx, id)
}

// ensureAdminRemains fails when no active user holds the admin role anymore.
func ensureAdminRemains(ctx context.Context, tx pgx.Tx) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users u JOIN user_roles ur ON ur.user_id=u.id
		JOIN roles r ON r.id=ur.role_id WHERE r.code='admin' AND u.status='active'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return httpx.Conflict("至少需要保留一个启用状态的超级管理员")
	}
	return nil
}

// SetStatus enables or disables a user; disabling revokes all sessions.
func (s *Service) SetStatus(ctx context.Context, actor *rbac.Principal, id int64, status string) (*User, error) {
	if status != "active" && status != "disabled" {
		return nil, httpx.Validation("status must be active or disabled")
	}
	if status == "disabled" && actor.UserID == id {
		return nil, httpx.Forbidden("不能禁用自己")
	}
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET status=$2, updated_at=now() WHERE id=$1`, id, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return httpx.NotFound("user not found")
		}
		return ensureAdminRemains(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	if status == "disabled" {
		if err := s.Sessions.RevokeAllSessions(ctx, id); err != nil {
			return nil, err
		}
	}
	s.Sessions.InvalidateUser(id)
	return s.Get(ctx, id)
}

// ResetPassword sets a new password (generated when empty), forces a change at
// next login and revokes all sessions. Returns the generated password, if any.
func (s *Service) ResetPassword(ctx context.Context, id int64, password string) (string, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	generated := ""
	if password == "" {
		generated = GeneratePassword(16)
		password = generated
	} else if err := auth.ValidatePassword(password, u.Username); err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET password_hash=$2, must_change_password=TRUE, password_changed_at=now(),
		failed_attempts=0, locked_until=NULL, updated_at=now() WHERE id=$1`, id, hash); err != nil {
		return "", err
	}
	return generated, s.Sessions.RevokeAllSessions(ctx, id)
}

// Unlock clears a login lockout.
func (s *Service) Unlock(ctx context.Context, id int64) (*User, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE users SET failed_attempts=0, locked_until=NULL, updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NotFound("user not found")
	}
	return s.Get(ctx, id)
}

// Delete removes a non-builtin user other than the caller.
func (s *Service) Delete(ctx context.Context, actor *rbac.Principal, id int64) error {
	if actor.UserID == id {
		return httpx.Forbidden("不能删除自己")
	}
	u, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.IsBuiltin {
		return httpx.Forbidden("内置用户不能删除，可禁用")
	}
	err = store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, id); err != nil {
			return err
		}
		return ensureAdminRemains(ctx, tx)
	})
	if err != nil {
		return err
	}
	s.Sessions.InvalidateUser(id)
	return nil
}

// GeneratePassword returns a random password satisfying the policy.
func GeneratePassword(n int) string {
	const (
		lower  = "abcdefghijkmnpqrstuvwxyz"
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		digits = "23456789"
		syms   = "!@#$%^&*-_=+"
	)
	all := lower + upper + digits + syms
	pick := func(set string) byte {
		i, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[i.Int64()]
	}
	if n < 12 {
		n = 12
	}
	b := []byte{pick(lower), pick(upper), pick(digits), pick(syms)}
	for len(b) < n {
		b = append(b, pick(all))
	}
	for i := len(b) - 1; i > 0; i-- { // Fisher-Yates
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b)
}

// RegisterRoutes mounts /users endpoints.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	read, manage := rbac.RequirePerm("user:read"), rbac.RequirePerm("user:manage")
	s.registerDepartments(r, read, manage)

	r.GET("/users", read, func(c *gin.Context) {
		p := httpx.ParsePage(c)
		f := ListFilter{Keyword: c.Query("q"), Status: c.Query("status")}
		fmt.Sscan(c.Query("role_id"), &f.RoleID)
		items, total, err := s.List(c.Request.Context(), f, p)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, httpx.NewPageResult(items, total, p))
	})
	r.GET("/users/:id", read, withID(func(c *gin.Context, id int64) {
		u, err := s.Get(c.Request.Context(), id)
		respond(c, u, err)
	}))
	r.POST("/users", manage, func(c *gin.Context) {
		var in CreateInput
		if !httpx.Bind(c, &in) {
			return
		}
		u, pw, err := s.Create(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "users", fmt.Sprint(u.ID))
		audit.SetDetail(c, gin.H{"username": u.Username, "role_ids": in.RoleIDs})
		data := gin.H{"user": u}
		if pw != "" {
			data["initial_password"] = pw
		}
		httpx.Created(c, data)
	})
	r.PUT("/users/:id", manage, withID(func(c *gin.Context, id int64) {
		var in UpdateInput
		if !httpx.Bind(c, &in) {
			return
		}
		u, err := s.Update(c.Request.Context(), id, in)
		respond(c, u, err)
	}))
	r.PUT("/users/:id/roles", manage, withID(func(c *gin.Context, id int64) {
		var in struct {
			RoleIDs []int64 `json:"role_ids"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetDetail(c, gin.H{"role_ids": in.RoleIDs})
		u, err := s.SetRoles(c.Request.Context(), id, in.RoleIDs)
		respond(c, u, err)
	}))
	r.PUT("/users/:id/status", manage, withID(func(c *gin.Context, id int64) {
		var in struct {
			Status string `json:"status" binding:"required"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetDetail(c, gin.H{"status": in.Status})
		u, err := s.SetStatus(c.Request.Context(), rbac.PrincipalFrom(c), id, in.Status)
		respond(c, u, err)
	}))
	r.POST("/users/:id/reset-password", manage, withID(func(c *gin.Context, id int64) {
		var in struct {
			Password string `json:"password" binding:"max=72"`
		}
		if c.Request.ContentLength != 0 && !httpx.Bind(c, &in) {
			return
		}
		pw, err := s.ResetPassword(c.Request.Context(), id, in.Password)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		data := gin.H{"must_change_password": true}
		if pw != "" {
			data["password"] = pw
		}
		httpx.OK(c, data)
	}))
	r.POST("/users/:id/unlock", manage, withID(func(c *gin.Context, id int64) {
		u, err := s.Unlock(c.Request.Context(), id)
		respond(c, u, err)
	}))
	r.DELETE("/users/:id", manage, withID(func(c *gin.Context, id int64) {
		if err := s.Delete(c.Request.Context(), rbac.PrincipalFrom(c), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id})
	}))
}

func withID(fn func(*gin.Context, int64)) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		fn(c, id)
	}
}

func respond(c *gin.Context, v any, err error) {
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}
