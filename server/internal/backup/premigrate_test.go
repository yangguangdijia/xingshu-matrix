package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type hookProbe struct {
	existing, recorded bool
	stateErr, snapErr  error
	snapCalls          int
	snapRecorded       bool
}

func (p *hookProbe) hook(enabled, warn bool) *preMigrate {
	return &preMigrate{
		enabled: enabled, warnOnly: warn,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: func(context.Context) (bool, bool, error) {
			return p.existing, p.recorded, p.stateErr
		},
		snapshot: func(_ context.Context, recorded bool) (string, error) {
			p.snapCalls++
			p.snapRecorded = recorded
			return "xs-20261114T000000Z.dump", p.snapErr
		},
	}
}

var pending = []string{"core/0012_jobs", "cicd/0004_targets"}

func TestPreMigrateTakesRecordedBackup(t *testing.T) {
	p := &hookProbe{existing: true, recorded: true}
	if err := p.hook(true, false).run(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if p.snapCalls != 1 || !p.snapRecorded {
		t.Fatalf("snapshot calls=%d recorded=%v", p.snapCalls, p.snapRecorded)
	}
}

func TestPreMigrateUnrecordedWhenTableMissing(t *testing.T) {
	p := &hookProbe{existing: true, recorded: false}
	if err := p.hook(true, false).run(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if p.snapCalls != 1 || p.snapRecorded {
		t.Fatalf("snapshot calls=%d recorded=%v", p.snapCalls, p.snapRecorded)
	}
}

func TestPreMigrateSkips(t *testing.T) {
	cases := map[string]struct {
		probe   *hookProbe
		enabled bool
		pending []string
	}{
		"disabled":     {&hookProbe{existing: true, recorded: true}, false, pending},
		"fresh db":     {&hookProbe{existing: false}, true, pending},
		"nothing todo": {&hookProbe{existing: true, recorded: true}, true, nil},
	}
	for name, c := range cases {
		if err := c.probe.hook(c.enabled, false).run(context.Background(), c.pending); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.probe.snapCalls != 0 {
			t.Fatalf("%s: snapshot taken", name)
		}
	}
}

func TestPreMigrateFailureAbortsWithChineseError(t *testing.T) {
	p := &hookProbe{existing: true, recorded: true, snapErr: errors.New("pg_dump 执行失败")}
	err := p.hook(true, false).run(context.Background(), pending)
	if err == nil {
		t.Fatal("expected abort")
	}
	msg := err.Error()
	for _, want := range []string{"迁移前自动备份失败", "已中止数据库迁移", "core/0012_jobs", "pg_dump 执行失败", "MATRIX_BACKUP_BEFORE_MIGRATE"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q lacks %q", msg, want)
		}
	}
	// state check failures abort too
	p = &hookProbe{stateErr: errors.New("检查数据库状态失败")}
	if err := p.hook(true, false).run(context.Background(), pending); err == nil || p.snapCalls != 0 {
		t.Fatalf("state error: err=%v calls=%d", err, p.snapCalls)
	}
}

func TestPreMigrateFailureWarnOnly(t *testing.T) {
	p := &hookProbe{existing: true, recorded: true, snapErr: errors.New("boom")}
	if err := p.hook(true, true).run(context.Background(), pending); err != nil {
		t.Fatalf("warn-only must not abort: %v", err)
	}
	p = &hookProbe{stateErr: errors.New("boom")}
	if err := p.hook(true, true).run(context.Background(), pending); err != nil {
		t.Fatalf("warn-only must not abort on state error: %v", err)
	}
}

func TestParseFailureMode(t *testing.T) {
	for in, want := range map[string]bool{"": false, "abort": false, "WARN": true, " warn ": true} {
		got, err := ParseFailureMode(in)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseFailureMode("ignore"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestPreMigrateHookNilRunner(t *testing.T) {
	if err := PreMigrateHook(nil)(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
}

func TestSummarize(t *testing.T) {
	got := summarize([]string{"a", "b", "c"}, 2)
	if got != "a、b 等 3 项" {
		t.Fatalf("got %q", got)
	}
}

func TestPolicy(t *testing.T) {
	s, err := ParseCron("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Keep: 7, Schedule: &s, ScheduleSpec: "0 2 * * *", BeforeMigrate: true, PreMigrateWarnOnly: true}
	p := r.Policy()
	if !p.ScheduleEnabled || p.Schedule != "0 2 * * *" || p.Keep != 7 || !p.BeforeMigrate || p.BeforeMigrateOnFailure != FailureWarn || p.Storage != "local" || p.OnlineRestore {
		t.Fatalf("policy %+v", p)
	}
	p = (&Runner{ScheduleSpec: "0 2 * * *"}).Policy()
	if p.ScheduleEnabled || p.Schedule != "" || p.BeforeMigrateOnFailure != FailureAbort {
		t.Fatalf("policy %+v", p)
	}
}
