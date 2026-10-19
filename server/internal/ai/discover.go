package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 模型发现：用户填好 Base URL 和 API Key 后，直接问上游要模型清单，
// 不用手抄模型 ID，也不用猜上下文长度（上游给了就用，没给就留空由用户填）。

type DiscoverRequest struct {
	BaseURL string `json:"base_url" binding:"required,max=300"`
	APIKey  string `json:"api_key" binding:"max=500"`
}

type DiscoveredModel struct {
	ID  string `json:"id"`
	Ctx int    `json:"context_length,omitempty"`
}

// RegisterDiscover mounts POST /ai/models/discover.
func (s *Service) RegisterDiscover(r *gin.RouterGroup) {
	r.POST("/ai/models/discover", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		var in DiscoverRequest
		if !httpx.Bind(c, &in) {
			return
		}
		base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			httpx.Fail(c, httpx.BadRequest("Base URL 需以 http:// 或 https:// 开头"))
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("Base URL 不合法"))
			return
		}
		if k := strings.TrimSpace(in.APIKey); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("无法连接上游："+err.Error()))
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 300 {
			httpx.Fail(c, httpx.BadRequest("上游返回 "+resp.Status+"，请检查 Base URL 与 API Key"))
			return
		}
		// OpenAI 兼容：{"data":[{"id":"...","context_length":N}]}；部分厂商用 {"models":[...]}
		var payload struct {
			Data   []DiscoveredModel `json:"data"`
			Models []DiscoveredModel `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			httpx.Fail(c, httpx.BadRequest("上游返回的不是 OpenAI 兼容的模型清单"))
			return
		}
		list := payload.Data
		if len(list) == 0 {
			list = payload.Models
		}
		out := make([]DiscoveredModel, 0, len(list))
		for _, m := range list {
			if strings.TrimSpace(m.ID) != "" {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			httpx.Fail(c, httpx.BadRequest("上游没有返回任何模型，请确认 Base URL 是否到 /v1"))
			return
		}
		httpx.OK(c, gin.H{"models": out, "count": len(out)})
	})
}

// 真实连通性测试：真的向上游发一次 chat/completions，拿不到就如实报错，
// 不做"模拟成功"。max_tokens=1 只验证鉴权与路由，不浪费额度。
type TestRequest struct {
	BaseURL string `json:"base_url" binding:"required,max=300"`
	APIKey  string `json:"api_key" binding:"max=500"`
	Model   string `json:"model" binding:"max=120"`
	ModelID int64  `json:"model_id"`
}

func (s *Service) RegisterTest(r *gin.RouterGroup) {
	r.POST("/ai/models/test", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		var in TestRequest
		if !httpx.Bind(c, &in) {
			return
		}
		base := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
		// 已保存的模型：前端拿不到明文密钥，按 id 从库里解密取用
		if in.ModelID > 0 {
			if m, err := s.Store.GetModel(c.Request.Context(), in.ModelID); err == nil {
				if base == "" {
					base = strings.TrimRight(m.BaseURL, "/")
				}
				if in.Model == "" {
					in.Model = m.Model
				}
				if strings.TrimSpace(in.APIKey) == "" {
					if k, kerr := s.Store.APIKey(m); kerr == nil {
						in.APIKey = k
					}
				}
			}
		}
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			httpx.Fail(c, httpx.BadRequest("Base URL 需以 http:// 或 https:// 开头"))
			return
		}
		body, _ := json.Marshal(map[string]any{
			"model":      in.Model,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
		})
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", strings.NewReader(string(body)))
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("Base URL 不合法"))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if k := strings.TrimSpace(in.APIKey); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		start := time.Now()
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("无法连接上游："+err.Error()))
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		ms := int(time.Since(start).Milliseconds())
		if resp.StatusCode >= 300 {
			msg := strings.TrimSpace(string(raw))
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
			httpx.Fail(c, httpx.BadRequest("上游返回 "+resp.Status+"："+msg))
			return
		}
		httpx.OK(c, gin.H{"ok": true, "latency_ms": ms, "model": in.Model})
	})
}
