// Package mcp exposes the AI tool registry as a Model Context Protocol server
// (Streamable HTTP transport, JSON-RPC 2.0, protocol version 2025-06-18).
//
// The endpoint is stateless and POST-only: every request is authenticated with
// an API token (Authorization: Bearer), cookies are never an identity, and
// tools are filtered and checked with the same rbac permissions as the in-app
// AI chat. Tools that require confirmation are never executed here; the caller
// is told to confirm inside the platform.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

// supportedVersions are accepted in initialize / MCP-Protocol-Version.
var supportedVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true}

// MaxBodyBytes caps a request body (1 MiB).
const MaxBodyBytes = 1 << 20

// AuthFunc validates an API token and returns the calling principal. The
// returned principal must be token-authenticated (Principal.ViaToken).
type AuthFunc func(ctx context.Context, token, ip string) (*rbac.Principal, error)

// PendingFunc records a tool call that needs confirmation in the platform and
// returns the path where the user confirms it (e.g.
// /api/v1/ai/tool-calls/<id>/confirm). Wire it to ai.Store.InsertPendingCall.
type PendingFunc func(ctx context.Context, p *rbac.Principal, tool string, args json.RawMessage) (confirmPath string, err error)

// AuditRecord describes one tools/call.
type AuditRecord struct {
	UserID    int64
	Username  string
	TokenID   int64
	IP        string
	UserAgent string
	Tool      string
	// Args is a redacted, truncated summary of the arguments.
	Args string
	// Outcome is one of ok, error, forbidden, unknown_tool, invalid_params,
	// pending_confirmation.
	Outcome  string
	Error    string
	Duration time.Duration
}

// AuditFunc receives one record per tools/call.
type AuditFunc func(ctx context.Context, rec AuditRecord)

// Options tunes a Server; zero values use defaults.
type Options struct {
	Version string // serverInfo.version
	// RatePerSecond / Burst define the per-user token bucket (default 5/s, 20).
	RatePerSecond float64
	Burst         int
	// AllowedOrigins are extra origins accepted besides same-origin.
	AllowedOrigins []string
	Pending        PendingFunc
	Audit          AuditFunc
	// ConfirmPath is the fallback confirm path when Pending is nil.
	ConfirmPath string
	Now         func() time.Time
}

// Server is an http.Handler serving MCP over Streamable HTTP (POST only).
type Server struct {
	registry *ai.ToolRegistry
	auth     AuthFunc
	opts     Options
	limiter  *limiter
}

// New returns a Server with default options.
func New(registry *ai.ToolRegistry, authFn AuthFunc) *Server {
	return NewWithOptions(registry, authFn, Options{})
}

// NewWithOptions returns a Server.
func NewWithOptions(registry *ai.ToolRegistry, authFn AuthFunc, o Options) *Server {
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.RatePerSecond <= 0 {
		o.RatePerSecond = 5
	}
	if o.Burst <= 0 {
		o.Burst = 20
	}
	if o.ConfirmPath == "" {
		o.ConfirmPath = "/ai"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Server{registry: registry, auth: authFn, opts: o,
		limiter: newLimiter(o.RatePerSecond, o.Burst, o.Now)}
}

// SetPending / SetAudit install hooks after construction.
func (s *Server) SetPending(f PendingFunc) { s.opts.Pending = f }

// SetAudit installs the audit hook.
func (s *Server) SetAudit(f AuditFunc) { s.opts.Audit = f }

// ---------------------------------------------------------------- HTTP

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, errorResponse(nil, NewError(code, msg)))
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpError(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed: this MCP endpoint accepts POST only")
		return
	}
	if !s.originAllowed(r) {
		httpError(w, http.StatusForbidden, CodeInvalidRequest, "origin not allowed")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedVersions[v] {
		httpError(w, http.StatusBadRequest, CodeInvalidRequest, "unsupported MCP-Protocol-Version "+v)
		return
	}
	p, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if !s.limiter.allow(p.UserID) {
		w.Header().Set("Retry-After", "1")
		httpError(w, http.StatusTooManyRequests, CodeInvalidRequest, "rate limit exceeded")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		httpError(w, http.StatusUnsupportedMediaType, CodeInvalidRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpError(w, http.StatusRequestEntityTooLarge, CodeInvalidRequest, "request body exceeds 1 MiB")
			return
		}
		httpError(w, http.StatusBadRequest, CodeParseError, "read body: "+err.Error())
		return
	}
	batch, msgs, perr := splitBatch(body)
	if perr != nil {
		writeJSON(w, http.StatusOK, errorResponse(nil, perr))
		return
	}
	if batch && len(msgs) == 0 {
		writeJSON(w, http.StatusOK, errorResponse(nil, NewError(CodeInvalidRequest, "invalid request: empty batch")))
		return
	}
	ctx := rbac.WithPrincipal(r.Context(), p)
	cc := callCtx{p: p, ip: clientIP(r), ua: r.UserAgent()}
	var out []*Response
	for _, m := range msgs {
		if resp := s.handleMessage(ctx, cc, m); resp != nil {
			out = append(out, resp)
		}
	}
	switch {
	case len(out) == 0:
		w.WriteHeader(http.StatusAccepted) // only notifications / responses
	case batch:
		writeJSON(w, http.StatusOK, out)
	default:
		writeJSON(w, http.StatusOK, out[0])
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*rbac.Principal, bool) {
	fail := func(msg string) (*rbac.Principal, bool) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		httpError(w, http.StatusUnauthorized, CodeInvalidRequest, msg)
		return nil, false
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		if r.Header.Get("Cookie") != "" {
			return fail("MCP 端点不接受 Cookie 登录态，请使用 Authorization: Bearer <API Token>")
		}
		return fail("authentication required: Authorization: Bearer <API Token>")
	}
	scheme, tok, ok := strings.Cut(h, " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return fail("Authorization header must be Bearer <API Token>")
	}
	if s.auth == nil {
		return fail("authentication unavailable")
	}
	p, err := s.auth(r.Context(), tok, clientIP(r))
	if err != nil || p == nil {
		return fail("invalid API token")
	}
	if !p.ViaToken() {
		return fail("MCP 端点仅接受 API Token，不接受登录会话令牌")
	}
	return p, true
}

