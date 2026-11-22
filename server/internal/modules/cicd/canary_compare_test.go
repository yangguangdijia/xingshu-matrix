package cicd

import (
	"encoding/json"
	"testing"
	"time"
)

func compareFixture(status string, step, weight int) *Canary {
	return &Canary{ID: 7, Status: status, CurrentStep: step, Weight: weight, Namespace: "shop",
		Deployment: "api", CanaryDeployment: "api-canary", Service: "api", CanaryService: "api-canary",
		Ingress: "api", CanaryIngress: "api-canary", Image: "reg/api:v2", StableImage: "reg/api:v1",
		Plan:      canaryPlan{Steps: []canaryStep{{Weight: 10, PauseSec: 60}, {Weight: 30, PauseSec: 60}, {Weight: 50, Manual: true}, {Weight: 100, Manual: true}}},
		CreatedAt: time.Unix(1000, 0)}
}

func stepEvent(ts int64, data any) *CanaryEvent {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return &CanaryEvent{Kind: "step", TS: time.Unix(ts, 0), Data: raw}
}

func TestBuildCanaryCompareProgressing(t *testing.T) {
	c := compareFixture(canaryProgressing, 1, 30)
	events := []*CanaryEvent{
		{Kind: "operator", TS: time.Unix(1000, 0)},
		stepEvent(1060, map[string]any{"step": 1, "weight": 10}),
		{Kind: "error", TS: time.Unix(1070, 0), Data: json.RawMessage(`{"weight":99}`)}, // 非步骤事件不影响曲线
		stepEvent(1200, map[string]any{"step": 2, "weight": 30}),
	}
	out := buildCanaryCompare(c, events, time.Unix(1500, 0))
	if out.Final || out.TotalSteps != 4 || out.DoneSteps != 1 {
		t.Fatalf("progress: %+v", out)
	}
	want := []string{"done", "current", "pending", "pending"}
	for i, s := range out.Steps {
		if s.State != want[i] {
			t.Fatalf("step %d state %s want %s", i, s.State, want[i])
		}
	}
	if out.Steps[0].EnteredAt == nil || out.Steps[0].EnteredAt.Unix() != 1060 || out.Steps[2].EnteredAt != nil {
		t.Fatalf("entered: %+v", out.Steps)
	}
	if out.Stable.Weight != 70 || out.Canary.Weight != 30 || out.Stable.Image != "reg/api:v1" || out.Canary.Workload != "api-canary" {
		t.Fatalf("sides: %+v %+v", out.Stable, out.Canary)
	}
	if out.Start != 1000 || out.End != 1500 {
		t.Fatalf("window %d-%d", out.Start, out.End)
	}
	ws := out.WeightSeries
	exp := [][2]int64{{1000, 0}, {1060, 10}, {1200, 30}, {1500, 30}}
	if len(ws) != len(exp) {
		t.Fatalf("series %v", ws)
	}
	for i := range exp {
		if ws[i] != exp[i] {
			t.Fatalf("series %v want %v", ws, exp)
		}
	}
}

func TestBuildCanaryCompareRolledBackAndSucceeded(t *testing.T) {
	fin := time.Unix(2000, 0)
	c := compareFixture(canaryRolledBack, 1, 0)
	c.FinishedAt = &fin
	events := []*CanaryEvent{stepEvent(1060, map[string]any{"step": 1, "weight": 10}), stepEvent(1200, map[string]any{"step": 2, "weight": 30}), stepEvent(1900, nil)}
	out := buildCanaryCompare(c, events, time.Unix(9999, 0))
	if !out.Final || out.End != 2000 || out.Steps[1].State != "stopped" || out.Steps[0].State != "done" {
		t.Fatalf("rolled back: %+v", out)
	}
	last := out.WeightSeries[len(out.WeightSeries)-1]
	if last != [2]int64{2000, 0} || out.WeightSeries[len(out.WeightSeries)-2] != [2]int64{1900, 0} {
		t.Fatalf("rollback series %v", out.WeightSeries)
	}

	s := compareFixture(canarySucceeded, 3, 100)
	s.FinishedAt = &fin
	out = buildCanaryCompare(s, []*CanaryEvent{stepEvent(1060, map[string]any{"step": 1, "weight": 10}), stepEvent(1800, map[string]any{"image": "reg/api:v2"})}, time.Unix(9999, 0))
	if out.DoneSteps != 4 || out.Canary.Weight != 100 || out.Stable.Weight != 0 {
		t.Fatalf("succeeded: %+v", out)
	}
	if out.WeightSeries[len(out.WeightSeries)-1] != [2]int64{2000, 100} {
		t.Fatalf("succeeded series %v", out.WeightSeries)
	}
}

