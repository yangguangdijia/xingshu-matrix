package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// 部门（Department）：平台内的组织单元，可手工创建/改名/删除。
// 用户通过 department_id 归属部门；删除部门时把成员置为"未分配"，不级联删人。

type Department struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	ParentID  *int64    `json:"parent_id"`
	Note      string    `json:"note"`
	Headcount int       `json:"headcount"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DepartmentInput struct {
	Name     string `json:"name" binding:"required,max=64"`
	ParentID *int64 `json:"parent_id"`
	Note     string `json:"note" binding:"max=200"`
}

// EnsureDepartmentSchema 幂等建表，兼容升级前已存在的库。
func (s *Service) EnsureDepartmentSchema(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `
CREATE TABLE IF NOT EXISTS departments (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  parent_id BIGINT REFERENCES departments(id) ON DELETE SET NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `ALTER TABLE users ADD COLUMN IF NOT EXISTS department_id BIGINT REFERENCES departments(id) ON DELETE SET NULL`)
	return err
}

const deptCols = `d.id, d.name, d.parent_id, d.note, (SELECT count(*) FROM users u WHERE u.department_id = d.id), d.created_at, d.updated_at`

func scanDept(row pgx.Row) (*Department, error) {
	var d Department
	if err := row.Scan(&d.ID, &d.Name, &d.ParentID, &d.Note, &d.Headcount, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDepartments 返回全部部门（数量有限，不分页），带人数统计。
func (s *Service) ListDepartments(ctx context.Context) ([]Department, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+deptCols+` FROM departments d ORDER BY d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Department, 0, 16)
	for rows.Next() {
		d, err := scanDept(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *Service) GetDepartment(ctx context.Context, id int64) (*Department, error) {
	d, err := scanDept(s.DB.QueryRow(ctx, `SELECT `+deptCols+` FROM departments d WHERE d.id=$1`, id))
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("部门不存在")
	}
	return d, err
}

func (s *Service) CreateDepartment(ctx context.Context, in DepartmentInput) (*Department, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, httpx.BadRequest("部门名称不能为空")
	}
	var id int64
	err := s.DB.QueryRow(ctx, `INSERT INTO departments(name, parent_id, note) VALUES ($1,$2,$3) RETURNING id`,
		name, in.ParentID, strings.TrimSpace(in.Note)).Scan(&id)
	if isUnique(err) {
		return nil, httpx.Conflict("部门名称已存在")
	}
	if err != nil {
		return nil, err
	}
	return s.GetDepartment(ctx, id)
}

func (s *Service) UpdateDepartment(ctx context.Context, id int64, in DepartmentInput) (*Department, error) {
	if _, err := s.GetDepartment(ctx, id); err != nil {
		return nil, err
	}
	if in.ParentID != nil && *in.ParentID == id {
		return nil, httpx.BadRequest("上级部门不能是自己")
	}
	_, err := s.DB.Exec(ctx, `UPDATE departments SET name=$2, parent_id=$3, note=$4, updated_at=now() WHERE id=$1`,
		id, strings.TrimSpace(in.Name), in.ParentID, strings.TrimSpace(in.Note))
	if isUnique(err) {
		return nil, httpx.Conflict("部门名称已存在")
	}
	if err != nil {
		return nil, err
	}
	return s.GetDepartment(ctx, id)
}

// DeleteDepartment 删除部门；成员保留但归属清空（ON DELETE SET NULL）。
func (s *Service) DeleteDepartment(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM departments WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("部门不存在")
	}
	return nil
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return err != nil && errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// registerDepartments 挂在 /departments 下，权限沿用 user:read / user:manage。
func (s *Service) registerDepartments(r *gin.RouterGroup, read, manage gin.HandlerFunc) {
	r.GET("/departments", read, func(c *gin.Context) {
		items, err := s.ListDepartments(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"items": items, "total": len(items)})
	})

	r.POST("/departments", manage, func(c *gin.Context) {
		var in DepartmentInput
		if !httpx.Bind(c, &in) {
			return
		}
		d, err := s.CreateDepartment(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "departments", fmt.Sprint(d.ID))
		c.JSON(201, gin.H{"code": 0, "message": "created", "data": d})
	})

	r.PUT("/departments/:id", manage, func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in DepartmentInput
		if !httpx.Bind(c, &in) {
			return
		}
		d, err := s.UpdateDepartment(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "departments", fmt.Sprint(id))
		httpx.OK(c, d)
	})

	r.DELETE("/departments/:id", manage, func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		if err := s.DeleteDepartment(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "departments", fmt.Sprint(id))
		httpx.OK(c, gin.H{"deleted": id})
	})
}
