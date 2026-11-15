package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// HandlerFunc processes one job. Return nil on success, an error to retry with
// backoff, or Permanent(err) to dead-letter immediately. The context carries
// the per-type timeout and is cancelled when the worker shuts down or loses
// the lease. Handlers must be idempotent: a job may run more than once
// (lease expiry after a crash, retry after a partial side effect).
type HandlerFunc func(ctx context.Context, job *Job) error

// HandlerOptions tune one job type. Zero values use the package defaults.
type HandlerOptions struct {
	// MaxAttempts is the default max_attempts for jobs enqueued without one.
	MaxAttempts int
	// Timeout bounds one attempt.
	Timeout time.Duration
	// BaseBackoff / MaxBackoff bound the exponential retry delay.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Jitter in [0,1] randomises the delay (default 0.2).
	Jitter float64
}

func (o HandlerOptions) withDefaults() HandlerOptions {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.MaxAttempts > MaxMaxAttempts {
		o.MaxAttempts = MaxMaxAttempts
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = DefaultBaseBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.Jitter < 0 {
		o.Jitter = 0
	}
	if o.Jitter == 0 {
		o.Jitter = 0.2
	}
	if o.Jitter > 1 {
		o.Jitter = 1
	}
	return o
}

type registration struct {
	fn   HandlerFunc
	opts HandlerOptions
}

// Registry maps job types to handlers. It is safe for concurrent use; modules
// register at startup before the worker starts.
type Registry struct {
	mu sync.RWMutex
	m  map[string]registration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]registration{}} }

// Register adds a handler for jobType. Registering the same type twice is an error.
func (r *Registry) Register(jobType string, fn HandlerFunc, opts HandlerOptions) error {
	if err := ValidateType(jobType); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("jobs: nil handler for %q", jobType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[jobType]; dup {
		return fmt.Errorf("jobs: handler for %q already registered", jobType)
	}
	r.m[jobType] = registration{fn: fn, opts: opts.withDefaults()}
	return nil
}

// MustRegister is Register that panics on error (startup wiring).
func (r *Registry) MustRegister(jobType string, fn HandlerFunc, opts HandlerOptions) {
	if err := r.Register(jobType, fn, opts); err != nil {
		panic(err)
	}
}

func (r *Registry) lookup(jobType string) (registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.m[jobType]
	return reg, ok
}

// Types returns the registered types, sorted.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for t := range r.m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Options returns the effective options for jobType (defaults if unregistered).
func (r *Registry) Options(jobType string) HandlerOptions {
	if reg, ok := r.lookup(jobType); ok {
		return reg.opts
	}
	return HandlerOptions{}.withDefaults()
}
