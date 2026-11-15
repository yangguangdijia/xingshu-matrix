package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// fakeStore is an in-memory QueueStore + AdminStore implementing the same
// state machine as PgStore's SQL.
type fakeStore struct {
	mu        sync.Mutex
	jobs      map[int64]*Job
	next      int64
	lostLease map[int64]bool // Heartbeat returns ErrLeaseLost for these
	finishes  int
}

func newFake() *fakeStore {
	return &fakeStore{jobs: map[int64]*Job{}, lostLease: map[int64]bool{}}
}

func (f *fakeStore) add(typ string, maxAttempts int) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.jobs[f.next] = &Job{ID: f.next, Type: typ, Status: StatusQueued, MaxAttempts: maxAttempts, RunAt: time.Now(), Payload: []byte(`{"password":"x","n":1}`)}
	return f.next
}

func (f *fakeStore) get(id int64) Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.jobs[id]
}

func (f *fakeStore) Claim(_ context.Context, worker string, types []string, lease time.Duration) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *Job
	for _, j := range f.jobs {
		if (j.Status != StatusQueued && j.Status != StatusFailed) || j.RunAt.After(time.Now()) {
			continue
		}
		ok := false
		for _, t := range types {
			ok = ok || t == j.Type
		}
		if ok && (best == nil || j.ID < best.ID) {
			best = j
		}
	}
	if best == nil {
		return nil, nil
	}
	until := time.Now().Add(lease)
	best.Status, best.Attempts, best.LockedBy, best.LockedUntil = StatusRunning, best.Attempts+1, worker, &until
	cp := *best
	return &cp, nil
}

func (f *fakeStore) Heartbeat(_ context.Context, id int64, worker string, lease time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	if f.lostLease[id] || j == nil || j.Status != StatusRunning || j.LockedBy != worker {
		return ErrLeaseLost
	}
	until := time.Now().Add(lease)
	j.LockedUntil = &until
	return nil
}

func (f *fakeStore) Finish(_ context.Context, id int64, worker string, o Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes++
	j := f.jobs[id]
	if j == nil || j.Status != StatusRunning || j.LockedBy != worker {
		return ErrLeaseLost
	}
	j.Status, j.LockedBy, j.LockedUntil = o.Status, "", nil
	if o.Status != StatusSucceeded {
		j.LastError = o.Error
	}
	if o.Status == StatusFailed {
		j.RunAt = time.Now().Add(o.Delay)
	}
	return nil
}

func (f *fakeStore) ReapExpired(_ context.Context, _ int) ([]Reaped, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Reaped
	for _, j := range f.jobs {
		if j.Status == StatusRunning && j.LockedUntil != nil && j.LockedUntil.Before(time.Now()) {
			j.Status = StatusFailed
			if j.Attempts >= j.MaxAttempts {
				j.Status = StatusDead
			}
			j.LockedBy, j.LockedUntil, j.RunAt, j.LastError = "", nil, time.Now(), leaseExpiredMsg
			out = append(out, Reaped{ID: j.ID, Type: j.Type, Status: j.Status})
		}
	}
	return out, nil
}

func (f *fakeStore) CountByStatus(context.Context) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for _, s := range AllStatuses {
		out[s] = 0
	}
	for _, j := range f.jobs {
		out[j.Status]++
	}
	return out, nil
}

func (f *fakeStore) List(_ context.Context, lf ListFilter, _, _ int) ([]Job, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Job
	for _, j := range f.jobs {
		out = append(out, *j)
	}
	return out, int64(len(out)), nil
}

func (f *fakeStore) Get(_ context.Context, id int64) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *j
	return &cp, nil
}

func (f *fakeStore) Retry(_ context.Context, id int64) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !CanRetry(j.Status) {
		return nil, ErrInvalidState
	}
	if j.Status == StatusDead {
		j.Attempts = 0
	}
	j.Status, j.RunAt = StatusQueued, time.Now()
	cp := *j
	return &cp, nil
}

func (f *fakeStore) Delete(_ context.Context, id int64) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !CanDelete(j.Status) {
		return nil, ErrInvalidState
	}
	delete(f.jobs, id)
	return j, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestWorker(fs *fakeStore, reg *Registry, m *Metrics) *Worker {
	w := &Worker{Store: fs, Registry: reg, ID: "w1", Log: quietLog(), Metrics: m,
		PollInterval: 5 * time.Millisecond, ReapInterval: 20 * time.Millisecond, Lease: 3 * time.Second}
	w.rnd = func() float64 { return 0.5 }
	return w
}

