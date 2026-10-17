// Package ai provides OpenAI-compatible model configuration, scene prompt
// templates, a streaming chat proxy (SSE) and a tool-calling registry that
// other modules register tools into.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ToolHandler executes a tool. ctx carries the calling principal
// (rbac.FromContext). args is the raw JSON produced by the model; validate it.
// The returned value is JSON-encoded and sent back to the model.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool is a function the model may call.
type Tool struct {
	// Name must match ^[a-zA-Z0-9_-]{1,64}$ (OpenAI constraint); prefix it with
	// the module, e.g. "k8s_get_pods".
	Name        string `json:"name"`
	Module      string `json:"module"`
	Description string `json:"description"`
	// Parameters is a JSON Schema object describing the arguments.
	Parameters json.RawMessage `json:"parameters"`
	// Permission the caller must hold for the tool to be offered and executed.
	Permission string `json:"permission"`
	// RequiresConfirmation makes the chat pause and ask the user to confirm
	// (POST /api/v1/ai/tool-calls/:id/confirm) before Handler runs. Use it for
	// every tool that changes state. Production changes must additionally go
	// through approval.Service.Gate inside the handler.
	RequiresConfirmation bool        `json:"requires_confirmation"`
	Handler              ToolHandler `json:"-"`
}

var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ToolRegistry is a concurrency-safe set of tools.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry returns an empty registry.
func NewToolRegistry() *ToolRegistry { return &ToolRegistry{tools: map[string]Tool{}} }

// Register adds a tool.
func (r *ToolRegistry) Register(t Tool) error {
	if !toolNameRe.MatchString(t.Name) {
		return fmt.Errorf("ai: invalid tool name %q", t.Name)
	}
	if t.Handler == nil {
		return fmt.Errorf("ai: tool %s has no handler", t.Name)
	}
	if t.Permission == "" {
		return fmt.Errorf("ai: tool %s must declare a permission", t.Name)
	}
	if len(t.Parameters) == 0 {
		t.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if !json.Valid(t.Parameters) {
		return fmt.Errorf("ai: tool %s parameters is not valid JSON", t.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("ai: tool %s already registered", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// MustRegister panics on error.
func (r *ToolRegistry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns all tools sorted by name.
func (r *ToolRegistry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Available returns the tools p may use, optionally restricted to names.
func (r *ToolRegistry) Available(p *rbac.Principal, names []string) []Tool {
	allow := map[string]bool{}
	for _, n := range names {
		allow[n] = true
	}
	var out []Tool
	for _, t := range r.List() {
		if len(allow) > 0 && !allow[t.Name] {
			continue
		}
		if p.Can(t.Permission) {
			out = append(out, t)
		}
	}
	return out
}

// openAITool is the wire format of a tool definition.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func toOpenAITools(ts []Tool) []openAITool {
	out := make([]openAITool, 0, len(ts))
	for _, t := range ts {
		var o openAITool
		o.Type = "function"
		o.Function.Name = t.Name
		o.Function.Description = t.Description
		if t.RequiresConfirmation {
			o.Function.Description += "（该操作需要用户确认后才会执行）"
		}
		o.Function.Parameters = t.Parameters
		out = append(out, o)
	}
	return out
}
