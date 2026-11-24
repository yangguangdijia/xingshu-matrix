package bastion

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRemoteExecClampTimeout(t *testing.T) {
	cases := []struct{ in, want time.Duration }{
		{0, remoteExecDefaultTimeout},
		{-time.Second, remoteExecDefaultTimeout},
		{time.Second, time.Second},
		{30 * time.Minute, 30 * time.Minute},
		{31 * time.Minute, 30 * time.Minute},
		{24 * time.Hour, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := clampExecTimeout(c.in); got != c.want {
			t.Errorf("clampExecTimeout(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestRemoteExecTruncateOutput(t *testing.T) {
	if out, cut := truncateOutput([]byte("abc"), 10); string(out) != "abc" || cut {
		t.Fatalf("short input changed: %q %v", out, cut)
	}
	if out, cut := truncateOutput([]byte("abcdef"), 3); string(out) != "abc" || !cut {
		t.Fatalf("got %q %v", out, cut)
	}
	// "中" is 3 bytes; cutting at 4 must not split the second rune.
	out, cut := truncateOutput([]byte("中文"), 4)
	if !cut || string(out) != "中" || !utf8.Valid(out) {
		t.Fatalf("rune split: %q %v", out, cut)
	}
	if out, _ := truncateOutput([]byte("x"), -1); len(out) != 0 {
		t.Fatalf("negative max: %q", out)
	}
}

func TestRemoteExecCappedBuffer(t *testing.T) {
	b := newCappedBuffer(remoteExecMaxOutput)
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 1024) // 16 KiB
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ { // 640 KiB from concurrent stdout/stderr writers
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := b.Write(chunk); n != len(chunk) || err != nil {
				t.Errorf("write must always succeed: %d %v", n, err)
			}
		}()
	}
	wg.Wait()
	out, truncated := b.Bytes()
	if !truncated || len(out) != remoteExecMaxOutput {
		t.Fatalf("len=%d truncated=%v", len(out), truncated)
	}
	small := newCappedBuffer(16)
	_, _ = small.Write([]byte("hello"))
	if out, tr := small.Bytes(); string(out) != "hello" || tr {
		t.Fatalf("small: %q %v", out, tr)
	}
	exact := newCappedBuffer(5)
	_, _ = exact.Write([]byte("hello"))
	if out, tr := exact.Bytes(); string(out) != "hello" || tr {
		t.Fatalf("exact: %q %v", out, tr)
	}
}

func TestRemoteExecDecideFilter(t *testing.T) {
	rule := func(id int64, action, pat string) FilterRule {
		return FilterRule{ID: id, Name: action + "-rule", Re: regexp.MustCompile(pat), Action: action}
	}
	rules := []FilterRule{
		rule(1, "deny", `rm\s+-rf\s+/`),
		rule(2, "approve", `^reboot`),
		rule(3, "confirm", `^systemctl\s+restart`),
		rule(4, "allow", `^(ls|df|uptime)\b`),
	}
	cases := []struct {
		cmd      string
		run      bool
		action   string
		approval bool
		blocked  bool
		ruleID   int64
	}{
		{"uptime", true, "allow", false, false, 4},
		{"cat /etc/os-release", true, "allow", false, false, 0},
		{"systemctl restart nginx", true, "confirmed", false, false, 3},
		{"reboot", false, "cancelled", true, true, 2},
		{"rm -rf /", false, "deny", false, true, 1},
		{"ls; rm -rf /", false, "deny", false, true, 1}, // allow-list cannot shadow a deny segment
	}
	for _, c := range cases {
		d := decideExec(Evaluate(rules, c.cmd))
		if d.Run != c.run || d.AuditAction != c.action || d.NeedsApproval != c.approval || (d.Blocked != "") != c.blocked || d.RuleID != c.ruleID {
			t.Errorf("%q => %+v", c.cmd, d)
		}
	}
	if d := decideExec(&FilterRule{ID: 9, Action: "weird"}); d.Run || d.AuditAction != "deny" || d.Blocked == "" {
		t.Errorf("unknown action must fail closed: %+v", d)
	}
}

func TestRemoteExecScrubAndBanner(t *testing.T) {
	if got := string(scrubSecret([]byte("pw=S3cretPass ok"), "S3cretPass")); strings.Contains(got, "S3cretPass") {
		t.Fatalf("secret leaked: %q", got)
	}
	if got := string(scrubSecret([]byte("abc"), "ab")); got != "abc" {
		t.Fatalf("short secrets are not scrubbed: %q", got)
	}
	if os := osFromBanner("SSH-2.0-OpenSSH_8.9p1 Ubuntu-3ubuntu0.6"); os != "linux/ubuntu" {
		t.Fatalf("banner os=%q", os)
	}
	if os := osFromBanner("SSH-2.0-OpenSSH_9.6"); os != "unknown" {
		t.Fatalf("banner os=%q", os)
	}
	if os := osFromBanner(""); os != "" {
		t.Fatalf("empty banner os=%q", os)
	}
	if got := splitNodePath(" /prod/ web //db/ "); strings.Join(got, "|") != "prod|web|db" {
		t.Fatalf("node path=%v", got)
	}
}