// originAllowed enforces same-origin when an Origin header is present (DNS
// rebinding protection).
func (s *Server) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	for _, a := range s.opts.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(o, "/")) {
			return true
		}
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return strings.EqualFold(u.Host, host)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------- dispatch

type callCtx struct {
	p      *rbac.Principal
	ip, ua string
}

func (s *Server) handleMessage(ctx context.Context, cc callCtx, raw json.RawMessage) *Response {
	req, id, rerr := parseMessage(raw)
	if rerr != nil {
		return errorResponse(id, rerr)
	}
	if req == nil { // client response; nothing to answer
		return nil
	}
	result, err := s.dispatch(ctx, cc, req)
	if req.IsNotification() {
		return nil
	}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = NewError(CodeInternalError, "internal error")
		}
		return errorResponse(req.ID, e)
	}
	return resultResponse(req.ID, result)
}

func (s *Server) dispatch(ctx context.Context, cc callCtx, req *Request) (any, error) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return s.toolsList(cc.p), nil
	case "tools/call":
		return s.toolsCall(ctx, cc, req.Params)
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, NewError(CodeMethodNotFound, "method not found: "+req.Method)
}

func decodeParams(raw json.RawMessage, v any) *Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return NewError(CodeInvalidParams, "invalid params: "+err.Error())
	}
	return nil
}

func (s *Server) initialize(params json.RawMessage) (any, error) {
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if e := decodeParams(params, &in); e != nil {
		return nil, e
	}
	if in.ProtocolVersion == "" {
		return nil, NewError(CodeInvalidParams, "invalid params: protocolVersion is required")
	}
	v := ProtocolVersion
	if supportedVersions[in.ProtocolVersion] {
		v = in.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "xingshu-matrix", "title": "星枢 Matrix", "version": s.opts.Version},
		"instructions":    "星枢 Matrix 平台工具。会改变状态的工具需要在平台内确认后才会执行。",
	}, nil
}

type toolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations toolAnnotations `json:"annotations"`
}

