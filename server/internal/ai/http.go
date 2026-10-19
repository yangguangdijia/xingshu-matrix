package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// sseEmitter writes Server-Sent Events, committing headers on first write.
type sseEmitter struct {
	c       *gin.Context
	started bool
}

func (e *sseEmitter) start() {
	if e.started {
		return
	}
	e.started = true
	h := e.c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx buffering
	e.c.Status(http.StatusOK)
}

func (e *sseEmitter) write(s string) error {
	e.start()
	if _, err := e.c.Writer.WriteString(s); err != nil {
		return err
	}
	e.c.Writer.Flush()
	return e.c.Request.Context().Err()
}

func (e *sseEmitter) Chunk(raw []byte) error { return e.write("data: " + string(raw) + "\n\n") }

func (e *sseEmitter) Event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return e.write("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// collector aggregates a chat for stream=false.
type collector struct {
	content strings.Builder
	finish  string
	events  []map[string]any
}

func (c *collector) Chunk(raw []byte) error {
	var ch streamChunk
	if json.Unmarshal(raw, &ch) == nil {
		for _, choice := range ch.Choices {
			if choice.Index == 0 {
				c.content.WriteString(choice.Delta.Content)
				if choice.FinishReason != nil {
					c.finish = *choice.FinishReason
				}
			}
		}
	}
	return nil
}

func (c *collector) Event(name string, data any) error {
	c.events = append(c.events, map[string]any{"event": name, "data": data})
	return nil
}

// RegisterRoutes mounts the AI API on an authenticated group.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	g := r.Group("/ai")

	// --- models
	g.GET("/models", rbac.RequirePerm("ai:read"), func(c *gin.Context) {
		ms, err := s.Store.ListModels(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, ms)
	})
	// lightweight list for chat users (no URLs / key masks)
	g.GET("/models/options", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		ms, err := s.Store.ListModels(c.Request.Context())
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out := []gin.H{}
		for _, m := range ms {
			if m.Enabled {
				out = append(out, gin.H{"id": m.ID, "name": m.Name, "model": m.Model, "is_default": m.IsDefault})
			}
		}
		httpx.OK(c, out)
	})
	g.GET("/models/:id", rbac.RequirePerm("ai:read"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		m, err := s.Store.GetModel(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, m)
	})
	g.POST("/models", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		var in ModelInput
		if !httpx.Bind(c, &in) {
			return
		}
		m, err := s.Store.CreateModel(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "ai_models", fmt.Sprint(m.ID))
		httpx.Created(c, m)
	})
	g.PUT("/models/:id", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in ModelInput
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetResource(c, "ai_models", fmt.Sprint(id))
		m, err := s.Store.UpdateModel(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, m)
	})
	g.DELETE("/models/:id", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "ai_models", fmt.Sprint(id))
		if err := s.Store.DeleteModel(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id})
	})
	g.POST("/models/:id/test", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "ai_models", fmt.Sprint(id))
		col := &collector{}
		p := rbac.PrincipalFrom(c)
		err := s.Chat(c.Request.Context(), p, ChatRequest{ModelID: id, Messages: []Message{TextMessage("user", "ping, reply with: pong")}, MaxTokens: 16}, col)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"ok": true, "reply": col.content.String()})
	})

	// --- prompt templates
	g.GET("/prompts", rbac.RequireAnyPerm("ai:read", "ai:chat"), func(c *gin.Context) {
		ts, err := s.Store.ListPrompts(c.Request.Context(), c.Query("scene"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, ts)
	})
	g.GET("/prompts/:id", rbac.RequireAnyPerm("ai:read", "ai:chat"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		t, err := s.Store.GetPrompt(c.Request.Context(), id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, t)
	})
	g.POST("/prompts", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		var in PromptInput
		if !httpx.Bind(c, &in) {
			return
		}
		t, err := s.Store.CreatePrompt(c.Request.Context(), in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetResource(c, "ai_prompts", fmt.Sprint(t.ID))
		httpx.Created(c, t)
	})
	g.PUT("/prompts/:id", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in PromptInput
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetResource(c, "ai_prompts", fmt.Sprint(id))
		t, err := s.Store.UpdatePrompt(c.Request.Context(), id, in)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, t)
	})
	g.DELETE("/prompts/:id", rbac.RequirePerm("ai:manage"), func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		audit.SetResource(c, "ai_prompts", fmt.Sprint(id))
		if err := s.Store.DeletePrompt(c.Request.Context(), id); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"id": id})
	})

	// --- tools
	g.GET("/tools", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		ts := s.Tools.Available(rbac.PrincipalFrom(c), nil)
		if ts == nil {
			ts = []Tool{}
		}
		httpx.OK(c, ts)
	})
	g.POST("/tool-calls/:id/confirm", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		audit.SetResource(c, "ai_tool_calls", c.Param("id"))
		pc, msg, err := s.ExecuteConfirmed(c.Request.Context(), rbac.PrincipalFrom(c), c.Param("id"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"tool": pc.Tool, "arguments": pc.Arguments, "status": pc.Status})
		httpx.OK(c, gin.H{"call": pc, "message": msg})
	})
	g.POST("/tool-calls/:id/reject", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		audit.SetResource(c, "ai_tool_calls", c.Param("id"))
		pc, msg, err := s.RejectConfirmed(c.Request.Context(), rbac.PrincipalFrom(c), c.Param("id"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		audit.SetDetail(c, gin.H{"tool": pc.Tool, "status": pc.Status})
		httpx.OK(c, gin.H{"call": pc, "message": msg})
	})

	// --- chat
	g.POST("/chat", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		audit.Skip(c) // chat is read-like; executed tools are audited via their own module calls / confirm endpoint
		var req ChatRequest
		if !httpx.Bind(c, &req) {
			return
		}
		p := rbac.PrincipalFrom(c)
		if req.Stream != nil && !*req.Stream {
			col := &collector{}
			if err := s.Chat(c.Request.Context(), p, req, col); err != nil {
				httpx.Fail(c, err)
				return
			}
			events := col.events
			if events == nil {
				events = []map[string]any{}
			}
			httpx.OK(c, gin.H{"content": col.content.String(), "finish_reason": col.finish, "events": events})
			return
		}
		em := &sseEmitter{c: c}
		err := s.Chat(c.Request.Context(), p, req, em)
		if err != nil && !em.started {
			httpx.Fail(c, err)
			return
		}
		if err != nil {
			var he *httpx.Error
			payload := map[string]any{"message": err.Error()}
			if errors.As(err, &he) {
				payload = map[string]any{"code": he.Code, "message": he.Message, "details": he.Details}
			}
			_ = c.Error(err)
			_ = em.Event("error", payload)
		}
		_ = em.write("data: [DONE]\n\n")
	})
	s.RegisterAssist(r)
	s.RegisterDiscover(r)
	s.RegisterTest(r)
}
