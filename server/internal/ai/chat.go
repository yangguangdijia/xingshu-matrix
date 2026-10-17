package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ChatRequest is the body of POST /api/v1/ai/chat.
type ChatRequest struct {
	ModelID    int64          `json:"model_id"`
	Scene      string         `json:"scene"`
	TemplateID int64          `json:"template_id"`
	Variables  map[string]any `json:"variables"`
	Messages   []Message      `json:"messages" binding:"required,min=1,max=200"`
	// Tools: true offers every tool the user may use; a list restricts to names.
	Tools       json.RawMessage `json:"tools"`
	Stream      *bool           `json:"stream"`
	Temperature *float32        `json:"temperature"`
	MaxTokens   int             `json:"max_tokens"`
}

// toolSelection parses the "tools" field: true/false or ["name", ...].
func (r *ChatRequest) toolSelection() (enabled bool, names []string, err error) {
	if len(r.Tools) == 0 || string(r.Tools) == "null" {
		return false, nil, nil
	}
	var b bool
	if json.Unmarshal(r.Tools, &b) == nil {
		return b, nil, nil
	}
	if err := json.Unmarshal(r.Tools, &names); err != nil {
		return false, nil, httpx.Validation("tools must be a boolean or an array of tool names")
	}
	return len(names) > 0, names, nil
}

// Emitter receives chat events (SSE writer or JSON collector).
type Emitter interface {
	// Chunk forwards one raw upstream chat.completion.chunk.
	Chunk(raw []byte) error
	// Event emits a named platform event (tool_call, tool_result, tool_confirmation, error).
	Event(name string, data any) error
}

// Service is the AI chat service.
type Service struct {
	Store         *Store
	Tools         *ToolRegistry
	HTTP          *http.Client
	Log           *slog.Logger
	Timeout       time.Duration
	MaxToolRounds int
	// ConfirmTTL is how long a pending tool confirmation stays valid.
	ConfirmTTL time.Duration
}

const maxToolResultBytes = 16 * 1024

// Chat runs a (possibly multi-round, tool-calling) conversation and streams it to em.
func (s *Service) Chat(ctx context.Context, p *rbac.Principal, req ChatRequest, em Emitter) error {
	if err := validateMessages(req.Messages); err != nil {
		return err
	}
	model, err := s.Store.ResolveModel(ctx, req.ModelID)
	if err != nil {
		return err
	}
	apiKey, err := s.Store.APIKey(model)
	if err != nil {
		return httpx.Internal(fmt.Errorf("decrypt model api key: %w", err))
	}

	msgs := make([]Message, 0, len(req.Messages)+1)
	sys, err := s.systemPrompt(ctx, req)
	if err != nil {
		return err
	}
	if sys != "" {
		msgs = append(msgs, TextMessage("system", sys))
	}
	msgs = append(msgs, req.Messages...)

	toolsOn, names, err := req.toolSelection()
	if err != nil {
		return err
	}
	var tools []Tool
	if toolsOn {
		tools = s.Tools.Available(p, names)
	}

	up := chatRequest{Model: model.Model, Messages: msgs, MaxTokens: model.MaxTokens}
	temp := model.Temperature
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	up.Temperature = &temp
	if req.MaxTokens > 0 {
		up.MaxTokens = req.MaxTokens
	}
	if len(tools) > 0 {
		up.Tools = toOpenAITools(tools)
		up.ToolChoice = "auto"
	}

	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	rounds := s.MaxToolRounds
	for round := 0; ; round++ {
		res, err := streamChat(ctx, s.HTTP, model, apiKey, up, em.Chunk)
		if err != nil {
			var ue *UpstreamError
			if errors.As(err, &ue) {
				return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "AI 模型接口返回错误").WithDetails(map[string]any{"status": ue.Status, "body": ue.Body})
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return httpx.NewError(http.StatusGatewayTimeout, httpx.CodeUpstream, "AI 模型接口超时")
			}
			return httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "AI 模型接口调用失败").Wrap(err)
		}
		if len(res.ToolCalls) == 0 || len(tools) == 0 {
			return nil
		}
		if round >= rounds {
			return em.Event("error", map[string]any{"message": fmt.Sprintf("tool-call rounds exceeded (%d)", rounds)})
		}
		// Assistant turn that requested the tools.
		assistant := Message{Role: "assistant", Content: json.RawMessage("null"), ToolCalls: res.ToolCalls}
		if res.Content.Len() > 0 {
			assistant.Content, _ = json.Marshal(res.Content.String())
		}
		up.Messages = append(up.Messages, assistant)
		appended := []Message{assistant}

		var confirmations []map[string]any
		for _, tc := range res.ToolCalls {
			tool, ok := s.Tools.Get(tc.Function.Name)
			var out Message
			switch {
			case !ok:
				out = toolMessage(tc.ID, map[string]any{"error": "unknown tool " + tc.Function.Name})
			case !p.Can(tool.Permission):
				out = toolMessage(tc.ID, map[string]any{"error": "permission denied: " + tool.Permission})
			case tool.RequiresConfirmation:
				pc := &PendingCall{ID: newCallID(), UserID: p.UserID, Tool: tool.Name, ToolCallID: tc.ID, Arguments: normaliseArgs(tc.Function.Arguments)}
				if err := s.Store.InsertPendingCall(ctx, pc); err != nil {
					return err
				}
				confirmations = append(confirmations, map[string]any{
					"confirmation_id": pc.ID, "tool_call_id": tc.ID, "tool": tool.Name,
					"description": tool.Description, "arguments": pc.Arguments, "expires_at": pc.CreatedAt.Add(s.ConfirmTTL),
				})
				continue
			default:
				_ = em.Event("tool_call", map[string]any{"tool_call_id": tc.ID, "tool": tool.Name, "arguments": normaliseArgs(tc.Function.Arguments)})
				out = s.runTool(ctx, p, tool, tc.ID, normaliseArgs(tc.Function.Arguments))
				_ = em.Event("tool_result", map[string]any{"tool_call_id": tc.ID, "tool": tool.Name, "content": out.Content})
			}
			up.Messages = append(up.Messages, out)
			appended = append(appended, out)
		}
		if len(confirmations) > 0 {
			// Pause: the client confirms/rejects each call, appends the returned
			// tool messages to `messages` and calls /ai/chat again.
			return em.Event("tool_confirmation", map[string]any{"pending": confirmations, "messages": appended})
		}
	}
}

