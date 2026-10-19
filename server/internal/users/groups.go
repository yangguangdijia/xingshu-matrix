package users

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// 用户组：把一组用户与一组角色绑定，成员通过组间接获得角色。
// 组本身不直接持有权限点，权限仍由角色决定。

type UserGroup struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	MemberIDs   []int64   `json:"member_ids"`
	Members     []string  `json:"members"`
	RoleIDs     []int64   `json:"role_ids"`
	Roles       []string  `json:"roles"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type UserGroupInput struct {
	Name        string  `json:"name" binding:"required,max=64"`
	Description string  `json:"description" binding:"max=200"`
	MemberIDs   []int64 `json:"member_ids"`
	RoleIDs     []int64 `json:"role_ids"`
}

const groupSelect = `SELECT g.id, g.name, g.description, g.created_at, g.updated_at,
  COALESCE((SELECT array_agg(m.user_id ORDER BY m.user_id) FROM user_group_members m WHERE m.group_id=g.id), '{}'),
  COALESCE((SELECT array_agg(u.username ORDER BY u.username) FROM user_group_members m JOIN users u ON u.id=m.user_id WHERE m.group_id=g.id), '{}'),
  COALESCE((SELECT array_agg(r.role_id ORDER BY r.role_id) FROM user_group_roles r WHERE r.group_id=g.id), '{}'),
  COALESCE((SELECT array_agg(ro.name ORDER BY ro.name) FROM user_group_roles r JOIN roles ro ON ro.id=r.role_id WHERE r.group_id=g.id), '{}')
  FROM user_groups g`

func (s *Service) ListUserGroups(ctx context.Context) ([]UserGroup, error) {
	rows, err := s.DB.Query(ctx, groupSelect+` ORDER BY g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserGroup, 0, 8)
	for rows.Next() {
		var g UserGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt, &g.MemberIDs, &g.Members, &g.RoleIDs, &g.Roles); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) GetUserGroup(ctx context.Context, id int64) (*UserGroup, error) {
	var g UserGroup
	err := s.DB.QueryRow(ctx, groupSelect+` WHERE g.id=$1`, id).Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt, &g.MemberIDs, &g.Members, &g.RoleIDs, &g.Roles)
	if err != nil {
		if store.IsNoRows(err) {
			return nil, httpx.NotFound("用户组不存在")
		}
		return nil, err
	}
	return &g, nil
}

func (s *Service) saveGroupLinks(ctx context.Context, id int64, in UserGroupInput) error {
	if _, err := s.DB.Exec(ctx, `DELETE FROM user_group_members WHERE group_id=$1`, id); err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM user_group_roles WHERE group_id=$1`, id); err != nil {
		return err
	}
	for _, uid := range in.MemberIDs {
		if _, err := s.DB.Exec(ctx, `INSERT INTO user_group_members(group_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, uid); err != nil {
			return err
		}
	}
	for _, rid := range in.RoleIDs {
		if _, err := s.DB.Exec(ctx, `INSERT INTO user_group_roles(group_id,role_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, rid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) CreateUserGroup(ctx context.Context, in UserGroupInput) (*UserGroup, error) {
	var id int64
	err := s.DB.QueryRow(ctx, `INSERT INTO user_groups(name, description) VALUES ($1,$2) RETURNING id`,
		strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)).Scan(&id)
	if err != nil {
		if isUnique(err) {
			return nil, httpx.Conflict("用户组名称已存在")
		}
		return nil, err
	}
	if err := s.saveGroupLinks(ctx, id, in); err != nil {
		return nil, err
	}
	return s.GetUserGroup(ctx, id)
}

func (s *Service) UpdateUserGroup(ctx context.Context, id int64, in UserGroupInput) (*UserGroup, error) {
	if _, err := s.GetUserGroup(ctx, id); err != nil {
		return nil, err
	}
	_, err := s.DB.Exec(ctx, `UPDATE user_groups SET name=$2, description=$3, updated_at=now() WHERE id=$1`,
		id, strings.TrimSpace(in.Name), strings.TrimSpace(in.Description))
	if err != nil {
		if isUnique(err) {
			return nil, httpx.Conflict("用户组名称已存在")
		}
		return nil, err
	}
	if err := s.saveGroupLinks(ctx, id, in); err != nil {
		return nil, err
	}
	return s.GetUserGroup(ctx, id)
}

func (s *Service) DeleteUserGroup(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM user_groups WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("用户组不存在")
	}
	return nil
}

func (s *Service) registerUserGroups(r *gin.RouterGroup, read, manage gin.HandlerFunc) {
	r.GET("/user-groups", read, func(c *gin.Context) {
		items, err := s.ListUserGroups(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"items": items, "total": len(items)})
	})
	r.POST("/user-groups", manage, func(c *gin.Context) {
		var in UserGroupInput
		if !httpx.Bind(c, &in) {
			return
		}
		g, err := s.CreateUserGroup(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "user_groups", fmt.Sprint(g.ID))
		c.JSON(201, gin.H{"code": 0, "message": "created", "data": g})
	})
	r.PUT("/user-groups/:id", manage, func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in UserGroupInput
		if !httpx.Bind(c, &in) {
			return
		}
		g, err := s.UpdateUserGroup(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "user_groups", fmt.Sprint(id))
		httpx.OK(c, g)
	})
	r.DELETE("/user-groups/:id", manage, func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		if err := s.DeleteUserGroup(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "user_groups", fmt.Sprint(id))
		httpx.OK(c, gin.H{"deleted": id})
	})
}
