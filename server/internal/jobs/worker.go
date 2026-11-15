package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

// Worker claims and runs jobs of the registered types.
type Worker struct {
	Store    QueueStore
	Registry *Registry
	// ID identifies this replica in locked_by (default host-pid-random).
	ID string
	// Concurrency is the number of jobs run in parallel (default 4).
	Concurrency int
	// PollInterval is the idle wait between claim attempts (default 2s).
	PollInterval time.Duration
	// Lease is how long a claim stays valid without a heartbeat (default 1m);
	// heartbeats run every Lease/3.
	Lease time.Duration
	// ReapInterval is how often expired leases are recovered and the status
	// gauges refreshed (default 30s).
	ReapInterval time.Duration
	Log          *slog.Logger
	Metrics      *Metrics

	// rnd returns a value in [0,1) for backoff jitter (tests override it).
	rnd func() float64

	initOnce sync.Once
	wake     chan struct{}
	wg       sync.WaitGroup
}

func (w *Worker) init() {
	w.initOnce.Do(func() {
		if w.ID == "" {
			w.ID = defaultWorkerID()
		}
		if w.Concurrency <= 0 {
			w.Concurrency = 4
		}
		if w.PollInterval <= 0 {
			w.PollInterval = 2 * time.Second
		}
		if w.Lease <= 0 {
			w.Lease = DefaultLease
		}
		if w.Lease < 3*time.Second {
			w.Lease = 3 * time.Second
		}
		if w.ReapInterval <= 0 {
			w.ReapInterval = 30 * time.Second
		}
		if w.Log == nil {
			w.Log = slog.Default()
		}
		if w.Registry == nil {
			w.Registry = NewRegistry()
		}
		if w.rnd == nil {
			w.rnd = mrand.Float64
		}
		w.wake = make(chan struct{}, 1)
	})
}

func defaultWorkerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "matrix"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
	if len(id) > 120 {
		id = id[len(id)-120:]
	}
	return id
}

// Notify wakes the claim loop (call after enqueuing to skip the poll wait).
func (w *Worker) Notify() {
	w.init()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Start runs the claim loop and the reaper until ctx is done, then waits for
// in-flight handlers (their contexts are cancelled) to return.
func (w *Worker) Start(ctx context.Context) {
	w.init()
	types := w.Registry.Types()
	w.Log.Info("job worker started", "worker", w.ID, "types", types, "concurrency", w.Concurrency)

	w.wg.Add(1)
	go func() { defer w.wg.Done(); w.reapLoop(ctx) }()

	sem := make(chan struct{}, w.Concurrency)
	for ctx.Err() == nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		job, err := w.Store.Claim(ctx, w.ID, types, w.Lease)
		if err != nil || job == nil {
			<-sem
			if err != nil && ctx.Err() == nil {
				w.Log.Warn("job claim failed", "err", err)
			}
			w.idle(ctx)
			continue
		}
		w.wg.Add(1)
		go func(j *Job) {
			defer w.wg.Done()
			defer func() { <-sem }()
			w.run(ctx, j)
		}(job)
	}
	w.wg.Wait()
	w.Log.Info("job worker stopped", "worker", w.ID)
}

func (w *Worker) idle(ctx context.Context) {
	t := time.NewTimer(w.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-t.C:
	}
}

func (w *Worker) backoffFor(opts HandlerOptions) func(int) time.Duration {
	return func(attempt int) time.Duration {
		return Backoff(attempt, opts.BaseBackoff, opts.MaxBackoff, opts.Jitter, w.rnd())
	}
}

// shutdownMsg is recorded when a handler is interrupted by worker shutdown.
const shutdownMsg = "执行节点停止，任务已释放等待重新执行"

// run executes one claimed job and records its outcome.
func (w *Worker) run(ctx context.Context, job *Job) {
	w.init()
	start := time.Now()
	log := w.Log.With("job_id", job.ID, "job_type", job.Type, "attempt", job.Attempts)
	reg, ok := w.Registry.lookup(job.Type)
	opts := w.Registry.Options(job.Type)

	var runErr error
	leaseLost := false
	if !ok {
		runErr = Permanent(fmt.Errorf("%w: %s", ErrUnknownType, job.Type))
	} else {
		hctx, cancel := context.WithTimeout(ctx, opts.Timeout)
		hbDone := make(chan struct{})
		go func() {
			defer close(hbDone)
			t := time.NewTicker(w.Lease / 3)
			defer t.Stop()
			for {
				select {
				case <-hctx.Done():
					return
				case <-t.C:
					err := w.Store.Heartbeat(hctx, job.ID, w.ID, w.Lease)
					if errors.Is(err, ErrLeaseLost) {
						leaseLost = true // read only after <-hbDone
						cancel()
						return
					}
					if err != nil && hctx.Err() == nil {
						log.Warn("job heartbeat failed", "err", err)
					}
				}
			}
		}()
		runErr = safeCall(hctx, reg.fn, job)
		cancel()
		<-hbDone
	}
	elapsed := time.Since(start).Seconds()

	if leaseLost {
		// Another replica's reaper took the job; whatever we report is stale.
		log.Warn("job lease lost during execution; outcome discarded", "err", runErr)
		return
	}

	out := Decide(runErr, job.Attempts, job.MaxAttempts, w.backoffFor(opts))
	if runErr != nil && ctx.Err() != nil && !IsPermanent(runErr) {
		// Interrupted by shutdown: release for another replica without
		// dead-lettering it.
		out = Outcome{Status: StatusFailed, Error: shutdownMsg}
	}

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.Store.Finish(fctx, job.ID, w.ID, out); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			log.Warn("job lease lost before finish; outcome discarded", "status", out.Status)
		} else {
			log.Error("job finish failed", "err", err, "status", out.Status)
		}
		return
	}
	switch out.Status {
	case StatusSucceeded:
		w.Metrics.observe(job.Type, ResultSucceeded, elapsed)
		log.Debug("job succeeded", "seconds", elapsed)
	case StatusFailed:
		w.Metrics.observe(job.Type, ResultFailed, elapsed)
		log.Warn("job failed; will retry", "err", out.Error, "retry_in", out.Delay)
	case StatusDead:
		w.Metrics.observe(job.Type, ResultDead, elapsed)
		log.Error("job dead-lettered", "err", out.Error, "attempts", job.Attempts, "max_attempts", job.MaxAttempts)
	}
}

// safeCall runs fn and converts a panic into an error (retried like any other).
func safeCall(ctx context.Context, fn HandlerFunc, job *Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v\n%s", p, debug.Stack())
		}
	}()
	return fn(ctx, job)
}

func (w *Worker) reapLoop(ctx context.Context) {
	t := time.NewTicker(w.ReapInterval)
	defer t.Stop()
	for {
		w.reapOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Worker) reapOnce(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reaped, err := w.Store.ReapExpired(rctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("job reaper failed", "err", err)
		}
	}
	for _, r := range reaped {
		w.Metrics.observe(r.Type, ResultReaped, -1)
		if r.Status == StatusDead {
			w.Metrics.observe(r.Type, ResultDead, -1)
		}
		w.Log.Warn("job lease expired; recovered", "job_id", r.ID, "job_type", r.Type, "status", r.Status)
	}
	if len(reaped) > 0 {
		w.Notify()
	}
	if w.Metrics == nil {
		return
	}
	counts, err := w.Store.CountByStatus(rctx)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("job status count failed", "err", err)
		}
		return
	}
	w.Metrics.setCounts(counts)
}
