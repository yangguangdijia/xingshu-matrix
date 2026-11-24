package mcp

import (
	"bytes"
	"encoding/json"
)

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is a JSON-RPC 2.0 request or notification. ID is absent (len 0) for
// notifications.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request carries no id.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// NewError builds an *Error.
func NewError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

// Response is a JSON-RPC 2.0 response. Exactly one of Result / Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

var nullID = json.RawMessage("null")

func resultResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, e *Error) *Response {
	if len(id) == 0 {
		id = nullID
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: e}
}

// validID reports whether raw is a JSON string or number (null is rejected:
// MCP forbids null ids).
func validID(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// parseMessage decodes one batch element / single message. On failure it
// returns the id it could recover (may be nil) and an Invalid Request error.
func parseMessage(raw json.RawMessage) (*Request, json.RawMessage, *Error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil, NewError(CodeInvalidRequest, "invalid request: not an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, NewError(CodeInvalidRequest, "invalid request")
	}
	var id json.RawMessage
	if v, ok := fields["id"]; ok {
		if !validID(v) {
			return nil, nil, NewError(CodeInvalidRequest, "invalid request: id must be a string or number")
		}
		id = v
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, id, NewError(CodeInvalidRequest, "invalid request")
	}
	req.ID = id
	if req.JSONRPC != "2.0" {
		return nil, id, NewError(CodeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	if req.Method == "" {
		if _, isResp := fields["result"]; isResp {
			return nil, nil, nil // a client response: accepted, ignored
		}
		if _, isResp := fields["error"]; isResp {
			return nil, nil, nil
		}
		return nil, id, NewError(CodeInvalidRequest, "invalid request: method is required")
	}
	if len(req.Params) > 0 {
		p := bytes.TrimSpace(req.Params)
		if len(p) == 0 || (p[0] != '{' && p[0] != '[') {
			return nil, id, NewError(CodeInvalidRequest, "invalid request: params must be an object or array")
		}
	}
	return &req, id, nil
}

// splitBatch reports whether body is a batch and returns its elements.
func splitBatch(body []byte) (batch bool, msgs []json.RawMessage, perr *Error) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return false, nil, NewError(CodeParseError, "parse error: empty body")
	}
	if !json.Valid(b) {
		return false, nil, NewError(CodeParseError, "parse error")
	}
	if b[0] == '[' {
		if err := json.Unmarshal(b, &msgs); err != nil {
			return true, nil, NewError(CodeParseError, "parse error")
		}
		return true, msgs, nil
	}
	return false, []json.RawMessage{b}, nil
}
