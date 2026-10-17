package auth

import (
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket (in-process). It protects the login
// endpoint from password spraying per client IP; account lockout (stored in the
// database) protects individual accounts across replicas.
type RateLimiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute requests per key with a burst of perMinute.
func NewRateLimiter(perMinute int) *RateLimiter {
	return &RateLimiter{perMin: float64(perMinute), burst: float64(perMinute), buckets: map[string]*bucket{}, now: time.Now}
}

// Allow consumes one token for key; it returns false and the wait time when empty.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * l.perMin
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.perMin * float64(time.Minute))
	return false, wait
}
