package bastion

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestEvaluateSegmentDenyBlocksBypass(t *testing.T) {
	rs := []FilterRule{
		{ID: 1, Name: "allow-ls", Re: regexp.MustCompile(`^ls\b`), Action: "allow"},
		{ID: 2, Name: "rm-root", Re: regexp.MustCompile(`^rm\s+-rf\s+/$`), Action: "deny"},
	}
	for _, cmd := range []string{"ls; rm -rf /", "ls && rm -rf /", "echo $(rm -rf /)", "sudo /bin/rm -rf /"} {
		if r := Evaluate(rs, cmd); r == nil || r.ID != 2 {
			t.Fatalf("%q => %+v, want deny rule 2", cmd, r)
		}
	}
	if r := Evaluate(rs, "ls -la"); r == nil || r.Action != "allow" {
		t.Fatalf("ls -la => %+v", r)
	}
	p := NewInputProcessor(rs)
	remote, _, ev := p.Input([]byte("ls; rm -rf /\r"))
	if len(ev) != 1 || ev[0].Action != "deny" || ev[0].RuleID != 2 || strings.Contains(string(remote), "rm -rf /\r") {
		t.Fatalf("events=%+v remote=%q", ev, remote)
	}
}

func TestEvaluateStrictestActionWins(t *testing.T) {
	rs := []FilterRule{
		{ID: 1, Name: "confirm-ls", Re: regexp.MustCompile(`^ls`), Action: "confirm"},
		{ID: 2, Name: "approve-reboot", Re: regexp.MustCompile(`^reboot`), Action: "approve"},
		{ID: 3, Name: "deny-shutdown", Re: regexp.MustCompile(`^shutdown`), Action: "deny"},
	}
	if r := Evaluate(rs, "ls | reboot"); r == nil || r.ID != 2 {
		t.Fatalf("got %+v, want approve", r)
	}
	if r := Evaluate(rs, "ls; reboot; shutdown -h now"); r == nil || r.ID != 3 {
		t.Fatalf("got %+v, want deny", r)
	}
	if r := Evaluate(rs, "pwd"); r != nil {
		t.Fatalf("got %+v, want nil", r)
	}
}

func TestEvaluateAllowNeedsWholeCommand(t *testing.T) {
	rs := []FilterRule{
		{ID: 1, Name: "allow-tmp", Re: regexp.MustCompile(`^rm -rf /tmp/`), Action: "allow"},
		{ID: 2, Name: "rm", Re: regexp.MustCompile(`rm\s+-rf`), Action: "deny"},
	}
	if r := Evaluate(rs, "rm -rf /tmp/x"); r == nil || r.ID != 1 {
		t.Fatalf("got %+v", r)
	}
	if r := Evaluate(rs, "rm -rf /tmp/x; rm -rf /var"); r == nil || r.ID != 2 {
		t.Fatalf("allow must not cover other segments: %+v", r)
	}
	if r := Evaluate(rs, "echo hi && rm -rf /tmp/x"); r == nil || r.ID != 2 {
		t.Fatalf("allow matching only a segment must not short-circuit: %+v", r)
	}
}

func TestLoginACLExemptMinutesInput(t *testing.T) {
	in := aclIn{Name: "r", SubjectType: "all", Action: "review", ExemptMinutes: 10081}
	if in.check() == nil {
		t.Fatal("exempt_minutes > 10080 must fail")
	}
	in = aclIn{Name: "r", SubjectType: "all", Action: "review", ExemptMinutes: -1}
	if in.check() == nil {
		t.Fatal("negative exempt_minutes must fail")
	}
	in = aclIn{Name: "r", SubjectType: "all", Action: "review", ExemptMinutes: 60}
	if err := in.check(); err != nil || in.ExemptMinutes != 60 {
		t.Fatalf("err=%v in=%+v", err, in)
	}
	in = aclIn{Name: "d", SubjectType: "all", Action: "deny", ExemptMinutes: 60}
	if err := in.check(); err != nil || in.ExemptMinutes != 0 {
		t.Fatalf("non-review rule must reset exempt_minutes: err=%v in=%+v", err, in)
	}
}

func TestExemptMigrationEmbedded(t *testing.T) {
	b, err := fs.ReadFile(migFS, "migrations/0007_acl_exempt.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"exempt_minutes INT NOT NULL DEFAULT 0", "BETWEEN 0 AND 10080", "'review'", "bastion_acl_exemptions", "WHERE revoked_at IS NULL"} {
		if !strings.Contains(s, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
	if _, err := fs.ReadFile(migFS, "migrations/0007_acl_exempt.down.sql"); err != nil {
		t.Fatal(err)
	}
}
