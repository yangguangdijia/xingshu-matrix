// Package httpx holds the HTTP conventions shared by every module: the unified
// JSON envelope {code,message,data}, error codes, request id, recover, CORS,
// access logging and pagination helpers.
package httpx

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Envelope is the unified JSON response body.
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id,omitempty"`
}

// Business error codes. 0 means success. The first three digits mirror the
// HTTP status so clients can branch on either.
const (
	CodeOK                     = 0
	CodeBadRequest             = 40000
	CodeValidation             = 40001
	CodeUnauthorized           = 40100
	CodeTokenExpired           = 40101
	CodeInvalidCredentials     = 40102
	CodeForbidden              = 40300
	CodePasswordChangeRequired = 40301
	CodeAccountLocked          = 40302
	CodeAccountDisabled        = 40303
	CodeApprovalRequired       = 40304
	CodeNotFound               = 40400
	CodeConflict               = 40900
	CodeTooManyRequests        = 42900
	CodeInternal               = 50000
	CodeUpstream               = 50200
	CodeUnavailable            = 50300
)

// Error is an error that maps to an HTTP status + business code.
type Error struct {
	Status  int    `json:"-"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.Message + ": " + e.cause.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// WithDetails returns a copy carrying details (e.g. field errors).
func (e *Error) WithDetails(d any) *Error { c := *e; c.Details = d; return &c }

// Wrap returns a copy carrying an internal cause (logged, never sent to the client).
func (e *Error) Wrap(err error) *Error { c := *e; c.cause = err; return &c }

// NewError builds an Error.
func NewError(status, code int, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// Convenience constructors.
func BadRequest(msg string) *Error   { return NewError(http.StatusBadRequest, CodeBadRequest, msg) }
func Validation(msg string) *Error   { return NewError(http.StatusBadRequest, CodeValidation, msg) }
func Unauthorized(msg string) *Error { return NewError(http.StatusUnauthorized, CodeUnauthorized, msg) }
func Forbidden(msg string) *Error    { return NewError(http.StatusForbidden, CodeForbidden, msg) }
func NotFound(msg string) *Error     { return NewError(http.StatusNotFound, CodeNotFound, msg) }
func Conflict(msg string) *Error     { return NewError(http.StatusConflict, CodeConflict, msg) }
func Internal(err error) *Error {
	return NewError(http.StatusInternalServerError, CodeInternal, "internal server error").Wrap(err)
}

// ErrNotFound is a sentinel repositories return for missing rows; Fail maps it to 404.
var ErrNotFound = errors.New("not found")

// OK writes a success envelope with HTTP 200.
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Code: CodeOK, Message: "ok", Data: data, RequestID: RequestIDFrom(c)})
}

// Created writes a success envelope with HTTP 201.
func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Code: CodeOK, Message: "created", Data: data, RequestID: RequestIDFrom(c)})
}

// Fail writes an error envelope and aborts the chain. Unknown errors become 500
// (the cause is attached to the gin context so the access logger records it).
func Fail(c *gin.Context, err error) {
	var e *Error
	switch {
	case errors.As(err, &e):
	case errors.Is(err, ErrNotFound):
		e = NotFound("resource not found")
	default:
		e = Internal(err)
	}
	if e.Status >= 500 || e.cause != nil {
		_ = c.Error(err)
	}
	body := Envelope{Code: e.Code, Message: e.Message, RequestID: RequestIDFrom(c)}
	if e.Details != nil {
		body.Data = gin.H{"details": e.Details}
	}
	c.AbortWithStatusJSON(e.Status, body)
}

// Bind decodes the JSON body into dst, translating binding errors to 400.
func Bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		Fail(c, Validation("invalid request body").WithDetails(err.Error()))
		return false
	}
	return true
}
