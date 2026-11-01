package bastion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type watchdogStore struct {
	kill bool
	err  error
	seen chan sessionRef
}

func (s *watchdogStore) renew(_ context.Context, ref sessionRef) (bool, error) {
	s.seen <- ref
	return s.kill, s.err
}
func (*watchdogStore) finish(context.Context, sessionRef, sessionEnd) error { return nil }
func (*watchdogStore) reap(context.Context) error                           { return nil }

func TestWatchdogCancelsOnlyRequestedLocalSession(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		kill         bool
		err          error
	}{
		{"persisted kill", "killed", true, nil},
		{"database failure", "lease_lost", false, errors.New("unavailable")},
		{"expired or wrong fence", "lease_lost", false, errFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, other := New(), New()
			var otherCancelled atomic.Bool
			ref := sessionRef{ID: 10, Owner: m.owner, Fence: newToken()}
			foreign := sessionRef{ID: 11, Owner: other.owner, Fence: newToken()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.register(ref, cancel)
			other.register(foreign, func() { otherCancelled.Store(true) })
			defer m.unregister(ref.ID)
			defer other.unregister(foreign.ID)
			s := &watchdogStore{kill: tc.kill, err: tc.err, seen: make(chan sessionRef, 1)}
			m.store = s
			done := m.watchLease(ctx, ref)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("watchdog did not cancel")
			}
			if got := <-s.seen; got != ref {
				t.Fatal("renewal did not use owner and fence")
			}
			if m.sessionReason(ref.ID) != tc.reason || ctx.Err() == nil {
				t.Fatal("wrong cancellation reason")
			}
			if otherCancelled.Load() {
				t.Fatal("another process was cancelled")
			}
		})
	}
}

func TestShutdownOnlyCancelsOwnedAndRejectsAdmission(t *testing.T) {
	m := New()
	var owned, foreign atomic.Bool
	m.register(sessionRef{ID: 1, Owner: m.owner, Fence: newToken()}, func() { owned.Store(true) })
	m.register(sessionRef{ID: 2, Owner: "another-process", Fence: newToken()}, func() { foreign.Store(true) })
	m.cancelOwned("shutdown")
	if !owned.Load() || foreign.Load() {
		t.Fatal("shutdown crossed process ownership")
	}
	if m.register(sessionRef{ID: 3, Owner: m.owner}, func() {}) {
		t.Fatal("new admission during shutdown")
	}
	if m.unregister(1) != "shutdown" || m.unregister(2) != "" {
		t.Fatal("shutdown reported a kill")
	}
}

func TestKillSuccessRequiresObservedAcknowledgement(t *testing.T) {
	for _, r := range []killResult{
		{}, {RequestID: "r", Status: "active"},
		{RequestID: "r", Status: "closed", Acknowledged: true},
		{RequestID: "r", Status: "failed", Acknowledged: true},
		{RequestID: "r", Status: "killed"},
		{Status: "killed", Acknowledged: true},
	} {
		if r.killed() {
			t.Fatalf("unobserved kill reported success: %+v", r)
		}
	}
	if !(killResult{RequestID: "r", Status: "killed", Acknowledged: true}).killed() {
		t.Fatal("observed acknowledged kill not recognized")
	}
}

func TestRecordingFailClosedAndNoOverwrite(t *testing.T) {
	root := t.TempDir()
	m := New()
	m.recordDir = root
	m.storageID = ""
	if err := m.recordingReady(); err == nil {
		t.Fatal("unconfigured storage accepted")
	}
	m.storageID = "test-volume"
	if err := m.recordingReady(); err == nil {
		t.Fatal("ordinary local temp directory accepted as shared storage")
	}
	path := filepath.Join(root, "original.cast")
	r, err := NewRecorder(path, 80, 24, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Output([]byte("public marker")); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err = NewRecorder(path, 80, 24, "overwrite"); err == nil {
		t.Fatal("recorder overwrote existing file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing recording changed")
	}
	if err = r.Output([]byte("late")); err == nil {
		t.Fatal("closed recorder silently succeeded")
	}
	broken, err := NewRecorder(filepath.Join(root, "broken.cast"), 80, 24, "audit")
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.f.Close()
	if err = broken.Output([]byte("public marker")); err == nil {
		t.Fatal("write failure ignored")
	}
	if err = broken.Close(); err == nil {
		t.Fatal("close hid previous recording failure")
	}
}

func TestRecordingIdentityAndIsolation(t *testing.T) {
	m := New()
	m.recordDir = t.TempDir()
	m.storageID = "volume-a"
	fence := newToken()
	base := Session{ID: 42, FenceToken: fence, Status: "closed", RecordingState: "ready",
		RecordingPath: "42-" + fence + ".cast", RecordingStorageID: "volume-a"}
	for _, tc := range []struct {
		name   string
		change func(*Session)
	}{
		{"another session", func(s *Session) { s.ID = 43 }},
		{"another fence", func(s *Session) { s.FenceToken = newToken() }},
		{"another volume", func(s *Session) { s.RecordingStorageID = "volume-b" }},
		{"partial", func(s *Session) { s.RecordingState = "recording" }},
		{"failed persistence", func(s *Session) { s.RecordingState = "failed" }},
		{"active", func(s *Session) { s.Status = "active" }},
		{"traversal", func(s *Session) { s.RecordingPath = "../" + s.RecordingPath }},
		{"absolute", func(s *Session) { s.RecordingPath = filepath.Join(m.recordDir, s.RecordingPath) }},
		{"windows traversal", func(s *Session) { s.RecordingPath = `..\` + s.RecordingPath }},
		{"legacy basename alias", func(s *Session) { s.RecordingPath = "legacy.cast" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.change(&s)
			if _, err := m.openRecording(&s); err == nil || !strings.Contains(err.Error(), "not finalized") {
				t.Fatalf("identity mismatch not rejected before storage access: %v", err)
			}
			if m.hasRecording(&s) {
				t.Fatal("invalid identity advertised a recording")
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "outside.cast")
	if err := os.WriteFile(outside, []byte("public marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.recordDir, "link.cast")
	if err := os.Symlink(outside, link); err == nil {
		if f, err := openRegular(m.recordDir, "link.cast"); err == nil {
			f.Close()
			t.Fatal("symlink escaped recording root")
		}
	} else {
		t.Log("symlink privilege unavailable; identity and traversal checks still exercised")
	}
	if _, err := openRegular(m.recordDir, "../outside.cast"); err == nil {
		t.Fatal("traversal accepted")
	}
	f, err := openRegular(filepath.Dir(outside), filepath.Base(outside))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestNFSMountVerificationUsesMostSpecificMount(t *testing.T) {
	base := "36 20 0:32 / / rw,relatime - overlay overlay rw\n" +
		"40 36 0:40 / /data rw,relatime - nfs4 192.168.6.3:/storage/storageclass rw\n"
	dir := filepath.FromSlash("/data/recordings")
	if !nfsMount(dir, base) {
		t.Fatal("NFS4 parent mount not recognized")
	}
	if nfsMount(dir, base+"41 40 0:41 / /data/recordings rw - tmpfs tmpfs rw\n") {
		t.Fatal("nested emptyDir accepted")
	}
	if nfsMount(filepath.FromSlash("/database/recordings"), base) {
		t.Fatal("mount prefix matched sibling directory")
	}
	if nfsMount(dir, strings.ReplaceAll(base, "/data rw,", "/data ro,")) {
		t.Fatal("read-only mount accepted")
	}
	if nfsMount(dir, strings.ReplaceAll(base, "nfs4", "ext4")) {
		t.Fatal("local filesystem accepted")
	}
}
