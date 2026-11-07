package auth

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RemoteLimiter is a shared (cross-replica) limiter backend. Allow returns an
// error when the backend is unusable; callers then fall back to in-process.
type RemoteLimiter interface {
	Allow(ctx context.Context, key string) (ok bool, wait time.Duration, err error)
}

// UseRemote makes l consult r first (e.g. a RedisRateLimiter) and fall back
// to its in-process buckets when r errors. Call once at startup.
func (l *RateLimiter) UseRemote(r RemoteLimiter) *RateLimiter {
	l.mu.Lock()
	l.remote = r
	l.mu.Unlock()
	return l
}

// allowRemote is called by Allow; handled=false means "use local buckets".
func (l *RateLimiter) allowRemote(key string) (handled, ok bool, wait time.Duration) {
	l.mu.Lock()
	r := l.remote
	l.mu.Unlock()
	if r == nil {
		return false, false, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ok, wait, err := r.Allow(ctx, key)
	if err != nil {
		return false, false, 0
	}
	return true, ok, wait
}

// fixedWindowScript increments KEYS[1], sets its TTL on first hit and returns
// {count, pttl}. Atomic in Redis, so every replica shares one counter.
var fixedWindowScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// RedisRateLimiter is a fixed-window limiter shared through Redis: at most
// Limit hits per Window per key across all replicas.
//
// When Redis is unreachable it reports an error and trips a short circuit
// breaker (BreakFor) so a missing Redis does not add latency to every request;
// callers (RateLimiter.UseRemote, the API limiter middleware) then use their
// in-process fallback.
type RedisRateLimiter struct {
	Client   redis.Cmdable
	Prefix   string
	Limit    int
	Window   time.Duration
	BreakFor time.Duration

	downUntil atomic.Int64 // unix nanos
	now       func() time.Time
}

// NewRedisRateLimiter allows perMinute hits per key per minute.
func NewRedisRateLimiter(c redis.Cmdable, prefix string, perMinute int) *RedisRateLimiter {
	return &RedisRateLimiter{Client: c, Prefix: prefix, Limit: perMinute, Window: time.Minute, BreakFor: 30 * time.Second, now: time.Now}
}

// ErrLimiterDown is returned while the circuit breaker is open.
var ErrLimiterDown = errorString("rate limiter backend unavailable")

type errorString string

func (e errorString) Error() string { return string(e) }

// Allow implements RemoteLimiter.
func (r *RedisRateLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	if r == nil || r.Client == nil {
		return false, 0, ErrLimiterDown
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	if until := r.downUntil.Load(); until > 0 && now().UnixNano() < until {
		return false, 0, ErrLimiterDown
	}
	win := r.Window
	if win <= 0 {
		win = time.Minute
	}
	res, err := fixedWindowScript.Run(ctx, r.Client, []string{"xs:rl:" + r.Prefix + ":" + key}, win.Milliseconds()).Int64Slice()
	if err != nil || len(res) != 2 {
		br := r.BreakFor
		if br <= 0 {
			br = 30 * time.Second
		}
		r.downUntil.Store(now().Add(br).UnixNano())
		if err == nil {
			err = ErrLimiterDown
		}
		return false, 0, err
	}
	r.downUntil.Store(0)
	if res[0] <= int64(r.Limit) {
		return true, 0, nil
	}
	return false, time.Duration(res[1]) * time.Millisecond, nil
}