func (s *Server) toolsList(p *rbac.Principal) any {
	tools := s.registry.Available(p, nil)
	out := make([]mcpTool, 0, len(tools))
	for _, t := range tools {
		desc := t.Description
		if t.RequiresConfirmation {
			desc += "（该操作需在星枢 Matrix 平台内确认后才会执行）"
		}
		out = append(out, mcpTool{
			Name: t.Name, Description: desc, InputSchema: t.Parameters,
			// Registry convention: every state-changing tool requires
			// confirmation, so that flag is the read-only/destructive signal.
			Annotations: toolAnnotations{ReadOnlyHint: !t.RequiresConfirmation, DestructiveHint: t.RequiresConfirmation},
		})
	}
	return map[string]any{"tools": out}
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

func textResult(isErr bool, text string) *callResult {
	return &callResult{Content: []textContent{{Type: "text", Text: text}}, IsError: isErr}
}

func (s *Server) toolsCall(ctx context.Context, cc callCtx, params json.RawMessage) (any, error) {
	start := s.opts.Now()
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 {
		return nil, NewError(CodeInvalidParams, "invalid params: name is required")
	}
	if e := decodeParams(params, &in); e != nil {
		return nil, e
	}
	rec := AuditRecord{UserID: cc.p.UserID, Username: cc.p.Username, TokenID: cc.p.TokenID,
		IP: cc.ip, UserAgent: cc.ua, Tool: in.Name, Args: summarizeArgs(in.Arguments)}
	done := func(outcome, errMsg string) {
		rec.Outcome, rec.Error, rec.Duration = outcome, errMsg, s.opts.Now().Sub(start)
		if s.opts.Audit != nil {
			s.opts.Audit(ctx, rec)
		}
	}
	if in.Name == "" {
		done("invalid_params", "missing name")
		return nil, NewError(CodeInvalidParams, "invalid params: name is required")
	}
	args := in.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if len(args) == 0 || args[0] != '{' {
		done("invalid_params", "arguments not an object")
		return nil, NewError(CodeInvalidParams, "invalid params: arguments must be an object")
	}
	t, ok := s.registry.Get(in.Name)
	if !ok {
		done("unknown_tool", "")
		return nil, NewError(CodeInvalidParams, "unknown tool: "+in.Name)
	}
	if !cc.p.Can(t.Permission) {
		done("forbidden", "missing permission "+t.Permission)
		return textResult(true, "权限不足：调用该工具需要权限 "+t.Permission), nil
	}
	if t.RequiresConfirmation {
		path := s.opts.ConfirmPath
		if s.opts.Pending != nil {
			p, err := s.opts.Pending(ctx, cc.p, t.Name, args)
			if err != nil {
				done("error", "pending: "+err.Error())
				return nil, NewError(CodeInternalError, "failed to create pending confirmation")
			}
			path = p
		}
		done("pending_confirmation", "")
		res := textResult(false, fmt.Sprintf("该操作需在平台内确认：工具 %s 未执行。请登录星枢 Matrix 在以下位置确认后执行：%s", t.Name, path))
		res.StructuredContent = map[string]any{"status": "pending_confirmation", "tool": t.Name, "confirm_path": path}
		return res, nil
	}
	v, err := runHandler(ctx, t, args)
	if err != nil {
		done("error", truncate(err.Error(), 256))
		return textResult(true, "工具执行失败："+err.Error()), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		done("error", "encode result")
		return nil, NewError(CodeInternalError, "failed to encode tool result")
	}
	done("ok", "")
	return textResult(false, string(b)), nil
}

func runHandler(ctx context.Context, t ai.Tool, args json.RawMessage) (v any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("tool %s panicked", t.Name)
		}
	}()
	return t.Handler(ctx, args)
}

// ---------------------------------------------------------------- redaction

var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|private|credential|auth|cookie|session|kubeconfig|cert|signature)`)

const maxArgSummary = 512

// summarizeArgs returns a redacted JSON summary of tool arguments: sensitive
// keys become "***", long strings are cut, the whole is capped.
func summarizeArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<invalid json>"
	}
	b, _ := json.Marshal(redact(v, 0))
	return truncate(string(b), maxArgSummary)
}

func redact(v any, depth int) any {
	if depth > 8 {
		return "…"
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if sensitiveKey.MatchString(k) {
				out[k] = "***"
				continue
			}
			out[k] = redact(val, depth+1)
		}
		return out
	case []any:
		n := len(x)
		if n > 20 {
			x = x[:20]
		}
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, redact(e, depth+1))
		}
		if n > 20 {
			out = append(out, fmt.Sprintf("…(+%d)", n-20))
		}
		return out
	case string:
		return truncate(x, 64)
	}
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------- rate limit

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[int64]*bucket
}

func newLimiter(rate float64, burst int, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: float64(burst), now: now, buckets: map[int64]*bucket{}}
}

func (l *limiter) allow(user int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[user]
	if !ok {
		if len(l.buckets) > 10000 { // drop idle buckets
			for k, ob := range l.buckets {
				if now.Sub(ob.last) > time.Duration(l.burst/l.rate*float64(time.Second)) {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[user] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
