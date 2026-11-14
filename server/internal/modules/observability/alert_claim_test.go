package observability

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

func TestClaimDecision(t *testing.T) {
	cases := []struct {
		hs, by, user string
		want         ClaimDecision
	}{
		{"open", "", "alice", ClaimAllowed},
		{"acked", "", "alice", ClaimAllowed}, // 认领人为空的异常数据可重新认领
		{"acked", "alice", "alice", ClaimAlreadyMine},
		{"acked", "bob", "alice", ClaimTakenByOther},
		{"closed", "", "alice", ClaimClosed},
		{"closed", "alice", "alice", ClaimClosed},
	}
	for _, c := range cases {
		if got := DecideClaim(c.hs, c.by, c.user); got != c.want {
			t.Errorf("DecideClaim(%q,%q,%q)=%v want %v", c.hs, c.by, c.user, got, c.want)
		}
	}
}

func TestClaimConflictError(t *testing.T) {
	err := claimError(ClaimTakenByOther, "bob")
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("want 409, got %v", err)
	}
	if he.Message != "告警已被 bob 认领，如需接手请先与其确认" {
		t.Fatalf("unexpected message %q", he.Message)
	}
	if err := claimError(ClaimClosed, ""); !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("closed want 409, got %v", err)
	}
	if claimError(ClaimAllowed, "") != nil || claimError(ClaimAlreadyMine, "alice") != nil {
		t.Fatal("allowed decisions must not error")
	}
}

func TestClaimDuration(t *testing.T) {
	if d, err := ParseClaimDuration(""); err != nil || d != 2*time.Hour {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := ParseClaimDuration("24h"); err != nil || d != 24*time.Hour {
		t.Fatalf("24h: %v %v", d, err)
	}
	for _, bad := range []string{"1m", "100h", "abc", "-1h"} {
		if _, err := ParseClaimDuration(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestTimelineSortOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	in := []TimelineEvent{
		{At: t0.Add(30 * time.Minute), Kind: "resolved"},
		{At: t0.Add(5 * time.Minute), Kind: "silenced"},
		{At: t0.Add(5 * time.Minute), Kind: "claimed"},
		{At: t0, Kind: "notified", Message: "n1"},
		{At: t0, Kind: "triggered"},
		{At: t0, Kind: "notify_failed", Message: "n2"},
		{At: t0.Add(10 * time.Minute), Kind: "incident"},
		{Kind: "closed"}, // 零时间丢弃
	}
	got := SortTimeline(in)
	want := []string{"triggered", "notified", "notify_failed", "claimed", "silenced", "incident", "resolved"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Fatalf("pos %d: %s want %s (%+v)", i, got[i].Kind, k, got)
		}
	}
	if got[1].Message != "n1" || got[2].Message != "n2" {
		t.Fatal("equal-rank events must keep input order")
	}
}

func TestTimelineFromAlertRow(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	ack, end, closed := t0.Add(3*time.Minute), t0.Add(20*time.Minute), t0.Add(25*time.Minute)
	until := ack.Add(2 * time.Hour)
	a := &Alert{ID: 7, DatasourceID: 3, Fingerprint: "fp1", Alertname: "HighCPU", Severity: "critical", Status: "resolved",
		StartsAt: &t0, EndsAt: &end, FireCount: 2, AckedBy: "alice", AckedAt: &ack, ClosedBy: "alice", ClosedAt: &closed,
		CloseComment: "已扩容", CreatedAt: t0, Annotations: map[string]string{"summary": "CPU>90%"}}
	ev := SortTimeline(alertTimelineEvents(a, claimState{Comment: "处理中", SilenceID: "s-1", SilenceUntil: &until}))
	kinds := []string{}
	for _, e := range ev {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"triggered", "claimed", "silenced", "resolved", "closed"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds=%v want %v", kinds, want)
		}
	}
	if ev[1].Actor != "alice" || ev[1].Message != "认领告警：处理中" || ev[2].Ref != "s-1" {
		t.Fatalf("claim events: %+v", ev[1:3])
	}
	// 重新触发后认领人被清空：不出现认领事件
	a2 := *a
	a2.AckedBy, a2.Status, a2.ClosedAt = "", "firing", nil
	for _, e := range alertTimelineEvents(&a2, claimState{}) {
		if e.Kind == "claimed" || e.Kind == "resolved" || e.Kind == "closed" {
			t.Fatalf("unexpected %s for reopened alert", e.Kind)
		}
	}
	if p := alertNotifyKeyPrefix(a); p != "alert:3:fp1:" {
		t.Fatalf("prefix %q", p)
	}
}

func TestTimelineNotifyEvent(t *testing.T) {
	at := time.Now()
	if e := notifyTimelineEvent(1, "值班群", "firing", "success", "", "", at); e.Kind != "notified" || e.Actor != "通知中心" || e.Message != "触发通知已发送至「值班群」" {
		t.Fatalf("%+v", e)
	}
	if e := notifyTimelineEvent(2, "邮箱", "resolved", "failed", "timeout", "", at); e.Kind != "notify_failed" || e.Message != "恢复通知发送至「邮箱」失败：timeout" {
		t.Fatalf("%+v", e)
	}
	if e := notifyTimelineEvent(3, "群", "firing", "suppressed", "静默", "", at); e.Kind != "notify_suppressed" {
		t.Fatalf("%+v", e)
	}
}

func TestIncidentSeverityFromAlert(t *testing.T) {
	for in, want := range map[string]string{"critical": "P1", "P0": "P1", "P1": "P1", "warning": "P2", "": "P2", "info": "P3"} {
		if got := IncidentSeverityFor(in); got != want {
			t.Errorf("IncidentSeverityFor(%q)=%s want %s", in, got, want)
		}
	}
}
