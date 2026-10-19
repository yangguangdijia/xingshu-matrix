package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
)

// 平台公告：存 settings 表（key=public.notices），所有登录用户可读，setting:manage 可写。
// 之前总览页的公告是前端内存里的假数据，刷新即失；这里换成真实持久化。

const noticesKey = "public.notices"

// Notice is one announcement.
type Notice struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

func registerNotices(r *gin.RouterGroup, s *settings.Service) {
	r.GET("/platform/notices", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		items, err := loadNotices(c.Request.Context(), s)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		httpx.OK(c, gin.H{"items": items, "total": len(items)})
	})

	r.POST("/platform/notices", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var in struct {
			Title  string `json:"title" binding:"required,max=200"`
			Body   string `json:"body" binding:"max=4000"`
			Pinned bool   `json:"pinned"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetResource(c, "platform", "notices")
		audit.SetDetail(c, gin.H{"title": in.Title, "pinned": in.Pinned})
		items, err := loadNotices(c.Request.Context(), s)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		p := rbac.PrincipalFrom(c)
		n := Notice{
			ID: fmt.Sprintf("n%d", time.Now().UnixNano()), Title: strings.TrimSpace(in.Title),
			Body: strings.TrimSpace(in.Body), Pinned: in.Pinned, CreatedAt: time.Now().UTC(),
		}
		if p != nil {
			n.CreatedBy = p.Username
		}
		if n.Pinned {
			for i := range items {
				items[i].Pinned = false
			}
		}
		items = append([]Notice{n}, items...)
		if err := saveNotices(c.Request.Context(), s, items); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		httpx.Created(c, n)
	})

	r.DELETE("/platform/notices/:id", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		id := c.Param("id")
		audit.SetResource(c, "platform", "notices")
		audit.SetDetail(c, gin.H{"id": id})
		items, err := loadNotices(c.Request.Context(), s)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		out := items[:0]
		found := false
		for _, n := range items {
			if n.ID == id {
				found = true
				continue
			}
			out = append(out, n)
		}
		if !found {
			httpx.Fail(c, httpx.ErrNotFound)
			return
		}
		if err := saveNotices(c.Request.Context(), s, out); err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		httpx.OK(c, gin.H{"deleted": id})
	})
}

func loadNotices(ctx context.Context, s *settings.Service) ([]Notice, error) {
	var items []Notice
	err := s.Get(ctx, noticesKey, &items)
	if err != nil {
		// 未配置时返回空列表，而不是报错
		return []Notice{}, nil
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Pinned != items[j].Pinned {
			return items[i].Pinned
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return items, nil
}

func saveNotices(ctx context.Context, s *settings.Service, items []Notice) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	_, err = s.Put(ctx, noticesKey, raw, false, "平台公告（总览页展示）", 0)
	return err
}
