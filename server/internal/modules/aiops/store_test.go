package aiops

import (
	"encoding/json"
	"testing"
)

func TestJSONText(t *testing.T) {
	cases := []struct {
		in, def, want string
	}{
		{"", "{}", "{}"},
		{"null", "[]", "[]"},
		{"  {\"a\":1} ", "{}", "{\"a\":1}"},
		{"{bad", "{}", "{}"},
		{"[1,2]", "{}", "[1,2]"},
	}
	for _, c := range cases {
		if got := jsonText([]byte(c.in), c.def); got != c.want {
			t.Errorf("jsonText(%q,%q)=%q want %q", c.in, c.def, got, c.want)
		}
	}
}

func TestJSONOf(t *testing.T) {
	if got := jsonOf(nil, "{}"); got != "{}" {
		t.Fatalf("nil: %q", got)
	}
	if got := jsonOf(json.RawMessage(`{"x":true}`), "{}"); got != `{"x":true}` {
		t.Fatalf("raw: %q", got)
	}
	if got := jsonOf(`[1]`, "{}"); got != `[1]` {
		t.Fatalf("json string: %q", got)
	}
	if got := jsonOf("plain text", "{}"); got != `"plain text"` {
		t.Fatalf("plain string: %q", got)
	}
	got := jsonOf(Conclusion{Summary: "内存不足", Confidence: "high"}, "{}")
	var c Conclusion
	if err := json.Unmarshal([]byte(got), &c); err != nil || c.Summary != "内存不足" || c.Confidence != "high" {
		t.Fatalf("struct: %q %v", got, err)
	}
	if got := jsonOf(map[string]any{"bad": make(chan int)}, "{}"); got != "{}" {
		t.Fatalf("unmarshalable: %q", got)
	}
}

func TestStepsRoundTrip(t *testing.T) {
	s, err := encodeSteps(nil)
	if err != nil || s != "[]" {
		t.Fatalf("nil steps: %q %v", s, err)
	}
	in := []Step{{ID: 7, PlanID: 3, Seq: 1, Phase: "main", Action: "k8s.restart",
		Params: json.RawMessage(`{"name":"web"}`), Risk: RiskChange, Executor: "ai", Status: StepPending}}
	enc, err := encodeSteps(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSteps([]byte(enc))
	if err != nil || len(out) != 1 || out[0].ID != 7 || out[0].Action != "k8s.restart" || string(out[0].Params) != `{"name":"web"}` {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if out, err := decodeSteps(nil); err != nil || out == nil || len(out) != 0 {
		t.Fatalf("empty decode: %v %v", out, err)
	}
	if _, err := decodeSteps([]byte("{")); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestPlanRiskAndDefaults(t *testing.T) {
	if r := planRisk(nil); r != RiskRead {
		t.Fatalf("empty risk %q", r)
	}
	if r := planRisk([]Step{{Risk: RiskRead}, {Risk: RiskHigh}, {Risk: RiskChange}}); r != RiskHigh {
		t.Fatalf("max risk %q", r)
	}
	s := Step{}
	fillStepDefaults(&s, 2)
	if s.Seq != 3 || s.Status != StepPending || s.Phase != "main" || s.Executor != ModeAI {
		t.Fatalf("defaults %+v", s)
	}
}

func TestNormPage(t *testing.T) {
	if l, o := normPage(0, 0); l != 20 || o != 0 {
		t.Fatalf("%d %d", l, o)
	}
	if l, o := normPage(3, 500); l != 200 || o != 400 {
		t.Fatalf("%d %d", l, o)
	}
}

func TestFindStepAndRunbookPlan(t *testing.T) {
	plans := []Plan{{ID: 1, Steps: []Step{{ID: 10}}}, {ID: 2, Steps: []Step{{ID: 11}, {ID: 12}}}}
	if p, i := findStep(plans, 12); p == nil || p.ID != 2 || i != 1 {
		t.Fatalf("find 12: %v %d", p, i)
	}
	if p, i := findStep(plans, 99); p != nil || i != -1 {
		t.Fatal("expected not found")
	}
	enc, err := encodeRunbookPlan(PlanDraft{Title: "重启"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal([]byte(enc), &back); err != nil || string(back["steps"]) != "[]" || string(back["verify"]) != "[]" {
		t.Fatalf("runbook plan: %s %v", enc, err)
	}
}
