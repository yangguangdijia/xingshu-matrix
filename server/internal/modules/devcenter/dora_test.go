package devcenter

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func doraT(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func doraTP(s string) *time.Time { t := doraT(s); return &t }

func TestDoraPercentile(t *testing.T) {
	if doraPercentile(nil, 50) != 0 {
		t.Fatal("empty must be 0")
	}
	if v := doraPercentile([]float64{5}, 90); v != 5 {
		t.Fatalf("single: %v", v)
	}
	if v := doraPercentile([]float64{4, 1, 3, 2}, 50); v != 2.5 {
		t.Fatalf("median: %v", v)
	}
	if v := doraPercentile([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90); doraRound(v, 2) != 9.1 {
		t.Fatalf("p90: %v", v)
	}
}

func TestDoraLevels(t *testing.T) {
	cases := []struct{ got, want string }{
		{doraLevelDeployFreq(2, true), doraElite},
		{doraLevelDeployFreq(1, true), doraElite},
		{doraLevelDeployFreq(0.2, true), doraHigh},
		{doraLevelDeployFreq(0.05, true), doraMedium},
		{doraLevelDeployFreq(0, true), doraLow},
		{doraLevelDeployFreq(0, false), doraUnknown},
		{doraLevelLeadTime(nil), doraUnknown},
		{doraLevelLeadTime(doraPtr(3)), doraElite},
		{doraLevelLeadTime(doraPtr(48)), doraHigh},
		{doraLevelLeadTime(doraPtr(24 * 10)), doraMedium},
		{doraLevelLeadTime(doraPtr(24 * 40)), doraLow},
		{doraLevelCFR(nil), doraUnknown},
		{doraLevelCFR(doraPtr(0.05)), doraElite},
		{doraLevelCFR(doraPtr(0.08)), doraHigh},
		{doraLevelCFR(doraPtr(0.15)), doraMedium},
		{doraLevelCFR(doraPtr(0.5)), doraLow},
		{doraLevelMTTR(nil), doraUnknown},
		{doraLevelMTTR(doraPtr(0.5)), doraElite},
		{doraLevelMTTR(doraPtr(5)), doraHigh},
		{doraLevelMTTR(doraPtr(48)), doraMedium},
		{doraLevelMTTR(doraPtr(200)), doraLow},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Fatalf("case %d: got %s want %s", i, c.got, c.want)
		}
	}
}

func TestDoraFailedDeploys(t *testing.T) {
	rels := []doraRelease{
		{ID: 1, AppID: 1, Env: "prod", Kind: "deploy", Status: "succeeded", CreatedAt: doraT("2026-11-10T01:00:00Z"), FinishedAt: doraTP("2026-11-10T01:10:00Z")},
		{ID: 2, AppID: 1, Env: "prod", Kind: "deploy", Status: "succeeded", CreatedAt: doraT("2026-11-10T05:00:00Z"), FinishedAt: doraTP("2026-11-10T05:10:00Z")},
		// 回滚只归到它之前最近一次成功部署（2），不连累 1
		{ID: 3, AppID: 1, Env: "prod", Kind: "rollback", Status: "succeeded", CreatedAt: doraT("2026-11-10T08:00:00Z"), FinishedAt: doraTP("2026-11-10T08:05:00Z")},
		// 不同环境的回滚不影响 prod
		{ID: 4, AppID: 1, Env: "test", Kind: "deploy", Status: "succeeded", CreatedAt: doraT("2026-11-11T01:00:00Z"), FinishedAt: doraTP("2026-11-11T01:10:00Z")},
		// 超过 24 小时的回滚不算
		{ID: 5, AppID: 1, Env: "test", Kind: "rollback", Status: "succeeded", CreatedAt: doraT("2026-11-12T03:00:00Z")},
		{ID: 6, AppID: 2, Env: "prod", Kind: "deploy", Status: "failed", CreatedAt: doraT("2026-11-12T01:00:00Z"), FinishedAt: doraTP("2026-11-12T01:03:00Z")},
		{ID: 7, AppID: 2, Env: "prod", Kind: "deploy", Status: "succeeded", CanaryAborted: true, CreatedAt: doraT("2026-11-13T01:00:00Z"), FinishedAt: doraTP("2026-11-13T01:03:00Z")},
	}
	f := doraFailedDeploys(rels)
	want := map[int64]bool{2: true, 6: true, 7: true}
	if len(f) != len(want) {
		t.Fatalf("failed set %v", f)
	}
	for id := range want {
		if !f[id] {
			t.Fatalf("release %d should be failed: %v", id, f)
		}
	}
}

func TestComputeDora(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	to := doraT("2026-11-14T12:00:00+08:00")
	from := time.Date(2026, 11, 8, 0, 0, 0, 0, loc) // 7 天窗口
	rels := []doraRelease{
		{ID: 1, AppID: 1, Env: "prod", Kind: "deploy", Status: "succeeded", CreatedAt: doraT("2026-11-08T09:00:00+08:00"), FinishedAt: doraTP("2026-11-08T10:00:00+08:00"), LeadStart: doraT("2026-11-08T08:00:00+08:00")},
		{ID: 2, AppID: 1, Env: "prod", Kind: "deploy", Status: "succeeded", CreatedAt: doraT("2026-11-08T20:00:00+08:00"), FinishedAt: doraTP("2026-11-08T21:00:00+08:00"), LeadStart: doraT("2026-11-07T21:00:00+08:00"), LeadFromLinks: true},
		{ID: 3, AppID: 1, Env: "prod", Kind: "deploy", Status: "failed", CreatedAt: doraT("2026-11-10T09:00:00+08:00"), FinishedAt: doraTP("2026-11-10T09:05:00+08:00"), LeadStart: doraT("2026-11-10T08:00:00+08:00")},
		{ID: 4, AppID: 1, Env: "prod", Kind: "deploy", Status: "running", CreatedAt: doraT("2026-11-14T11:00:00+08:00")},
		{ID: 5, AppID: 1, Env: "prod", Kind: "rollback", Status: "succeeded", CreatedAt: doraT("2026-11-08T22:00:00+08:00"), FinishedAt: doraTP("2026-11-08T22:01:00+08:00")},
	}
	inc := []doraIncident{
		{DeclaredAt: doraT("2026-11-09T00:00:00Z"), ResolvedAt: doraTP("2026-11-09T02:00:00Z")},
		{DeclaredAt: doraT("2026-11-10T00:00:00Z"), ResolvedAt: doraTP("2026-11-10T04:00:00Z")},
		{DeclaredAt: doraT("2026-11-11T00:00:00Z")}, // 未解决不计
	}
	r := computeDora(rels, inc, true, from, to, 7, loc)
	if r.WindowDays != 7 || len(r.DeployFrequency.Series) != 7 || r.DeployFrequency.Series[0][0] != "2026-11-08" || r.DeployFrequency.Series[0][1] != 2 {
		t.Fatalf("series %v", r.DeployFrequency.Series)
	}
	if r.DeployFrequency.Total != 2 || r.DeployFrequency.PerDay != 0.286 || r.Level.DeployFrequency != doraHigh {
		t.Fatalf("deploy freq %+v %s", r.DeployFrequency, r.Level.DeployFrequency)
	}
	if r.LeadTime.Samples != 2 || *r.LeadTime.MedianHours != 13 || *r.LeadTime.P90Hours != 21.8 || r.LeadTime.Basis != "mixed" || r.Level.LeadTime != doraElite {
		t.Fatalf("lead time %+v", r.LeadTime)
	}
	// 分母 3（1、2、3，running 不计，回滚不计）；失败 2（2 被回滚，3 失败）
	if r.ChangeFailureRate.Total != 3 || r.ChangeFailureRate.Failed != 2 || *r.ChangeFailureRate.Rate != 0.6667 || r.Level.ChangeFailureRate != doraLow {
		t.Fatalf("cfr %+v", r.ChangeFailureRate)
	}
	if r.MTTR.Samples != 2 || *r.MTTR.MedianHours != 3 || r.Level.MTTR != doraHigh || !r.MTTR.Available {
		t.Fatalf("mttr %+v", r.MTTR)
	}
}

func TestComputeDoraEmpty(t *testing.T) {
	loc := time.UTC
	to := doraT("2026-11-14T12:00:00Z")
	from := doraT("2026-10-16T00:00:00Z")
	r := computeDora(nil, nil, false, from, to, 30, loc)
	if r.Level != (DoraLevels{doraUnknown, doraUnknown, doraUnknown, doraUnknown}) {
		t.Fatalf("levels %+v", r.Level)
	}
	if r.LeadTime.Basis != "none" || r.ChangeFailureRate.Rate != nil || r.MTTR.Available || len(r.DeployFrequency.Series) != 30 {
		t.Fatalf("empty report %+v", r)
	}
}

func TestRegisterDora(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	noop := func(c *gin.Context) {}
	(&Module{}).registerDora(r.Group("/devcenter"), noop, noop)
	found := false
	for _, rt := range r.Routes() {
		if rt.Method == "GET" && rt.Path == "/devcenter/dora" {
			found = true
		}
	}
	if !found {
		t.Fatal("GET /devcenter/dora not registered")
	}
}
