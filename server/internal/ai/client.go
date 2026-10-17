package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is an OpenAI chat message. Content is kept raw so multimodal arrays
// pass through untouched.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// TextMessage builds a message with string content.
func TextMessage(role, text string) Message {
	b, _ := json.Marshal(text)
	return Message{Role: role, Content: b}
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatRequest is the upstream request body.
type chatRequest struct {
	Model       string       `json:"model"`
	Messages    []Message    `json:"messages"`
	Stream      bool         `json:"stream"`
	Temperature *float32     `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Tools       []openAITool `json:"tools,omitempty"`
	ToolChoice  string       `json:"tool_choice,omitempty"`
}

// streamChunk is the subset of a chat.completion.chunk we interpret.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Endpoint resolves base_url to the chat completions URL. Accepts
// "https://api.x.com", "https://api.x.com/v1" or a full ".../chat/completions".
func Endpoint(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case strings.HasSuffix(u, "/chat/completions"):
		return u
	case strings.HasSuffix(u, "/v1") || strings.Contains(u, "/v1/") || hasVersionSuffix(u):
		return u + "/chat/completions"
	default:
		return u + "/v1/chat/completions"
	}
}

// hasVersionSuffix detects provider paths like /api/v3 or /compatible-mode/v1.
func hasVersionSuffix(u string) bool {
	i := strings.LastIndex(u, "/")
	if i < 0 || i+2 > len(u) {
		return false
	}
	seg := u[i+1:]
	if len(seg) < 2 || seg[0] != 'v' {
		return false
	}
	for _, r := range seg[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// UpstreamError is a non-2xx answer from the model provider.
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("model provider returned HTTP %d: %s", e.Status, e.Body)
}

// streamResult is the aggregate of one streamed completion.
type streamResult struct {
	Content      strings.Builder
	ToolCalls    []ToolCall
	FinishReason string
}

// streamChat posts req (stream=true) and calls onChunk with every raw data
// payload, returning the aggregated content and tool calls.
func streamChat(ctx context.Context, hc *http.Client, m *ModelConfig, apiKey string, req chatRequest, onChunk func(raw []byte) error) (*streamResult, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(m.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range m.ExtraHeaders {
		hreq.Header.Set(k, v)
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("call model provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &UpstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	res := &streamResult{}
	calls := map[int]*ToolCall{}
	var order []int

	// Non-streaming fallback: some gateways ignore stream=true and answer JSON.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		return parseNonStream(b, res, onChunk)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // tolerate provider keep-alive / comments
		}
		if onChunk != nil {
			if err := onChunk([]byte(data)); err != nil {
				return nil, err
			}
		}
		for _, c := range ch.Choices {
			if c.Index != 0 {
				continue
			}
			res.Content.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &ToolCall{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				res.FinishReason = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("read model stream: %w", err)
	}
	for i, idx := range order {
		tc := calls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", i)
		}
		res.ToolCalls = append(res.ToolCalls, *tc)
	}
	return res, ctx.Err()
}

func parseNonStream(b []byte, res *streamResult, onChunk func([]byte) error) (*streamResult, error) {
	var full struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &full); err != nil {
		return nil, fmt.Errorf("decode model response: %w", err)
	}
	if len(full.Choices) == 0 {
		return nil, errors.New("model response has no choices")
	}
	c := full.Choices[0]
	res.Content.WriteString(c.Message.Content)
	res.ToolCalls = c.Message.ToolCalls
	res.FinishReason = c.FinishReason
	if onChunk != nil && c.Message.Content != "" {
		// re-emit as a single OpenAI-style chunk so clients see one format
		chunk, _ := json.Marshal(map[string]any{
			"object":  "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": c.Message.Content}, "finish_reason": c.FinishReason}},
		})
		if err := onChunk(chunk); err != nil {
			return nil, err
		}
	}
	return res, nil
}