func claimAndRun(t *testing.T, w *Worker, fs *fakeStore) {
	t.Helper()
	w.init()
	j, err := fs.Claim(context.Background(), w.ID, w.Registry.Types(), w.Lease)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	w.run(context.Background(), j)
}

func TestWorkerRetryThenDead(t *testing.T) {
	fs := newFake()
	reg := NewRegistry()
	calls := 0
	reg.MustRegister("t", func(context.Context, *Job) error { calls++; return errors.New("down") },
		HandlerOptions{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	promReg := prometheus.NewRegistry()
	m := NewMetrics(promReg)
	w := newTestWorker(fs, reg, m)
	id := fs.add("t", 3)

	claimAndRun(t, w, fs)
	if j := fs.get(id); j.Status != StatusFailed || j.Attempts != 1 || j.LastError != "down" {
		t.Fatalf("after 1: %+v", j)
	}
	time.Sleep(3 * time.Millisecond)
	claimAndRun(t, w, fs)
	time.Sleep(3 * time.Millisecond)
	claimAndRun(t, w, fs)
	if j := fs.get(id); j.Status != StatusDead || j.Attempts != 3 {
		t.Fatalf("after 3: %+v", j)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
	if got := metricValue(t, m.processed.WithLabelValues("t", ResultFailed)); got != 2 {
		t.Fatalf("failed counter %v", got)
	}
	if got := metricValue(t, m.processed.WithLabelValues("t", ResultDead)); got != 1 {
		t.Fatalf("dead counter %v", got)
	}
	// metrics registration is idempotent on the same registry
	if m2 := NewMetrics(promReg); metricValue(t, m2.processed.WithLabelValues("t", ResultDead)) != 1 {
		t.Fatal("re-registration did not reuse collectors")
	}
}

func TestWorkerPermanentAndPanic(t *testing.T) {
	fs := newFake()
	reg := NewRegistry()
	reg.MustRegister("perm", func(context.Context, *Job) error { return Permanent(errors.New("bad input")) }, HandlerOptions{})
	reg.MustRegister("panic", func(context.Context, *Job) error { panic("kaboom") }, HandlerOptions{BaseBackoff: time.Hour})
	w := newTestWorker(fs, reg, nil)
	a := fs.add("perm", 5)
	claimAndRun(t, w, fs)
	if j := fs.get(a); j.Status != StatusDead || j.Attempts != 1 {
		t.Fatalf("permanent: %+v", j)
	}
	b := fs.add("panic", 5)
	claimAndRun(t, w, fs)
	if j := fs.get(b); j.Status != StatusFailed || !strings.Contains(j.LastError, "kaboom") || !j.RunAt.After(time.Now().Add(20*time.Minute)) {
		t.Fatalf("panic: %+v", j)
	}
}

func TestWorkerLeaseLostDiscardsOutcome(t *testing.T) {
	fs := newFake()
	reg := NewRegistry()
	sawCancel := make(chan bool, 1)
	reg.MustRegister("slow", func(ctx context.Context, _ *Job) error {
		select {
		case <-ctx.Done():
			sawCancel <- true
			return ctx.Err()
		case <-time.After(5 * time.Second):
			sawCancel <- false
			return nil
		}
	}, HandlerOptions{})
	w := newTestWorker(fs, reg, nil)
	id := fs.add("slow", 5)
	fs.lostLease[id] = true
	claimAndRun(t, w, fs)
	if !<-sawCancel {
		t.Fatal("handler was not cancelled on lease loss")
	}
	if fs.finishes != 0 {
		t.Fatal("outcome must not be written after lease loss")
	}
}

func TestWorkerStartEndToEndAndReaper(t *testing.T) {
	fs := newFake()
	reg := NewRegistry()
	var mu sync.Mutex
	n := 0
	reg.MustRegister("ok", func(context.Context, *Job) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return errors.New("first fails")
		}
		return nil
	}, HandlerOptions{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	m := NewMetrics(nil)
	w := newTestWorker(fs, reg, m)
	id := fs.add("ok", 5)
	other := fs.add("unregistered", 5)
	// a running job whose lease already expired (crashed replica)
	stale := fs.add("ok", 1)
	past := time.Now().Add(-time.Minute)
	fs.jobs[stale].Status, fs.jobs[stale].Attempts, fs.jobs[stale].LockedBy, fs.jobs[stale].LockedUntil = StatusRunning, 1, "gone", &past

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Start(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fs.get(id).Status == StatusSucceeded && fs.get(stale).Status == StatusDead {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // let the reaper refresh the gauges
	cancel()
	<-done
	if j := fs.get(id); j.Status != StatusSucceeded || j.Attempts != 2 {
		t.Fatalf("job: %+v", j)
	}
	if j := fs.get(stale); j.Status != StatusDead || j.LastError != leaseExpiredMsg {
		t.Fatalf("stale: %+v", j)
	}
	if j := fs.get(other); j.Status != StatusQueued {
		t.Fatalf("unregistered type must not be claimed: %+v", j)
	}
	if got := metricValue(t, m.byStatus.WithLabelValues(StatusDead)); got != 1 {
		t.Fatalf("dead gauge %v", got)
	}
}

func TestWorkerShutdownReleasesWithoutDeadLetter(t *testing.T) {
	fs := newFake()
	reg := NewRegistry()
	started := make(chan struct{})
	reg.MustRegister("block", func(ctx context.Context, _ *Job) error { close(started); <-ctx.Done(); return ctx.Err() }, HandlerOptions{})
	w := newTestWorker(fs, reg, nil)
	id := fs.add("block", 1) // last attempt: a normal failure would dead-letter it
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Start(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if j := fs.get(id); j.Status != StatusFailed || j.LastError != shutdownMsg {
		t.Fatalf("shutdown: %+v", j)
	}
}

func TestAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fs := newFake()
	dead := fs.add("t", 1)
	fs.jobs[dead].Status = StatusDead
	running := fs.add("t", 1)
	fs.jobs[running].Status = StatusRunning

	mk := func(perms ...string) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			rbac.SetPrincipal(c, &rbac.Principal{UserID: 1, Username: "u", Permissions: perms})
		})
		RegisterRoutes(r, APIDeps{Store: fs})
		return r
	}
	do := func(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	reader, admin := mk(PermRead), mk(PermRead, PermManage)

	if rec := do(reader, http.MethodGet, "/jobs?status=dead,running"); rec.Code != 200 || strings.Contains(rec.Body.String(), `"x"`) || !strings.Contains(rec.Body.String(), `"***"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := do(reader, http.MethodGet, "/jobs?status=bogus"); rec.Code != 400 {
		t.Fatalf("bad status: %d", rec.Code)
	}
	if rec := do(reader, http.MethodGet, "/jobs/999"); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := do(reader, http.MethodGet, "/jobs/abc"); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}
	if rec := do(reader, http.MethodPost, "/jobs/1/retry"); rec.Code != 403 {
		t.Fatalf("reader retry: %d", rec.Code)
	}
	if rec := do(admin, http.MethodDelete, "/jobs/2"); rec.Code != 409 {
		t.Fatalf("delete running: %d", rec.Code)
	}
	if rec := do(admin, http.MethodPost, "/jobs/2/retry"); rec.Code != 409 {
		t.Fatalf("retry running: %d", rec.Code)
	}
	if rec := do(admin, http.MethodPost, "/jobs/1/retry"); rec.Code != 200 || fs.get(dead).Status != StatusQueued {
		t.Fatalf("retry dead: %d %s", rec.Code, rec.Body)
	}
	fs.jobs[dead].Status = StatusSucceeded
	if rec := do(admin, http.MethodDelete, "/jobs/1"); rec.Code != 200 {
		t.Fatalf("delete succeeded: %d", rec.Code)
	}
	if _, err := fs.Get(context.Background(), dead); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
}

// metricValue reads a counter/gauge by gathering a private registry, so the
// test needs no import of client_model (keeps go.mod untouched).
func metricValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 1 || len(mfs[0].GetMetric()) != 1 {
		t.Fatalf("gather: %v %d", err, len(mfs))
	}
	m := mfs[0].GetMetric()[0]
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}
