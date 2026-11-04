package observability

import (
	"testing"
	"time"
)

func TestCurrentShiftRotationAndOverride(t *testing.T) {
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r := Rotation{Layer: 1, Start: start, Length: 24 * time.Hour, Members: []string{"alice", "bob", "carol"}}
	now := start.Add(49 * time.Hour) // 第 3 天
	s, ok := CurrentShift(r, nil, now)
	if !ok || s.Username != "carol" || s.NextUsername != "alice" || !s.Start.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("unexpected shift %+v", s)
	}
	if _, ok := CurrentShift(r, nil, start.Add(-time.Minute)); ok {
		t.Fatal("rotation must not be active before start")
	}
	ovs := []Override{
		{ID: 1, Layer: 1, Username: "dave", Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
		{ID: 2, Layer: 1, Username: "erin", Start: now.Add(-time.Hour), End: now.Add(2 * time.Hour)},
		{ID: 3, Layer: 2, Username: "frank", Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
		{ID: 4, Layer: 1, Username: "gone", Start: now.Add(-3 * time.Hour), End: now},
	}
	s, _ = CurrentShift(r, ovs, now)
	if !s.Override || s.Username != "erin" {
		t.Fatalf("override not applied correctly: %+v", s)
	}
}

func TestComputeBudget(t *testing.T) {
	b := ComputeBudget(9995, 10000, 99.9, 1)
	if !b.HasData || *b.RemainingPct < 49.9 || *b.RemainingPct > 50.1 || !b.Healthy {
		t.Fatalf("expected ~50%% remaining, got %+v", *b.RemainingPct)
	}
	b = ComputeBudget(9980, 10000, 99.9, 1)
	if b.Healthy || *b.RemainingPct > -99 {
		t.Fatalf("expected overspent budget, got %v", *b.RemainingPct)
	}
	b = ComputeBudget(0, 0, 99.9, 1)
	if b.HasData || !b.Healthy {
		t.Fatal("no data must be reported as no data")
	}
	b = ComputeBudget(9995, 10000, 99.9, 0.25)
	if b.Healthy || *b.BurnRate < 1.9 {
		t.Fatalf("half budget used in a quarter window must burn too fast, burn=%v", *b.BurnRate)
	}
}

func TestFirstSample(t *testing.T) {
	v, ok, err := firstSample([]byte(`{"resultType":"vector","result":[{"metric":{},"value":[1,"0.9993"]}]}`))
	if err != nil || !ok || v != 0.9993 {
		t.Fatalf("vector parse: %v %v %v", v, ok, err)
	}
	_, ok, _ = firstSample([]byte(`{"resultType":"vector","result":[]}`))
	if ok {
		t.Fatal("empty vector must have no data")
	}
}
