package ai

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// 通用表单助手：把一句自然语言翻译成某个对话框需要的字段 JSON。
//
// 前端每个"新建/编辑"对话框把字段清单（schema）和用户那句话传进来，
// 这里用平台已配置的默认模型生成 JSON，前端再回填表单——创建、修改、查询、
// 告警规则等对话框都复用同一个接口，不必各写一套提示词。

// AssistRequest is the payload of POST /ai/assist.
type AssistRequest struct {
	Scene       string          `json:"scene" binding:"required,max=64"`
	Instruction string          `json:"instruction" binding:"required,max=4000"`
	Schema      json.RawMessage `json:"schema" binding:"max=20000"`
	ModelID     int64           `json:"model_id"`
}

const assistSystem = `你是「星枢 Matrix」的表单填充助手。
用户会用一句中文描述想创建/修改/查询的内容，你的任务是输出**严格合法的 JSON 对象**：
- 只输出 JSON，不要 Markdown 代码块、不要解释、不要多余文字。
- 键名必须来自用户给出的字段定义；不要发明新键。
- 无法确定的字段：字符串给 ""，数字给 0，布尔给 false，数组给 []。
- 枚举字段只能取定义里列出的值。
- 值要符合字段名语义（例如 name 用英文小写短横线，replicas 用整数）。`

// RegisterAssist mounts the form-assist endpoint on the authenticated group.
func (s *Service) RegisterAssist(r *gin.RouterGroup) {
	r.POST("/ai/assist", rbac.RequirePerm("ai:chat"), func(c *gin.Context) {
		var in AssistRequest
		if !httpx.Bind(c, &in) {
			return
		}
		sys := assistSystem
		if len(in.Schema) > 0 {
			sys += "\n\n字段定义：\n" + string(in.Schema)
		}
		req := ChatRequest{
			ModelID: in.ModelID,
			Scene:   in.Scene,
			Messages: []Message{
				TextMessage("system", sys),
				TextMessage("user", in.Instruction),
			},
			Tools: json.RawMessage(`[]`), // 表单填充不需要工具调用
		}
		col := &chunkCollector{}
		if err := s.Chat(c.Request.Context(), rbac.PrincipalFrom(c), req, col); err != nil {
			httpx.Fail(c, err)
			return
		}
		raw := strings.TrimSpace(col.text())
		raw = trimCodeFence(raw)
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			httpx.Fail(c, httpx.BadRequest("模型没有返回合法 JSON，请换个说法重试"))
			return
		}
		httpx.OK(c, gin.H{"fields": fields, "raw": raw})
	})
}

// chunkCollector accumulates streamed chat.completion.chunk deltas into text.
type chunkCollector struct{ b strings.Builder }

func (c *chunkCollector) text() string { return c.b.String() }

func (c *chunkCollector) Chunk(raw []byte) error {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return nil // 非标准分片直接忽略，不影响整体
	}
	for _, ch := range chunk.Choices {
		c.b.WriteString(ch.Delta.Content)
	}
	return nil
}

func (c *chunkCollector) Event(string, any) error { return nil }

// trimCodeFence removes ```json ... ``` wrappers some models add despite instructions.
func trimCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	s = strings.TrimPrefix(s, "\n")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
