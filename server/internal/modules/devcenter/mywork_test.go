package devcenter

import (
	"testing"
	"time"
)

func tp(t time.Time) *time.Time { return &t }

func TestStagesFromLinks(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	// 只有提交：代码完成
	r := stagesFromLinks([]vsLink{{Kind: "commit", Ref: "abc", At: tp(t0)}})
	if r.Stage != "code" || r.State != "done" || r.Stages["build"] != "none" {
		t.Fatalf("commit only: %+v", r)
	}
	// 运行成功 + 发布成功（两次发布取最近成功的环境）
	r = stagesFromLinks([]vsLink{
		{Kind: "mr", Ref: "!3", Status: "opened", At: tp(t0)},
		{Kind: "commit", Ref: "abc", At: tp(t0)},
		{Kind: "run", Ref: "11", Status: "succeeded", At: tp(t0.Add(time.Hour))},
		{Kind: "release", Ref: "5", Status: "succeeded", Env: "test", At: tp(t0.Add(2 * time.Hour))},
		{Kind: "release", Ref: "6", Status: "succeeded", Env: "prod", At: tp(t0.Add(3 * time.Hour))},
	})
	if r.Stage != "env" || r.State != "done" || r.EnvLatest != "prod" || r.ReleasedAt == nil || !r.ReleasedAt.Equal(t0.Add(3*time.Hour)) || r.FailReason != "" {
		t.Fatalf("full chain: %+v", r)
	}
	for _, k := range StageKeys {
		if r.Stages[k] != "done" {
			t.Fatalf("stage %s should be done: %+v", k, r.Stages)
		}
	}
	// 最近一次运行失败：构建失败、受阻原因
	r = stagesFromLinks([]vsLink{
		{Kind: "run", Ref: "1", Status: "succeeded", At: tp(t0)},
		{Kind: "run", Ref: "2", Status: "failed", At: tp(t0.Add(time.Hour))},
	})
	if r.Stages["build"] != "failed" || r.Stages["artifact"] != "done" || r.Stages["code"] != "done" || r.FailReason == "" || r.Stage != "artifact" {
		t.Fatalf("failed run: %+v", r)
	}
	// 发布失败带环境名
	r = stagesFromLinks([]vsLink{{Kind: "release", Ref: "9", Status: "failed", Env: "prod", At: tp(t0)}})
	if r.Stage != "release" || r.State != "failed" || r.FailReason != "最近一次发布失败（prod）" || r.Stages["env"] != "none" {
		t.Fatalf("failed release: %+v", r)
	}
	// 运行中
	r = stagesFromLinks([]vsLink{{Kind: "run", Ref: "3", Status: "running", At: tp(t0)}})
	if r.Stage != "build" || r.State != "running" {
		t.Fatalf("running: %+v", r)
	}
}

func TestStagesFromStatus(t *testing.T) {
	now := time.Now()
	cases := []struct{ kind, status, stage, state string }{
		{"requirements", "todo", "none", "none"},
		{"requirements", "doing", "code", "running"},
		{"requirements", "testing", "release", "running"},
		{"requirements", "done", "env", "done"},
		{"bugs", "fixed", "code", "done"},
		{"bugs", "closed", "env", "done"},
		{"bugs", "reopened", "code", "running"},
	}
	for _, c := range cases {
		r := stagesFromStatus(c.kind, c.status, now)
		if r.Stage != c.stage || r.State != c.state {
			t.Fatalf("%s/%s: got %s/%s", c.kind, c.status, r.Stage, r.State)
		}
		if (c.stage == "env") != (r.ReleasedAt != nil) {
			t.Fatalf("%s/%s released_at mismatch", c.kind, c.status)
		}
	}
}

func TestClassifyAndCounts(t *testing.T) {
	now := time.Now()
	mk := func(id int64, status string, upd time.Time) *MyItem {
		return &MyItem{ID: id, Status: status, UpdatedAt: upd}
	}
	a := mk(1, "doing", now)
	classify("requirements", a, []byte(`{}`), nil)
	if a.Bucket != "dev" || a.Source != "status" || a.Blocked {
		t.Fatalf("a: %+v", a)
	}
	b := mk(2, "doing", now.Add(-time.Hour))
	classify("requirements", b, []byte(`{}`), []vsLink{{Kind: "run", Ref: "2", Status: "failed", At: tp(now)}})
	if !b.Blocked || b.Bucket != "blocked" || b.BlockedReason != "最近一次构建失败" || b.Source != "links" {
		t.Fatalf("b: %+v", b)
	}
	c := mk(3, "doing", now.Add(-2*time.Hour))
	classify("requirements", c, []byte(`{"blocked":true,"blocked_reason":"等待接口方"}`), nil)
	if !c.Blocked || c.BlockedReason != "等待接口方" {
		t.Fatalf("c: %+v", c)
	}
	d := mk(4, "testing", now.Add(-3*time.Hour))
	classify("requirements", d, nil, []vsLink{{Kind: "run", Ref: "4", Status: "running", At: tp(now)}})
	if d.Bucket != "build" {
		t.Fatalf("d: %+v", d)
	}
	e := mk(5, "done", now.Add(-4*time.Hour))
	classify("requirements", e, nil, []vsLink{{Kind: "release", Ref: "9", Status: "failed", At: tp(now)}})
	if e.Blocked || e.Bucket != "released" {
		t.Fatalf("finished item must not be blocked by an old failure: %+v", e)
	}
	f := mk(6, "rejected", now)
	classify("requirements", f, nil, nil)
	if f.Bucket != "closed" {
		t.Fatalf("f: %+v", f)
	}
	items := []*MyItem{a, b, c, d, e, f}
	sortMine(items)
	if !items[0].Blocked || !items[1].Blocked || items[0].ID != 2 || items[2].ID != 1 {
		t.Fatalf("blocked must be pinned first: %v %v %v", items[0].ID, items[1].ID, items[2].ID)
	}
	var cnt MyCounts
	cnt.add(items)
	if cnt != (MyCounts{InDev: 1, InBuild: 1, Released: 1, Blocked: 2}) {
		t.Fatalf("counts: %+v", cnt)
	}
}

func TestNormState(t *testing.T) {
	for _, c := range [][3]string{{"run", "queued", "running"}, {"run", "cancelled", "failed"}, {"release", "succeeded", "done"}, {"mr", "merged", "done"}, {"mr", "closed", "none"}, {"commit", "", "done"}, {"run", "", "none"}} {
		if got := normState(c[0], c[1]); got != c[2] {
			t.Fatalf("%s/%s => %s want %s", c[0], c[1], got, c[2])
		}
	}
}
