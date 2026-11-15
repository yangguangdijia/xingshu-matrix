package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBackoffExponentialAndCapped(t *testing.T) {
	base, max := 10*time.Second, 5*time.Minute
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := Backoff(i+1, base, max, 0, 0); got != w {
			t.Errorf("attempt %d: got %v want %v", i+1, got, w)
		}
	}
	if got := Backoff(1000, base, max, 0, 0); got != max {
		t.Errorf("huge attempt must clamp to max, got %v", got)
	}
	if got := Backoff(0, base, max, 0, 0); got != base {
		t.Errorf("attempt 0 treated as 1, got %v", got)
	}
	if got := Backoff(1, 0, 0, 0, 0); got != DefaultBaseBackoff {
		t.Errorf("defaults: got %v", got)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	base, max := 10*time.Second, time.Hour
	lo := Backoff(3, base, max, 0.5, 0)
	hi := Backoff(3, base, max, 0.5, 0.999999)
	mid := Backoff(3, base, max, 0.5, 0.5)
	if lo != 30*time.Second || mid != 40*time.Second || hi < 49*time.Second || hi > 50*time.Second {
		t.Fatalf("jitter range wrong: lo=%v mid=%v hi=%v", lo, mid, hi)
	}
	// jitter never exceeds max
	if got := Backoff(50, base, max, 1, 0.999); got > max {
		t.Fatalf("jitter exceeded max: %v", got)
	}
	// and never goes under base/2
	if got := Backoff(1, base, max, 1, 0); got < base/2 {
		t.Fatalf("jitter under base/2: %v", got)
	}
}

func TestDecideStateMachine(t *testing.T) {
	bo := func(a int) time.Duration { return time.Duration(a) * time.Minute }
	if o := Decide(nil, 1, 3, bo); o.Status != StatusSucceeded || o.Error != "" {
		t.Fatalf("success: %+v", o)
	}
	if o := Decide(errors.New("boom"), 1, 3, bo); o.Status != StatusFailed || o.Delay != time.Minute || o.Error != "boom" {
		t.Fatalf("first failure: %+v", o)
	}
	if o := Decide(errors.New("boom"), 2, 3, bo); o.Status != StatusFailed || o.Delay != 2*time.Minute {
		t.Fatalf("second failure: %+v", o)
	}
	if o := Decide(errors.New("boom"), 3, 3, bo); o.Status != StatusDead {
		t.Fatalf("exhausted must be dead: %+v", o)
	}
	if o := Decide(fmt.Errorf("wrap: %w", Permanent(errors.New("bad payload"))), 1, 5, bo); o.Status != StatusDead || !strings.Contains(o.Error, "bad payload") {
		t.Fatalf("permanent must be dead: %+v", o)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil")
	}
}

func TestRetryDeleteRules(t *testing.T) {
	cases := map[string][2]bool{ // status -> {canRetry, canDelete}
		StatusQueued:    {false, false},
		StatusRunning:   {false, false},
		StatusFailed:    {true, false},
		StatusDead:      {true, true},
		StatusSucceeded: {false, true},
	}
	for st, w := range cases {
		if CanRetry(st) != w[0] || CanDelete(st) != w[1] {
			t.Errorf("%s: retry=%v delete=%v", st, CanRetry(st), CanDelete(st))
		}
	}
	if ValidStatus("bogus") || !ValidStatus(StatusDead) {
		t.Fatal("ValidStatus")
	}
}

func TestTruncateError(t *testing.T) {
	long := strings.Repeat("错", 3000) // 9000 bytes
	got := TruncateError(long)
	if len(got) > maxErrorLen+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("len=%d", len(got))
	}
	if !json.Valid([]byte(fmt.Sprintf("%q", got))) || strings.ContainsRune(strings.TrimSuffix(got, "…"), '\uFFFD') {
		t.Fatal("truncation split a rune")
	}
	if TruncateError("short") != "short" {
		t.Fatal("short changed")
	}
}

func TestRedactPayload(t *testing.T) {
	in := json.RawMessage(`{"host":"a","password":"p","nested":{"API_KEY":"k","list":[{"token":"t","x":1}]},"ssh_private_key":"pk"}`)
	out := string(RedactPayload(in))
	for _, leak := range []string{`"p"`, `"k"`, `"t"`, `"pk"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %s in %s", leak, out)
		}
	}
	if !strings.Contains(out, `"host":"a"`) || !strings.Contains(out, `"x":1`) {
		t.Fatalf("non-sensitive data lost: %s", out)
	}
	if string(RedactPayload(nil)) != "{}" || string(RedactPayload(json.RawMessage("not json"))) != `"***"` {
		t.Fatal("edge cases")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	h := func(context.Context, *Job) error { return nil }
	if err := r.Register("", h, HandlerOptions{}); err == nil {
		t.Fatal("empty type accepted")
	}
	if err := r.Register("a.b", nil, HandlerOptions{}); err == nil {
		t.Fatal("nil handler accepted")
	}
	if err := r.Register("b", h, HandlerOptions{MaxAttempts: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("a", h, HandlerOptions{MaxAttempts: 2, Jitter: 3}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("a", h, HandlerOptions{}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if got := r.Types(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("types %v", got)
	}
	if o := r.Options("a"); o.MaxAttempts != 2 || o.Jitter != 1 || o.Timeout != DefaultTimeout {
		t.Fatalf("opts %+v", o)
	}
	if o := r.Options("b"); o.MaxAttempts != MaxMaxAttempts {
		t.Fatalf("max clamp %+v", o)
	}
	if o := r.Options("missing"); o.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("default %+v", o)
	}
}

func TestParseListFilter(t *testing.T) {
	f, err := parseListFilter(" dead,failed,dead ,", " x ")
	if err != nil || len(f.Statuses) != 2 || f.Type != "x" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseListFilter("dead,nope", ""); err == nil {
		t.Fatal("invalid status accepted")
	}
	if _, err := parseListFilter("", strings.Repeat("t", 200)); err == nil {
		t.Fatal("long type accepted")
	}
}
