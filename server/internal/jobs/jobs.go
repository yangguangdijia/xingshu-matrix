// Package jobs is the platform background job queue backed by PostgreSQL
// (table platform_jobs, core migration 0012_jobs).
//
// Workers claim runnable rows with FOR UPDATE SKIP LOCKED, hold a lease
// (locked_by + locked_until) that a heartbeat extends while the handler runs,
// and record the outcome only while they still own the lease. Failed attempts
// are retried with exponential backoff until max_attempts; then the job is
// dead-lettered (status dead). A reaper turns running rows whose lease expired
// (crashed replica) back into failed (or dead when attempts are exhausted).
//
// State machine:
//
//	queued  --claim-->            running
//	failed  --claim (run_at)-->   running
//	running --ok-->               succeeded
//	running --error, attempts<max-> failed (run_at = now + backoff)
//	running --error, exhausted / Permanent--> dead
//	running --lease expired-->    failed | dead
//	failed | dead --manual retry--> queued
//	succeeded | dead --delete-->  (removed)
package jobs

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

// Status values of platform_jobs.status.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusDead      = "dead"
)

// AllStatuses lists every status in display order.
var AllStatuses = []string{StatusQueued, StatusRunning, StatusFailed, StatusDead, StatusSucceeded}

// ValidStatus reports whether s is a known status.
func ValidStatus(s string) bool {
	for _, v := range AllStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// Defaults.
const (
	DefaultMaxAttempts = 5
	MaxMaxAttempts     = 100
	DefaultBaseBackoff = 10 * time.Second
	DefaultMaxBackoff  = time.Hour
	DefaultTimeout     = 10 * time.Minute
	DefaultLease       = time.Minute
	maxErrorLen        = 4000
	maxTypeLen         = 128
)

// Job is one row of platform_jobs.
type Job struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Priority    int             `json:"priority"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	RunAt       time.Time       `json:"run_at"`
	LockedBy    string          `json:"locked_by"`
	LockedUntil *time.Time      `json:"locked_until"`
	LastError   string          `json:"last_error"`
	DedupeKey   *string         `json:"dedupe_key"`
	CreatedBy   *int64          `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	StartedAt   *time.Time      `json:"started_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
}

// Errors returned by the store / state machine.
var (
	ErrNotFound      = errors.New("job not found")
	ErrInvalidState  = errors.New("job is not in a state that allows this operation")
	ErrDuplicate     = errors.New("an unfinished job with the same dedupe key exists")
	ErrLeaseLost     = errors.New("job lease lost")
	ErrUnknownType   = errors.New("no handler registered for job type")
	errEmptyType     = errors.New("job type is required")
	errTypeTooLong   = errors.New("job type too long")
	errBadMaxAttempt = errors.New("max_attempts out of range")
)

// permanentError marks an error that must not be retried.
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// Permanent wraps err so the worker dead-letters the job without retrying
// (bad payload, target deleted, ...). Permanent(nil) returns nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err (or anything it wraps) was marked Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Backoff returns the delay before retry number `attempt` (1 = the delay after
// the first failed attempt): base * 2^(attempt-1), capped at max. jitter in
// [0,1] spreads the delay to (1-jitter/2 .. 1+jitter/2) * d using r in [0,1);
// the result never exceeds max and is never below base/2 (when base>0).
func Backoff(attempt int, base, max time.Duration, jitter, r float64) time.Duration {
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	if max <= 0 {
		max = DefaultMaxBackoff
	}
	if max < base {
		max = base
	}
	if attempt < 1 {
		attempt = 1
	}
	// 2^62 overflows Duration; clamp the exponent long before that.
	exp := attempt - 1
	if exp > 40 {
		exp = 40
	}
	d := float64(base) * math.Pow(2, float64(exp))
	if d > float64(max) {
		d = float64(max)
	}
	if jitter > 0 {
		if jitter > 1 {
			jitter = 1
		}
		if r < 0 {
			r = 0
		}
		if r >= 1 {
			r = 0.999999
		}
		d = d * (1 - jitter/2 + jitter*r)
	}
	if d > float64(max) {
		d = float64(max)
	}
	if min := float64(base) / 2; d < min {
		d = min
	}
	return time.Duration(d)
}

// Outcome is the transition decided after an attempt finished.
type Outcome struct {
	Status string
	Delay  time.Duration // failed: delay before the next attempt (run_at = now + Delay)
	Error  string
}

// Decide computes the state after an attempt. attempts already includes the
// attempt that just ran (claim increments it).
func Decide(runErr error, attempts, maxAttempts int, backoff func(attempt int) time.Duration) Outcome {
	if runErr == nil {
		return Outcome{Status: StatusSucceeded}
	}
	msg := TruncateError(runErr.Error())
	if IsPermanent(runErr) || attempts >= maxAttempts {
		return Outcome{Status: StatusDead, Error: msg}
	}
	return Outcome{Status: StatusFailed, Delay: backoff(attempts), Error: msg}
}

// CanRetry reports whether a manual retry is allowed from status.
func CanRetry(status string) bool { return status == StatusFailed || status == StatusDead }

// CanDelete reports whether a job in status may be deleted.
func CanDelete(status string) bool { return status == StatusDead || status == StatusSucceeded }

// TruncateError bounds an error message stored in last_error.
func TruncateError(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= maxErrorLen {
		return s
	}
	cut := maxErrorLen
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ValidateType checks a job type name.
func ValidateType(t string) error {
	switch {
	case strings.TrimSpace(t) == "":
		return errEmptyType
	case len(t) > maxTypeLen:
		return errTypeTooLong
	}
	return nil
}

// sensitiveKeyParts mark payload keys whose values are masked in API output.
var sensitiveKeyParts = []string{"password", "passwd", "secret", "token", "private_key", "privatekey", "access_key", "accesskey", "api_key", "apikey", "credential", "kubeconfig"}

func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, p := range sensitiveKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// RedactPayload masks values of sensitive keys at any depth. Enqueuers must
// not put secrets in payloads; this is a second line of defence for the API.
func RedactPayload(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`"***"`)
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return json.RawMessage(`"***"`)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKey(k) {
				t[k] = "***"
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
		return t
	default:
		return v
	}
}