func TestBuildCanaryCompareEdgeCases(t *testing.T) {
	// 未开始：没有事件、步骤全部待执行；坏数据被忽略；乱序时间被拉平为非递减。
	c := compareFixture(canaryPending, -1, 0)
	out := buildCanaryCompare(c, nil, time.Unix(1100, 0))
	if out.DoneSteps != 0 || out.Steps[0].State != "pending" || len(out.WeightSeries) != 2 {
		t.Fatalf("pending: %+v", out)
	}
	bad := &CanaryEvent{Kind: "step", TS: time.Unix(1050, 0), Data: json.RawMessage(`{"weight":"x"`)}
	events := []*CanaryEvent{stepEvent(1100, map[string]any{"step": 1, "weight": 250}), bad, stepEvent(1090, map[string]any{"step": 2, "weight": -5})}
	c = compareFixture(canaryProgressing, 1, 30)
	out = buildCanaryCompare(c, events, time.Unix(1200, 0))
	for i := 1; i < len(out.WeightSeries); i++ {
		if out.WeightSeries[i][0] < out.WeightSeries[i-1][0] {
			t.Fatalf("non monotonic %v", out.WeightSeries)
		}
		if w := out.WeightSeries[i][1]; w < 0 || w > 100 {
			t.Fatalf("weight out of range %v", out.WeightSeries)
		}
	}
	// 结束时间早于开始时（时钟漂移）不产生负区间。
	early := time.Unix(10, 0)
	c = compareFixture(canaryFailed, 0, 10)
	c.FinishedAt = &early
	if out = buildCanaryCompare(c, nil, time.Unix(5, 0)); out.End < out.Start {
		t.Fatalf("negative window %d-%d", out.Start, out.End)
	}
}

func TestCompactWeightSeriesCaps(t *testing.T) {
	in := [][2]int64{}
	for i := 0; i < 300; i++ {
		in = append(in, [2]int64{int64(i), int64(i % 100)})
	}
	out := compactWeightSeries(in)
	if len(out) != canaryCompareMaxPoints || out[0] != in[0] || out[len(out)-1] != in[len(in)-1] {
		t.Fatalf("cap: %d %v %v", len(out), out[0], out[len(out)-1])
	}
	dup := compactWeightSeries([][2]int64{{1, 0}, {1, 0}, {2, 10}, {2, 10}, {3, 10}})
	if len(dup) != 3 {
		t.Fatalf("dedupe %v", dup)
	}
}

func TestCanaryMetricsTargetOf(t *testing.T) {
	c := compareFixture(canaryProgressing, 1, 30)
	if canaryMetricsTargetOf(c, 0) != nil {
		t.Fatal("unlinked target must not expose metrics target")
	}
	mt := canaryMetricsTargetOf(c, 9)
	if mt == nil || mt.ClusterID != 9 || mt.StableName != "api" || mt.CanaryName != "api-canary" || mt.CanaryIngress != "api-canary" || mt.WorkloadKind != "deployments" {
		t.Fatalf("target %+v", mt)
	}
	c.CanaryDeployment = ""
	if canaryMetricsTargetOf(c, 9) != nil {
		t.Fatal("missing canary workload")
	}
}