func (s *Service) runTool(ctx context.Context, p *rbac.Principal, tool Tool, callID string, args json.RawMessage) Message {
	ctx = rbac.WithPrincipal(ctx, p)
	var res any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tool panic: %v", r)
			}
		}()
		res, err = tool.Handler(ctx, args)
	}()
	if err != nil {
		s.Log.Warn("ai tool failed", "tool", tool.Name, "user", p.Username, "error", err)
		return toolMessage(callID, map[string]any{"error": err.Error()})
	}
	return toolMessage(callID, res)
}

// ExecuteConfirmed runs a confirmed pending call and returns the tool message to append.
func (s *Service) ExecuteConfirmed(ctx context.Context, p *rbac.Principal, id string) (*PendingCall, Message, error) {
	pc, err := s.Store.ClaimPendingCall(ctx, id, p.UserID, "executed", s.ConfirmTTL)
	if err != nil {
		return nil, Message{}, err
	}
	tool, ok := s.Tools.Get(pc.Tool)
	if !ok {
		_ = s.Store.FinishCall(ctx, pc.ID, "failed", nil, "tool no longer registered")
		return nil, Message{}, httpx.Conflict("tool no longer registered")
	}
	if !p.Can(tool.Permission) {
		_ = s.Store.FinishCall(ctx, pc.ID, "failed", nil, "permission denied")
		return nil, Message{}, httpx.Forbidden("permission denied: " + tool.Permission)
	}
	msg := s.runTool(ctx, p, tool, pc.ToolCallID, pc.Arguments)
	status, errMsg := "executed", ""
	var probe struct {
		Error string `json:"error"`
	}
	var str string
	if json.Unmarshal(msg.Content, &str) == nil && json.Unmarshal([]byte(str), &probe) == nil && probe.Error != "" {
		status, errMsg = "failed", probe.Error
	}
	resJSON := json.RawMessage(str)
	if !json.Valid(resJSON) {
		resJSON, _ = json.Marshal(str)
	}
	if err := s.Store.FinishCall(ctx, pc.ID, status, resJSON, errMsg); err != nil {
		return nil, Message{}, err
	}
	pc.Status, pc.Result, pc.Error = status, resJSON, errMsg
	return pc, msg, nil
}

// RejectConfirmed rejects a pending call and returns the tool message to append.
func (s *Service) RejectConfirmed(ctx context.Context, p *rbac.Principal, id string) (*PendingCall, Message, error) {
	pc, err := s.Store.ClaimPendingCall(ctx, id, p.UserID, "rejected", s.ConfirmTTL)
	if err != nil {
		return nil, Message{}, err
	}
	return pc, toolMessage(pc.ToolCallID, map[string]any{"error": "用户拒绝执行该操作"}), nil
}

func (s *Service) systemPrompt(ctx context.Context, req ChatRequest) (string, error) {
	var t *PromptTemplate
	var err error
	switch {
	case req.TemplateID > 0:
		t, err = s.Store.GetPrompt(ctx, req.TemplateID)
		if err == nil && !t.Enabled {
			return "", httpx.Validation("prompt template is disabled")
		}
	case req.Scene != "":
		t, err = s.Store.ScenePrompt(ctx, req.Scene)
	}
	if err != nil || t == nil {
		return "", err
	}
	return t.Render(req.Variables)
}

func validateMessages(ms []Message) error {
	if len(ms) == 0 {
		return httpx.Validation("messages must not be empty")
	}
	for i, m := range ms {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return httpx.Validation(fmt.Sprintf("messages[%d].role is invalid", i))
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return httpx.Validation(fmt.Sprintf("messages[%d]: tool messages need tool_call_id", i))
		}
		if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
			ms[i].Content = json.RawMessage(`""`)
		}
	}
	return nil
}

func toolMessage(callID string, v any) Message {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"error": "unserialisable tool result"})
	}
	if len(b) > maxToolResultBytes {
		b = append(b[:maxToolResultBytes:maxToolResultBytes], []byte("...(truncated)")...)
	}
	content, _ := json.Marshal(string(b))
	return Message{Role: "tool", ToolCallID: callID, Content: content}
}

func normaliseArgs(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{"_raw": s})
	return b
}

func newCallID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "tc_" + hex.EncodeToString(b)
}
